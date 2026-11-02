package replay_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/inference"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/replay"
	"aos/internal/runlog"
	"aos/internal/runtime"
	"aos/internal/skills"
	"aos/internal/tools"
)

// step is one scripted model call: a reply action, or a failure.
type step struct {
	action map[string]any
	delay  time.Duration
	status int  // an HTTP error status instead of a reply
	block  bool // never answer
	tokens int  // prompt tokens reported
}

func tool(name string, args map[string]any) step {
	return step{action: map[string]any{"type": "tool", "tool_call": map[string]any{"name": name, "args": args}}}
}

func exit(reason string) step {
	return step{action: map[string]any{"type": "exit", "exit_status": map[string]any{"code": 0, "reason": reason}}}
}

func scriptServer(t *testing.T, script []step) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: only then does the server notice a client
		// that hangs up, which a blocked step waits for.
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		current := step{block: true}
		if n < len(script) {
			current = script[n]
		}
		n++
		mu.Unlock()
		if current.block {
			<-r.Context().Done()
			return
		}
		time.Sleep(current.delay)
		if current.status != 0 {
			w.WriteHeader(current.status)
			return
		}
		content, _ := json.Marshal(current.action)
		tokens := current.tokens
		if tokens == 0 {
			tokens = 100
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": string(content)}}},
			"usage":   map[string]any{"prompt_tokens": tokens, "completion_tokens": 7},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

type options struct {
	budget     capability.Budget
	compaction runtime.CompactionPolicy
}

func replayed(t *testing.T, stateDir string, unpinned bool) replay.Report {
	t.Helper()
	report, err := replay.Run(context.Background(), replay.Options{StateDir: stateDir, UnpinnedClock: unpinned})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return report
}

func calls(t *testing.T, stateDir string) []inference.CallRecord {
	t.Helper()
	records, err := runlog.Read[inference.CallRecord](runlog.Path(stateDir, runlog.CallsFile))
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// roundTripScript writes, runs a command, fails a read, meets a model
// error that is retried, and is compacted after a handoff, under an
// advisory time budget, so every kind of input and note is in the record.
func roundTripScript(workspace string) []step {
	return []step{
		tool("write_file", map[string]any{"path": filepath.Join(workspace, "a.txt"), "content": "hello"}),
		tool("run", map[string]any{"command": "cat a.txt && printf x > b.txt"}),
		tool("read_file", map[string]any{"path": filepath.Join(workspace, "missing.txt")}),
		{status: http.StatusBadRequest},
		tool("read_file", map[string]any{"path": filepath.Join(workspace, "a.txt")}),
		tool("read_file", map[string]any{"path": filepath.Join(workspace, "b.txt")}),
		tool("run", map[string]any{"command": "ls"}),
		exit("checked"),
		exit("checked"),
		exit("checked"),
	}
}

// The workspace path is only known once recorded; record with a
// placeholder script that refers to it through a fixed directory.
func recordRoundTrip(t *testing.T) string {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	return recordAt(t, root, context.Background(), func(ws string) []step { return roundTripScript(ws) },
		options{budget: capability.Budget{Seconds: 600}, compaction: runtime.CompactionPolicy{MaxMessages: 12, KeepMessages: 4, Handoff: true}})
}

// recordAt records with the workspace under root, so the script can name
// it.
func recordAt(t *testing.T, root string, ctx context.Context, script func(string) []step, opts options) string {
	t.Helper()
	stateDir, _ := recordRunIn(t, root, ctx, script, opts)
	return stateDir
}

func recordRunIn(t *testing.T, root string, ctx context.Context, script func(string) []step, opts options) (string, string) {
	t.Helper()
	workspace := filepath.Join(root, "ws")
	os.MkdirAll(workspace, 0o755)
	stateDir := filepath.Join(root, "state")
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	store, _ := artifact.New(filepath.Join(stateDir, "artifacts"))
	leases, _ := lease.New(filepath.Join(stateDir, "leases"), k)
	bus, _ := ipc.New(filepath.Join(stateDir, "bus"), k)
	toolRuntime := tools.NewRuntime()
	toolRuntime.Protected = []string{stateDir}
	toolRuntime.ScratchRoot = filepath.Join(root, "scratch")
	server := scriptServer(t, script(workspace))
	backend := inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: server.URL, Model: "scripted", Timeout: time.Minute,
		Sampling: inference.Sampling{Temperature: 0.6, MaxTokens: 512}})
	callLog, _ := runlog.Open(runlog.Path(stateDir, runlog.CallsFile))
	backend.SetCallLog(callLog)
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal([]byte(`{"name":"replayed","entry_agent":"solver","agents":[{"id":"solver","system_prompt":"Solve the task.","max_steps":12,
		"mounts":[{"name":"workspace","host_path":"`+workspace+`"}],
		"capabilities":{"tools":["write_file","read_file","run"],"read_paths":["`+workspace+`"],"write_paths":["`+workspace+`"],"unconfined_run":true}}]}`), &spec); err != nil {
		t.Fatal(err)
	}
	engine := agentflow.Engine{Kernel: k, Artifacts: store, Bus: bus, Leases: leases, Tools: toolRuntime,
		Factory:      agentflow.StaticBackendFactory{Completer: backend, Skills: skills.NewRegistry(nil)},
		Compaction:   opts.compaction,
		BackendRetry: runtime.BackendRetryPolicy{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond},
		Budget:       opts.budget,
		RunRecord:    map[string]string{"build_version": "test"},
	}
	_, _ = engine.Run(ctx, spec, agentflow.RunRequest{Goal: "write a.txt, then check it"})
	return stateDir, workspace
}

func TestReplayRoundTrip(t *testing.T) {
	stateDir := recordRoundTrip(t)
	events := eventTypes(t, stateDir)
	for _, want := range []string{"run.started", "backend.retry", "memory.compacted"} {
		if !strings.Contains(events, want) {
			t.Fatalf("the recording should cover %s: %s", want, events)
		}
	}
	report := replayed(t, stateDir, false)
	if !report.Identical() {
		t.Fatalf("divergence: %+v", report.Divergence)
	}
	starts := 0
	for _, record := range calls(t, stateDir) {
		if record.Phase == inference.PhaseStart {
			starts++
		}
	}
	if report.Calls != starts || report.Matched != starts || report.Tools != 6 || report.Events == 0 || report.Artifacts == 0 || report.InFlight {
		t.Fatalf("report = %+v (starts %d)", report, starts)
	}
	if report.RecordedBuild["build_version"] != "test" {
		t.Fatalf("recorded build = %v", report.RecordedBuild)
	}
}

func eventTypes(t *testing.T, stateDir string) string {
	t.Helper()
	var events []kernel.Event
	raw, _ := os.ReadFile(filepath.Join(stateDir, "kernel", "events.json"))
	json.Unmarshal(raw, &events)
	var types []string
	for _, event := range events {
		types = append(types, event.Type)
	}
	return strings.Join(types, ",")
}

// copyDir copies a state directory so a test can change the copy.
func copyDir(t *testing.T, from string) string {
	t.Helper()
	to := filepath.Join(t.TempDir(), "state")
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return to
}

// artifactOf is the first recorded artifact of type kind for tool.
func artifactOf(t *testing.T, stateDir, kind, toolName string) string {
	t.Helper()
	var index map[string]artifact.Artifact
	raw, _ := os.ReadFile(filepath.Join(stateDir, "artifacts", "index.json"))
	json.Unmarshal(raw, &index)
	best := ""
	for id, record := range index {
		if record.Type == kind && record.Metadata["tool"] == toolName && (best == "" || id < best) {
			best = id
		}
	}
	if best == "" {
		t.Fatalf("no %s for %s", kind, toolName)
	}
	return best
}

// rewrite replaces old with new in an artifact's payload. With rehash the
// index gets the new payload's sha256, so the record is a consistent one
// that differs; without it the payload no longer matches the index.
func rewrite(t *testing.T, stateDir, id, old, new string, rehash bool) {
	t.Helper()
	path := filepath.Join(stateDir, "artifacts", "payloads", id+".json")
	raw, _ := os.ReadFile(path)
	changed := []byte(strings.Replace(string(raw), old, new, 1))
	if string(changed) == string(raw) {
		t.Fatalf("%s has no %q", id, old)
	}
	if err := os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if !rehash {
		return
	}
	indexPath := filepath.Join(stateDir, "artifacts", "index.json")
	var index map[string]artifact.Artifact
	raw, _ = os.ReadFile(indexPath)
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	entry := index[id]
	sum := sha256.Sum256(changed)
	entry.SHA256 = hex.EncodeToString(sum[:])
	index[id] = entry
	raw, _ = json.Marshal(index)
	os.WriteFile(indexPath, raw, 0o644)
}

// A changed record shows up at the first call it affects: a tool result
// at the call after it, the system prompt at the first call.
func TestReplayFindsTheFirstDivergence(t *testing.T) {
	stateDir := recordRoundTrip(t)

	changed := copyDir(t, stateDir)
	runResult := artifactOf(t, changed, "tool_result", "run")
	rewrite(t, changed, runResult, "hello", "HELLO", true)
	report := replayed(t, changed, false)
	d := report.Divergence
	if d == nil || d.Kind != "request" || d.Step != 3 || report.Matched != 2 || !strings.Contains(d.Recorded, "hello") || !strings.Contains(d.Replayed, "HELLO") {
		t.Fatalf("a changed run result should diverge at step 3's call: %+v (matched %d)", d, report.Matched)
	}
	var startOfStep3 int
	for _, record := range calls(t, changed) {
		if record.Phase == inference.PhaseStart && record.Step == 3 {
			startOfStep3 = record.Seq
			break
		}
	}
	if d.Call != startOfStep3 {
		t.Fatalf("divergence at call %d, want %d", d.Call, startOfStep3)
	}

	prompt := copyDir(t, stateDir)
	specID := artifactOf(t, prompt, agentflow.RunSpecArtifact, "")
	rewrite(t, prompt, specID, "Solve the task.", "Solve the task carefully.", true)
	report = replayed(t, prompt, false)
	if d := report.Divergence; d == nil || d.Kind != "request" || d.Call != 1 || report.Matched != 0 || !strings.Contains(d.Detail, "message 1") {
		t.Fatalf("a changed prompt should diverge at the first call: %+v", d)
	}
}

// A payload that no longer matches the index's sha256 makes the record
// not replayable, naming the artifact, before any call is replayed; an
// index entry without a hash predates hashing and is trusted.
func TestReplayRefusesATamperedRecord(t *testing.T) {
	stateDir := recordRoundTrip(t)

	tampered := copyDir(t, stateDir)
	runResult := artifactOf(t, tampered, "tool_result", "run")
	rewrite(t, tampered, runResult, "hello", "HELLO", false)
	report, err := replay.Run(context.Background(), replay.Options{StateDir: tampered})
	if !errors.Is(err, replay.ErrNotReplayable) || !errors.Is(err, artifact.ErrArtifactTampered) || !strings.Contains(err.Error(), runResult) {
		t.Fatalf("a tampered payload should be named as not replayable: %v", err)
	}
	if report.Calls != 0 || report.Matched != 0 || report.Divergence != nil {
		t.Fatalf("nothing should be replayed: %+v", report)
	}

	legacy := copyDir(t, stateDir)
	rewrite(t, legacy, runResult, "hello", "HELLO", false)
	indexPath := filepath.Join(legacy, "artifacts", "index.json")
	var index map[string]artifact.Artifact
	raw, _ := os.ReadFile(indexPath)
	json.Unmarshal(raw, &index)
	entry := index[runResult]
	entry.SHA256 = ""
	index[runResult] = entry
	raw, _ = json.Marshal(index)
	os.WriteFile(indexPath, raw, 0o644)
	if d := replayed(t, legacy, false).Divergence; d == nil || d.Kind != "request" || !strings.Contains(d.Replayed, "HELLO") {
		t.Fatalf("a legacy entry is trusted and the change replays as a divergence: %+v", d)
	}
}

// A ledger cut short (a call's outcome missing) replays up to that call
// as in flight. Here the kernel's log goes on past the cut, unlike a real
// kill (TestReplayOfAKilledRun), and the replay says where.
func TestReplayOfALedgerCutShort(t *testing.T) {
	stateDir := copyDir(t, recordRoundTrip(t))
	path := runlog.Path(stateDir, runlog.CallsFile)
	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(strings.TrimSuffix(string(raw), "\n"), "\n")
	// Keep the first three calls whole and the fourth's start.
	os.WriteFile(path, []byte(strings.Join(lines[:7], "")), 0o644)
	report := replayed(t, stateDir, false)
	d := report.Divergence
	if !report.InFlight || report.Matched != 4 || d == nil || d.Kind != "kernel" || !strings.Contains(d.Recorded, "backend.retry") {
		t.Fatalf("report = %+v, divergence %+v", report, d)
	}
}

// Budget notes depend on the clock. Pinned to each step's recorded
// reading, the replay matches; on the wall clock it diverges at the first
// note whose seconds differ.
func TestReplayPinsTheClock(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), func(ws string) []step {
		return []step{
			tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "slow"}),
			{action: map[string]any{"type": "tool", "tool_call": map[string]any{"name": "read_file", "args": map[string]any{"path": filepath.Join(ws, "a.txt")}}}, delay: 1100 * time.Millisecond},
			exit("done"),
		}
	}, options{budget: capability.Budget{Seconds: 600}})
	if report := replayed(t, stateDir, false); !report.Identical() {
		t.Fatalf("pinned: %+v", report.Divergence)
	}
	report := replayed(t, stateDir, true)
	if d := report.Divergence; d == nil || d.Kind != "request" || d.Step != 3 || !strings.Contains(d.Recorded, "used=1s") || !strings.Contains(d.Replayed, "used=0s") {
		t.Fatalf("unpinned should diverge at step 3's time note: %+v", d)
	}
}

// A token budget caps each call's max_tokens (part of the request) and
// stops the run; the replay does the same.
func TestReplayOfATokenBudget(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), func(ws string) []step {
		write := tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "x"})
		write.tokens = 400
		return []step{write, write, write, write}
	}, options{budget: capability.Budget{Tokens: 1000}})
	if !strings.Contains(exitReason(t, stateDir), "token budget exceeded") {
		t.Fatalf("the recording should end on the token budget: %s", exitReason(t, stateDir))
	}
	if report := replayed(t, stateDir, false); !report.Identical() || report.Matched != 3 {
		t.Fatalf("report = %+v", report)
	}
}

func exitReason(t *testing.T, stateDir string) string {
	t.Helper()
	var processes map[string]kernel.Process
	raw, _ := os.ReadFile(filepath.Join(stateDir, "kernel", "processes.json"))
	json.Unmarshal(raw, &processes)
	if solver, ok := processes["pid-000002"]; ok && solver.ExitStatus != nil {
		return solver.ExitStatus.Reason
	}
	return ""
}

// A strict time budget that runs out during a model call: the replay stops
// the same call with the same cause, and the exits match.
func TestReplayOfAStrictStop(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), func(ws string) []step {
		return []step{tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "x"}), {block: true}}
	}, options{budget: capability.Budget{Seconds: 1, StrictTime: true}})
	if !strings.HasPrefix(exitReason(t, stateDir), "time budget exceeded") {
		t.Fatalf("the recording should end on the strict budget: %q", exitReason(t, stateDir))
	}
	if report := replayed(t, stateDir, false); !report.Identical() || report.Matched != 2 {
		t.Fatalf("report = %+v", report)
	}
}

// A run killed during a model call (copied while the call hangs, as a kill
// would leave it) replays up to that call, and its kernel and artifacts
// match as far as the record goes.
func TestReplayOfAKilledRun(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stateDir := filepath.Join(root, "state")
	done := make(chan struct{})
	go func() {
		defer close(done)
		recordAt(t, root, ctx, func(ws string) []step {
			return []step{
				tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "x"}),
				tool("read_file", map[string]any{"path": filepath.Join(ws, "a.txt")}),
				{block: true},
			}
		}, options{budget: capability.Budget{Seconds: 600}})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		records, _ := runlog.Read[inference.CallRecord](runlog.Path(stateDir, runlog.CallsFile))
		if open := inference.InFlight(records); len(records) == 5 && len(open) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the third call never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	killed := copyDir(t, stateDir)
	cancel()
	<-done
	report := replayed(t, killed, false)
	if !report.Identical() || !report.InFlight || report.Matched != 3 || report.Events == 0 {
		t.Fatalf("report = %+v", report)
	}
}

// What cannot be replayed is refused with a reason.
func TestReplayRefusesWhatItCannotReplay(t *testing.T) {
	stateDir := copyDir(t, recordRoundTrip(t))
	events := filepath.Join(stateDir, "kernel", "events.json")
	raw, _ := os.ReadFile(events)
	os.WriteFile(events, []byte(strings.ReplaceAll(string(raw), `"run.started"`, `"run.begun"`)), 0o644)
	if _, err := replay.Run(context.Background(), replay.Options{StateDir: stateDir}); err == nil || !strings.Contains(err.Error(), "run.started") {
		t.Fatalf("a run without run.started: %v", err)
	}
}
