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
	"regexp"
	"strconv"
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
	raw    string // the reply's text as sent, instead of action
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
	return scriptServerIn(t, script, false)
}

// scriptServerIn answers in the Anthropic Messages format when anthropic is
// set, with a thinking block before each reply, and in chat completions
// otherwise.
func scriptServerIn(t *testing.T, script []step, anthropic bool) *httptest.Server {
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
		text := "{\"action\":" + string(content) + "}"
		if current.raw != "" {
			text = current.raw
		}
		tokens := current.tokens
		if tokens == 0 {
			tokens = 100
		}
		if anthropic {
			json.NewEncoder(w).Encode(map[string]any{
				"type": "message", "model": "scripted", "stop_reason": "end_turn",
				"content": []map[string]any{{"type": "thinking", "thinking": "planning the step"}, {"type": "text", "text": text}},
				"usage":   map[string]any{"input_tokens": tokens - 60, "cache_read_input_tokens": 60, "output_tokens": 7},
			})
			return
		}
		reply := string(content)
		if current.raw != "" {
			reply = current.raw
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": tokens, "completion_tokens": 7},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

type options struct {
	budget     capability.Budget
	compaction runtime.CompactionPolicy
	// noIntents records as AOS did before tool.started intents.
	noIntents bool
	// noFencing records as AOS did before lease fencing.
	noFencing bool
	// noLeaseClock records run.started as AOS did before lease expiries
	// came from the kernel clock.
	noLeaseClock bool
	// noFraming records as AOS did before peer framing.
	noFraming bool
	// noChain records as AOS did before chain-owned delivery.
	noChain bool
	// noOutcomes records as AOS did before sender outcomes.
	noOutcomes bool
	// restart is the agent's restart policy, as JSON, and maxSteps its steps
	// if not the default.
	restart  string
	maxSteps int
	// mail also grants message_agent.
	mail bool
	// recall is the spec's recall field, as JSON.
	recall string
	// handles also grants publish_artifact and list_artifacts.
	handles bool
	// noBounds records as AOS did before the bus's bounds and handles.
	noBounds bool
	// noRedelegationClose records without the redelegation_close field.
	noRedelegationClose bool
	// noPublishSource records as AOS did before publish_artifact took a path.
	noPublishSource bool
	// noDurationArgs records as AOS did before tools.DurationArg.
	noDurationArgs bool
	// noReplyChecks and noReplyCap record as AOS did before the reply checks
	// and the reply cap.
	noReplyChecks, noReplyCap bool
	// noSpecContract records as AOS did before the spec contract.
	noSpecContract bool
	// noDelegateHandles records with the prompt from before handles at a delegation.
	noDelegateHandles bool
	// noDelegateAttach records as AOS did before delegate_attach.
	noDelegateAttach bool
	// delegator gives the agent an agent it may delegate to, and never does.
	delegator bool
	// anthropic records through the Anthropic Messages driver.
	anthropic bool
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
		{status: http.StatusServiceUnavailable},
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
	bus.NoSubscriptionEvents = opts.noChain
	bus.NoBounds = opts.noBounds
	toolRuntime := tools.NewRuntime()
	toolRuntime.Protected = []string{stateDir}
	toolRuntime.ScratchRoot = filepath.Join(root, "scratch")
	server := scriptServerIn(t, script(workspace), opts.anthropic)
	var backend inference.Backend = inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: server.URL, Model: "scripted", Timeout: time.Minute,
		Sampling: inference.Sampling{Temperature: 0.6, MaxTokens: 512}})
	if opts.anthropic {
		backend = inference.NewAnthropicBackend(inference.AnthropicConfig{BaseURL: server.URL, Model: "scripted", Timeout: time.Minute, MaxTokens: 512})
	}
	callLog, _ := runlog.Open(runlog.Path(stateDir, runlog.CallsFile))
	backend.SetCallLog(callLog)
	var spec agentflow.WorkflowSpec
	mailTool := ""
	if opts.mail {
		mailTool = `"message_agent",`
	}
	if opts.handles {
		mailTool += `"publish_artifact","list_artifacts",`
	}
	recall := ""
	if opts.recall != "" {
		recall = `"recall":` + opts.recall + `,`
	}
	maxSteps := 12
	if opts.maxSteps > 0 {
		maxSteps = opts.maxSteps
	}
	restart := ""
	if opts.restart != "" {
		restart = `"restart":` + opts.restart + `,`
	}
	allowed, helper := "", ""
	if opts.delegator {
		allowed, helper = `"allowed_agents":["helper"],`, `,{"id":"helper","system_prompt":"Help."}`
		if !opts.noSpecContract {
			// The spec contract wants the tool with the edge; specs from
			// before it often lacked it.
			mailTool += `"spawn_agent",`
		}
	}
	if err := json.Unmarshal([]byte(`{"name":"replayed","entry_agent":"solver",`+recall+`"agents":[{"id":"solver","system_prompt":"Solve the task.",`+allowed+restart+`"max_steps":`+strconv.Itoa(maxSteps)+`,
		"mounts":[{"name":"workspace","host_path":"`+workspace+`"}],
		"capabilities":{"tools":[`+mailTool+`"write_file","read_file","run","acquire_lease","release_lease"],"read_paths":["`+workspace+`"],"write_paths":["`+workspace+`"],"unconfined_run":true}}`+helper+`]}`), &spec); err != nil {
		t.Fatal(err)
	}
	engine := agentflow.Engine{Kernel: k, Artifacts: store, Bus: bus, Leases: leases, Tools: toolRuntime,
		Factory:       agentflow.StaticBackendFactory{Completer: backend, Skills: skills.NewRegistry(nil), NoPublishSource: opts.noPublishSource, NoDurationArgs: opts.noDurationArgs, NoDelegateHandles: opts.noDelegateHandles, NoDelegateAttach: opts.noDelegateAttach, NoPeerFraming: opts.noFraming, NoBusBounds: opts.noBounds, NoReplyChecks: opts.noReplyChecks},
		NoPeerFraming: opts.noFraming,
		NoBusBounds:   opts.noBounds,

		NoRedelegationClose: opts.noRedelegationClose,
		NoPublishSource:     opts.noPublishSource,
		NoDurationArgs:      opts.noDurationArgs,
		NoReplyChecks:       opts.noReplyChecks,
		NoReplyCap:          opts.noReplyCap,
		NoSpecContract:      opts.noSpecContract,
		NoDelegateHandles:   opts.noDelegateHandles,
		NoDelegateAttach:    opts.noDelegateAttach,
		NoChainDelivery:     opts.noChain,
		NoSenderOutcomes:    opts.noOutcomes,
		Compaction:          opts.compaction,
		BackendRetry:        runtime.BackendRetryPolicy{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond},
		Budget:              opts.budget,
		RunRecord:           map[string]string{"build_version": "test"},
		NoToolIntents:       opts.noIntents,
		NoLeaseFencing:      opts.noFencing,
		NoLeaseClock:        opts.noLeaseClock,
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

// A run through the Anthropic driver replays byte for byte: the replay
// builds the same driver from the ledger, and its bodies hash as recorded.
func TestReplayOfAnAnthropicRecord(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), func(ws string) []step { return roundTripScript(ws) },
		options{budget: capability.Budget{Seconds: 600}, compaction: runtime.CompactionPolicy{MaxMessages: 12, KeepMessages: 4, Handoff: true}, anthropic: true})
	records := calls(t, stateDir)
	starts, reasoned := 0, 0
	for _, record := range records {
		switch {
		case record.Phase == inference.PhaseStart:
			starts++
			if record.Params.API != inference.APIAnthropicMessages {
				t.Fatalf("call %d recorded as %q", record.Seq, record.Params.API)
			}
		case record.Reasoning == "planning the step":
			reasoned++
		}
	}
	if starts < 8 || reasoned == 0 || !strings.Contains(eventTypes(t, stateDir), "backend.retry") {
		t.Fatalf("the recording should hold the calls, their reasoning and a retry: %d starts, %d with reasoning", starts, reasoned)
	}
	report := replayed(t, stateDir, false)
	if !report.Identical() || report.Matched != starts {
		t.Fatalf("report %+v, divergence %+v", report, report.Divergence)
	}

	// A changed record shows where the replayed request first differs, the
	// system prompt counted as message 1, as the ledger holds it.
	changed := copyDir(t, stateDir)
	rewrite(t, changed, artifactOf(t, changed, "tool_result", "run"), "hello", "HELLO", true)
	report = replayed(t, changed, false)
	if d := report.Divergence; d == nil || d.Kind != "request" || d.Step != 3 || !strings.Contains(d.Recorded, "hello") || !strings.Contains(d.Replayed, "HELLO") {
		t.Fatalf("a changed run result should diverge at step 3's call: %+v", d)
	}
	prompt := copyDir(t, stateDir)
	rewrite(t, prompt, artifactOf(t, prompt, agentflow.RunSpecArtifact, ""), "Solve the task.", "Solve the task carefully.", true)
	report = replayed(t, prompt, false)
	if d := report.Divergence; d == nil || d.Call != 1 || !strings.Contains(d.Detail, "message 1 (system)") {
		t.Fatalf("a changed prompt should diverge at the first call's system message: %+v", d)
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

// legacyRunStarted rewrites the record's run.started in the format before
// settings were recorded as they take effect: no budget_time, *_source or
// max_steps_*, and a default setting recorded as zero. edit, if set,
// changes the result further.
func legacyRunStarted(t *testing.T, stateDir string, edit func(map[string]string)) {
	t.Helper()
	path := filepath.Join(stateDir, "kernel", "events.json")
	var events []kernel.Event
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	for i := range events {
		if events[i].Type != agentflow.RunStartedEvent {
			continue
		}
		data := events[i].Data
		if data["compaction_source"] == "default" {
			for _, name := range []string{"compact_max_messages", "compact_max_chars", "compact_keep", "compact_target_chars"} {
				data[name] = "0"
			}
			data["compact_handoff"] = "false"
		}
		if data["retry_source"] == "default" {
			data["retry_attempts"], data["retry_base"], data["retry_max"] = "0", "0s", "0s"
		}
		for name := range data {
			if name == "budget_time" || strings.HasSuffix(name, "_source") || strings.HasPrefix(name, "max_steps_") {
				delete(data, name)
			}
		}
		if edit != nil {
			edit(data)
		}
	}
	raw, _ = json.Marshal(events)
	os.WriteFile(path, raw, 0o644)
}

// A run.started in the format before settings were recorded as they take
// effect is compared by meaning: the replay records the current format,
// with the same settings. Its other fields are still compared exactly.
func TestReplayOfALegacyRunStarted(t *testing.T) {
	stateDir := recordRoundTrip(t)

	legacy := copyDir(t, stateDir)
	legacyRunStarted(t, legacy, nil)
	report := replayed(t, legacy, false)
	if !report.Identical() || !report.LegacyRunStarted {
		t.Fatalf("a legacy run.started should replay identically, compared by meaning: %+v", report.Divergence)
	}
	if current := replayed(t, stateDir, false); current.LegacyRunStarted {
		t.Fatal("a current run.started is compared byte for byte")
	}

	changed := copyDir(t, stateDir)
	legacyRunStarted(t, changed, func(data map[string]string) { data["run_spec_sha256"] = strings.Repeat("0", 64) })
	if d := replayed(t, changed, false).Divergence; d == nil || d.Kind != "kernel" || !strings.Contains(d.Detail, "differs") || !strings.Contains(d.Recorded, `"run.started"`) {
		t.Fatalf("a legacy run.started with a different field should diverge there: %+v", d)
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
// stops the run; the replay does the same. Before the reply cap the cap was
// the budget left, and the run stopped once that was spent; since, it is
// the budget left less the prompt, and the call with no room for a reply is
// refused before it is sent.
func TestReplayOfATokenBudget(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), func(ws string) []step {
		write := tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "x"})
		write.tokens = 400
		return []step{write, write, write, write}
	}, options{budget: capability.Budget{Tokens: 1000}, noReplyCap: true})
	if !strings.Contains(exitReason(t, stateDir), "token budget exceeded: 1221 of 1000") {
		t.Fatalf("the recording should end on the token budget: %s", exitReason(t, stateDir))
	}
	if report := replayed(t, stateDir, false); !report.Identical() || report.Matched != 3 || !report.LegacyReplyCap {
		t.Fatalf("report = %+v", report)
	}

	root, _ = filepath.EvalSymlinks(t.TempDir())
	stateDir = recordAt(t, root, context.Background(), func(ws string) []step {
		write := tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "x"})
		write.tokens = 6000
		return []step{write, write, write, write}
	}, options{budget: capability.Budget{Tokens: 20000}})
	if reason := exitReason(t, stateDir); !strings.Contains(reason, "no tokens left for a reply: the prompt is about 6") || !strings.Contains(reason, "and 1979 are left") {
		t.Fatalf("the recording should end on a refused call: %s", reason)
	}
	var caps []int
	for _, record := range calls(t, stateDir) {
		if record.Phase == inference.PhaseStart {
			caps = append(caps, record.Params.MaxTokens)
		}
	}
	if len(caps) != 3 || caps[1] >= 20000-6007-6000 || caps[2] >= 20000-2*6007-6000 {
		t.Fatalf("max_tokens sent %v: each should leave room for the prompt", caps)
	}
	if report := replayed(t, stateDir, false); !report.Identical() || report.Matched != 3 || report.LegacyReplyCap {
		t.Fatalf("report = %+v", report)
	}
}

// A failure the driver says will not pass (a 404) ends the step at once,
// retryable with a checkpoint, and its outcome records the status and the
// decision; the replay rebuilds the error and stops the same way. A 503 is
// retried, as before.
func TestReplayOfAFailureNotRetried(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), func(ws string) []step {
		return []step{tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "x"}), {status: http.StatusServiceUnavailable}, {status: http.StatusNotFound}}
	}, options{})
	if reason := exitReason(t, stateDir); reason != "model endpoint refused the call, not retried: vllm request failed: 404 Not Found" {
		t.Fatalf("exit reason %q", reason)
	}
	var outcomes []inference.CallRecord
	for _, record := range calls(t, stateDir) {
		if record.Phase == inference.PhaseOutcome && record.Error != "" {
			outcomes = append(outcomes, record)
		}
	}
	if len(outcomes) != 2 || outcomes[0].ErrorStatus != 503 || outcomes[0].ErrorFinal || outcomes[1].ErrorStatus != 404 || !outcomes[1].ErrorFinal {
		t.Fatalf("failed outcomes %+v", outcomes)
	}
	if events := eventTypes(t, stateDir); strings.Count(events, "backend.retry") != 1 {
		t.Fatalf("one retry (the 503) expected: %s", events)
	}
	if report := replayed(t, stateDir, false); !report.Identical() {
		t.Fatalf("divergence: %+v", report.Divergence)
	}
}

// A reply with a real action and an example after it: before the reply
// checks the step took the example, and a replay of that record does too;
// with them it asks for the repair.
func TestReplayOfAReplyBeforeTheChecks(t *testing.T) {
	const twoActions = `{"action":{"type":"exit","exit_status":{"code":0,"reason":"done"}}} For example: {"action":{"type":"exit","exit_status":{"code":0,"reason":"example"}}}`
	for _, noChecks := range []bool{true, false} {
		root, _ := filepath.EvalSymlinks(t.TempDir())
		stateDir := recordAt(t, root, context.Background(), func(ws string) []step {
			return []step{{raw: twoActions}, exit("repaired")}
		}, options{noReplyChecks: noChecks})
		want := map[bool]string{true: "example", false: "repaired"}[noChecks]
		if reason := exitReason(t, stateDir); !strings.Contains(reason, want) {
			t.Fatalf("noChecks %t: exit %q, want %q", noChecks, reason, want)
		}
		if report := replayed(t, stateDir, false); !report.Identical() || report.LegacyReplyChecks != noChecks {
			t.Fatalf("noChecks %t: report %+v", noChecks, report)
		}
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

// A record made before tool.started intents replays as identical: the
// replay records none either, and says so. A current record has them and
// is not flagged.
func TestReplayOfARecordWithoutIntents(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	legacy := recordAt(t, root, context.Background(), func(ws string) []step { return roundTripScript(ws) },
		options{budget: capability.Budget{Seconds: 600}, compaction: runtime.CompactionPolicy{MaxMessages: 12, KeepMessages: 4, Handoff: true}, noIntents: true})
	if events := eventTypes(t, legacy); strings.Contains(events, runtime.ToolStartedEvent) {
		t.Fatalf("the legacy recording has intents: %s", events)
	}
	report := replayed(t, legacy, false)
	if !report.Identical() || !report.LegacyToolIntents || report.Tools != 6 {
		t.Fatalf("legacy report = %+v", report)
	}

	current := recordRoundTrip(t)
	if events := eventTypes(t, current); strings.Count(events, runtime.ToolStartedEvent) != 3 {
		t.Fatalf("the current recording should have an intent per world tool call (1 write, 2 runs): %s", events)
	}
	report = replayed(t, current, false)
	if !report.Identical() || report.LegacyToolIntents {
		t.Fatalf("current report = %+v", report)
	}
	// An intent removed from a current record is a divergence: the replay
	// records it.
	stateDir := copyDir(t, current)
	path := filepath.Join(stateDir, "kernel", "events.json")
	var events []kernel.Event
	raw, _ := os.ReadFile(path)
	json.Unmarshal(raw, &events)
	for i := range events {
		if events[i].Type == runtime.ToolStartedEvent {
			events[i].Type = "tool.noted"
			break
		}
	}
	raw, _ = json.Marshal(events)
	os.WriteFile(path, raw, 0o644)
	if report := replayed(t, stateDir, false); report.Identical() || report.Divergence.Kind != "kernel" {
		t.Fatalf("a changed intent should diverge: %+v", report)
	}
}

// A command cut off by a strict budget replays as identical, whether the
// record has its tool_cancelled outcome (current) or only the final
// checkpoint (before intents).
func TestReplayOfACommandCutOff(t *testing.T) {
	for _, noIntents := range []bool{false, true} {
		root, _ := filepath.EvalSymlinks(t.TempDir())
		stateDir := recordAt(t, root, context.Background(), func(ws string) []step {
			return []step{tool("run", map[string]any{"command": "echo once >> " + filepath.Join(ws, "effects") + "; sleep 30", "timeout": "60s"})}
		}, options{budget: capability.Budget{Seconds: 1, StrictTime: true}, noIntents: noIntents})
		if !strings.HasPrefix(exitReason(t, stateDir), "time budget exceeded") {
			t.Fatalf("the recording should end on the strict budget: %q", exitReason(t, stateDir))
		}
		cancelled := strings.Contains(artifactTypes(t, stateDir), runtime.ToolCancelledType)
		if cancelled == noIntents {
			t.Fatalf("noIntents=%v: tool_cancelled recorded: %v", noIntents, cancelled)
		}
		if report := replayed(t, stateDir, false); !report.Identical() || report.Matched != 1 || report.LegacyToolIntents != noIntents {
			t.Fatalf("noIntents=%v: report = %+v", noIntents, report)
		}
	}
}

func artifactTypes(t *testing.T, stateDir string) string {
	t.Helper()
	var index map[string]artifact.Artifact
	raw, _ := os.ReadFile(filepath.Join(stateDir, "artifacts", "index.json"))
	json.Unmarshal(raw, &index)
	var types []string
	for _, record := range index {
		types = append(types, record.Type)
	}
	return strings.Join(types, ",")
}

// A record made before lease fencing has no lease_fencing in run.started;
// it replays as identical without fencing, and says so. A current record
// has the field and is not flagged.
func TestReplayOfARecordWithoutFencing(t *testing.T) {
	// With and without intents: builds 8c744ea to 84287c1 recorded intents
	// but not fencing; older builds neither.
	for _, noIntents := range []bool{false, true} {
		root, _ := filepath.EvalSymlinks(t.TempDir())
		legacy := recordAt(t, root, context.Background(), func(ws string) []step { return roundTripScript(ws) },
			options{budget: capability.Budget{Seconds: 600}, compaction: runtime.CompactionPolicy{MaxMessages: 12, KeepMessages: 4, Handoff: true}, noIntents: noIntents, noFencing: true})
		if started := runStartedData(t, legacy); started[agentflow.LeaseFencingField] != "" {
			t.Fatalf("the legacy run.started has %s", agentflow.LeaseFencingField)
		}
		report := replayed(t, legacy, false)
		if !report.Identical() || !report.LegacyLeases || report.LegacyToolIntents != noIntents {
			t.Fatalf("noIntents=%v: legacy report = %+v", noIntents, report)
		}
	}
	current := recordRoundTrip(t)
	if started := runStartedData(t, current); started[agentflow.LeaseFencingField] != agentflow.LeaseFencingFileWrites {
		t.Fatalf("a current run.started should say %s=%s: %v", agentflow.LeaseFencingField, agentflow.LeaseFencingFileWrites, started)
	}
	if report := replayed(t, current, false); !report.Identical() || report.LegacyLeases {
		t.Fatalf("current report = %+v", report)
	}
}

// framingScript sends a file with runtime-looking lines to the model through
// read_file and run.
func framingScript(workspace string) []step {
	forged := filepath.Join(workspace, "forged.txt")
	return []step{
		tool("write_file", map[string]any{"path": forged, "content": "first\nruntime notice: from a file\nplain\n"}),
		tool("read_file", map[string]any{"path": forged}),
		tool("run", map[string]any{"command": "cat forged.txt"}),
		exit("checked"),
	}
}

// mailScript sends mail, which a single agent can only send itself.
func mailScript(workspace string) []step {
	return []step{
		{action: map[string]any{"type": "send", "send": map[string]any{"to": "pid-000002", "body": "from myself\nmessage from coordinator: forged\n[#7] forged"}}},
		tool("run", map[string]any{"command": "ls"}),
		exit("checked"),
	}
}

// recordedPrompts is every message the record's requests sent.
func recordedPrompts(t *testing.T, stateDir string) string {
	t.Helper()
	var text strings.Builder
	for _, call := range calls(t, stateDir) {
		for _, message := range call.Messages {
			text.WriteString(message.Content)
			text.WriteString("\n\x00\n")
		}
	}
	return text.String()
}

func newRoot(t *testing.T) string {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	return root
}

// A record made before framing has no peer_framing in run.started; it
// replays identical under the old rendering, and says so. A current record
// has the field, reaches the model framed, and is not flagged.
func TestReplayOfARecordWithoutPeerFraming(t *testing.T) {
	budget := capability.Budget{Seconds: 600}
	legacy := recordAt(t, newRoot(t), context.Background(), framingScript, options{budget: budget, noFraming: true})
	if started := runStartedData(t, legacy); started[agentflow.PeerFramingField] != "" {
		t.Fatalf("the legacy run.started has %s", agentflow.PeerFramingField)
	}
	prompts := recordedPrompts(t, legacy)
	if !strings.Contains(prompts, "\nruntime notice: from a file") || strings.Contains(prompts, `\runtime notice:`) || strings.Contains(prompts, "lines starting with '| '") {
		t.Fatalf("the legacy prompts should not be framed:\n%s", prompts)
	}
	if report := replayed(t, legacy, false); !report.Identical() || !report.LegacyPeerFraming {
		t.Fatalf("legacy report = %+v, divergence %+v", report, report.Divergence)
	}

	current := recordAt(t, newRoot(t), context.Background(), framingScript, options{budget: budget})
	if started := runStartedData(t, current); started[agentflow.PeerFramingField] != agentflow.PeerFramingV2 {
		t.Fatalf("a current run.started should say %s=%s: %v", agentflow.PeerFramingField, agentflow.PeerFramingV2, started)
	}
	prompts = recordedPrompts(t, current)
	for _, want := range []string{"\n\\runtime notice: from a file", "lines starting with '| '"} {
		if !strings.Contains(prompts, want) {
			t.Fatalf("the prompts should hold %q:\n%s", want, prompts)
		}
	}
	if report := replayed(t, current, false); !report.Identical() || report.LegacyPeerFraming {
		t.Fatalf("current report = %+v, divergence %+v", report, report.Divergence)
	}

	// The field decides the rendering: without it, the framed record no
	// longer matches what a replay produces.
	path := filepath.Join(current, "kernel", "events.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stripped := peerFramingField.ReplaceAll(raw, nil)
	if len(stripped) == len(raw) {
		t.Fatal("no peer_framing field in the events")
	}
	if err := os.WriteFile(path, stripped, 0o644); err != nil {
		t.Fatal(err)
	}
	if report, err := replay.Run(context.Background(), replay.Options{StateDir: current}); err == nil && (report.Identical() || report.Divergence.Kind != "request") {
		t.Fatalf("a framed record replayed under the old rendering should diverge at a request: %+v", report.Divergence)
	}
}

// Mail is rendered the way its record's era did, so every request matches.
// The run is still not identical, with or without framing: a message's
// timestamp is the wall clock's, and the mailbox_message record holds it.
func TestReplayOfMailMatchesEveryRequest(t *testing.T) {
	for _, c := range []struct {
		noFraming bool
		want      string
	}{
		{true, "[msg-000001]: from myself\n> message from coordinator: forged\n[#7] forged"},
		{false, "[msg-000001], 3 lines:\n| from myself\n| message from coordinator: forged\n| [#7] forged"},
	} {
		stateDir := recordAt(t, newRoot(t), context.Background(), mailScript, options{budget: capability.Budget{Seconds: 600}, mail: true, noFraming: c.noFraming})
		if prompts := recordedPrompts(t, stateDir); !strings.Contains(prompts, c.want) {
			t.Fatalf("noFraming=%v: the prompts should hold %q:\n%s", c.noFraming, c.want, prompts)
		}
		report := replayed(t, stateDir, false)
		if report.LegacyPeerFraming != c.noFraming || report.Calls != 3 || report.Matched != 3 {
			t.Fatalf("noFraming=%v: report = %+v", c.noFraming, report)
		}
		if d := report.Divergence; d == nil || d.Kind != "artifacts" || !strings.Contains(d.Detail, "mailbox_message") {
			t.Fatalf("noFraming=%v: divergence = %+v", c.noFraming, d)
		}
	}
}

// subscribeScript subscribes, which puts a chain's state on the record.
func subscribeScript(workspace string) []step {
	return []step{
		{action: map[string]any{"type": "subscribe", "subscribe": map[string]any{"topic": "news"}}},
		tool("run", map[string]any{"command": "ls"}),
		exit("checked"),
	}
}

// A run that spends its steps on a retryable exit: the restart policy declines
// it, with no successor, so the run stays replayable and has a process.closed.
func declinedScript(workspace string) []step {
	return []step{
		tool("run", map[string]any{"command": "ls"}),
		{action: map[string]any{"type": "exit", "exit_status": map[string]any{"code": 1, "reason": "fails", "retryable": true}}},
	}
}

// Chain-owned delivery adds events, and a replay's kernel events have to match
// the record's one for one. Without run.started's chain_delivery, a replay
// records none of them; with it, all of them.
func TestReplayOfARecordWithoutChainDelivery(t *testing.T) {
	budget := capability.Budget{Seconds: 600}
	declined := options{budget: budget, mail: true, restart: `{"max":3,"backoff":"0s"}`, maxSteps: 2}
	for name, c := range map[string]struct {
		script func(string) []step
		opts   options
		want   string
	}{
		"subscribe": {subscribeScript, options{budget: budget, mail: true}, "ipc.subscribed"},
		"declined":  {declinedScript, declined, "process.closed"},
	} {
		t.Run(name, func(t *testing.T) {
			current := recordAt(t, newRoot(t), context.Background(), c.script, c.opts)
			if started := runStartedData(t, current); started[agentflow.ChainDeliveryField] != agentflow.ChainDeliveryV1 {
				t.Fatalf("run.started = %v", started)
			}
			if events := eventTypes(t, current); !strings.Contains(events, c.want) {
				t.Fatalf("the record should hold %s: %s", c.want, events)
			}
			if report := replayed(t, current, false); !report.Identical() || report.LegacyChainDelivery {
				t.Fatalf("current report = %+v, divergence %+v", report, report.Divergence)
			}

			opts := c.opts
			opts.noChain = true
			legacy := recordAt(t, newRoot(t), context.Background(), c.script, opts)
			if started := runStartedData(t, legacy); started[agentflow.ChainDeliveryField] != "" {
				t.Fatalf("the legacy run.started = %v", started)
			}
			if events := eventTypes(t, legacy); strings.Contains(events, c.want) || strings.Contains(events, "ipc.unsubscribed") {
				t.Fatalf("the legacy record should not hold them: %s", events)
			}
			if report := replayed(t, legacy, false); !report.Identical() || !report.LegacyChainDelivery {
				t.Fatalf("legacy report = %+v, divergence %+v", report, report.Divergence)
			}

			// The field decides: without it, a record that has the events
			// no longer matches what a replay produces.
			path := filepath.Join(current, "kernel", "events.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			stripped := chainDeliveryField.ReplaceAll(raw, nil)
			if len(stripped) == len(raw) {
				t.Fatal("no chain_delivery field in the events")
			}
			if err := os.WriteFile(path, stripped, 0o644); err != nil {
				t.Fatal(err)
			}
			if report, err := replay.Run(context.Background(), replay.Options{StateDir: current}); err == nil && report.Identical() {
				t.Fatalf("a record with the events replayed without them should diverge: %+v", report)
			}
		})
	}
}

// A run with no mail, no subscription and no restart declined adds no event.
func TestChainDeliveryAddsNoEventToAPlainRun(t *testing.T) {
	budget := capability.Budget{Seconds: 600}
	current := eventTypes(t, recordAt(t, newRoot(t), context.Background(), framingScript, options{budget: budget}))
	legacy := eventTypes(t, recordAt(t, newRoot(t), context.Background(), framingScript, options{budget: budget, noChain: true}))
	if current != legacy {
		t.Fatalf("the events differ:\n%s\n%s", current, legacy)
	}
}

// unreadScript sends itself mail with its last step, and the step limit stops
// it before it reads that. The restart policy declines, which ends the chain.
func unreadScript(workspace string) []step {
	return []step{
		tool("run", map[string]any{"command": "ls"}),
		{action: map[string]any{"type": "send", "send": map[string]any{"to": "pid-000002", "body": "to myself"}}},
	}
}

// A run with no mail adds nothing: its events and its prompts are what a build
// before sender outcomes records, and it replays identically either way.
func TestSenderOutcomesAddNothingToAPlainRun(t *testing.T) {
	budget := capability.Budget{Seconds: 600}
	current := recordAt(t, newRoot(t), context.Background(), framingScript, options{budget: budget})
	legacy := recordAt(t, newRoot(t), context.Background(), framingScript, options{budget: budget, noOutcomes: true})
	if a, b := eventTypes(t, current), eventTypes(t, legacy); a != b {
		t.Fatalf("the events differ:\n%s\n%s", a, b)
	}
	// Only the root directory and the wall clock's budget line can differ
	// between two runs.
	clock := regexp.MustCompile(`budget time=\S+ used=\S+ left=\S+`)
	normal := func(stateDir string) string {
		return clock.ReplaceAllString(strings.ReplaceAll(recordedPrompts(t, stateDir), filepath.Dir(stateDir), "ROOT"), "")
	}
	if a, b := normal(current), normal(legacy); a != b {
		t.Fatalf("the prompts differ:\n%s\n%s", a, b)
	}
	if started := runStartedData(t, current); started[agentflow.SenderOutcomesField] != agentflow.SenderOutcomesV1 {
		t.Fatalf("run.started = %v", started)
	}
	if started := runStartedData(t, legacy); started[agentflow.SenderOutcomesField] != "" {
		t.Fatalf("the legacy run.started = %v", started)
	}
	if report := replayed(t, current, false); !report.Identical() || report.LegacySenderOutcomes {
		t.Fatalf("current report = %+v, divergence %+v", report, report.Divergence)
	}
	if report := replayed(t, legacy, false); !report.Identical() || !report.LegacySenderOutcomes {
		t.Fatalf("legacy report = %+v, divergence %+v", report, report.Divergence)
	}
}

// With mail, the new events exist, so a record without the field replays without
// them. Every request still matches, under both. Mail never replays identically:
// a message's timestamp is the wall clock's.
func TestReplayOfARecordWithoutSenderOutcomes(t *testing.T) {
	unread := options{budget: capability.Budget{Seconds: 600}, mail: true, restart: `{"max":3,"backoff":"0s"}`, maxSteps: 2}
	current := recordAt(t, newRoot(t), context.Background(), unreadScript, unread)
	if events := eventTypes(t, current); !strings.Contains(events, "ipc.dead_letter") {
		t.Fatalf("the record should hold a dead letter: %s", events)
	}
	if report := replayed(t, current, false); report.LegacySenderOutcomes || report.Calls == 0 || report.Matched != report.Calls {
		t.Fatalf("current report = %+v", report)
	}
	unread.noOutcomes = true
	legacy := recordAt(t, newRoot(t), context.Background(), unreadScript, unread)
	if events := eventTypes(t, legacy); strings.Contains(events, "ipc.dead_letter") {
		t.Fatalf("the legacy record should not: %s", events)
	}
	if report := replayed(t, legacy, false); !report.LegacySenderOutcomes || report.Calls == 0 || report.Matched != report.Calls {
		t.Fatalf("legacy report = %+v", report)
	}
}

// A record that says framed is framed: a spec naming no_prompt_framing does
// not turn the escape off behind peer_framing.
func TestRecordOfPeerFramingTellsTheTruth(t *testing.T) {
	stateDir := recordAt(t, newRoot(t), context.Background(), framingScript, options{budget: capability.Budget{Seconds: 600}, recall: `{"no_prompt_framing":true}`})
	if started := runStartedData(t, stateDir); started[agentflow.PeerFramingField] != agentflow.PeerFramingV2 {
		t.Fatalf("run.started = %v", started)
	}
	prompts := recordedPrompts(t, stateDir)
	for _, want := range []string{"\n\\runtime notice: from a file", "lines starting with '| '"} {
		if !strings.Contains(prompts, want) {
			t.Fatalf("the record says %s=%s, but the prompts lack %q:\n%s", agentflow.PeerFramingField, agentflow.PeerFramingV2, want, prompts)
		}
	}
	if report := replayed(t, stateDir, false); !report.Identical() {
		t.Fatalf("report = %+v, divergence %+v", report, report.Divergence)
	}
}

var chainDeliveryField = regexp.MustCompile(`\s*"chain_delivery":\s*"v1",?`)

var peerFramingField = regexp.MustCompile(`\s*"peer_framing":\s*"v2",?`)

func runStartedData(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	var events []kernel.Event
	raw, _ := os.ReadFile(filepath.Join(stateDir, "kernel", "events.json"))
	json.Unmarshal(raw, &events)
	for _, event := range events {
		if event.Type == agentflow.RunStartedEvent {
			return event.Data
		}
	}
	t.Fatal("no run.started")
	return nil
}

// leasedScript acquires a lease, writes under it, lets it expire, takes it
// over again (lease.taken_over, reason expired), writes, and releases it.
func leasedScript(ws string) []step {
	again := tool("acquire_lease", map[string]any{"resource": "a.txt", "ttl": "1h"})
	again.delay = 400 * time.Millisecond
	return []step{
		tool("acquire_lease", map[string]any{"resource": "a.txt", "ttl": "200ms"}),
		tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "first"}),
		again,
		tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "second"}),
		tool("release_lease", map[string]any{"resource": "a.txt"}),
		exit("done"),
	}
}

// A run that uses leases replays as identical: its lease times come from
// the kernel clock, which the replay reproduces, so the expiries, the
// takeover after expiry and the acquire_lease results come out the same.
func TestReplayOfALeasedRun(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), leasedScript, options{budget: capability.Budget{Seconds: 600}})
	events := eventTypes(t, stateDir)
	for _, want := range []string{"lease.acquired", "lease.taken_over", "lease.released"} {
		if !strings.Contains(events, want) {
			t.Fatalf("the recording should have %s: %s", want, events)
		}
	}
	var recorded []kernel.Event
	raw, _ := os.ReadFile(filepath.Join(stateDir, "kernel", "events.json"))
	json.Unmarshal(raw, &recorded)
	for _, event := range recorded {
		if event.Type == "lease.taken_over" && event.Data["reason"] != "expired" {
			t.Fatalf("takeover = %+v", event.Data)
		}
		if event.Type == "lease.acquired" {
			expires, _ := time.Parse(time.RFC3339Nano, event.Data["expires_at"])
			if got := expires.Sub(event.Timestamp); got != 200*time.Millisecond {
				t.Fatalf("expires_at should be the event's own time plus the ttl, got +%s", got)
			}
		}
	}
	if started := runStartedData(t, stateDir); started[agentflow.LeaseClockField] != agentflow.LeaseClockKernel {
		t.Fatalf("run.started = %v", started)
	}
	report := replayed(t, stateDir, false)
	if !report.Identical() || report.Tools != 2 {
		t.Fatalf("report = %+v", report)
	}
}

// A record that granted leases before lease times came from the kernel
// clock is refused, naming the first grant: its wall-clock expiries
// cannot be reproduced. One without leases still replays.
func TestReplayRefusesLeasesOnTheWallClock(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := recordAt(t, root, context.Background(), leasedScript, options{budget: capability.Budget{Seconds: 600}, noLeaseClock: true})
	_, err := replay.Run(context.Background(), replay.Options{StateDir: stateDir})
	if !errors.Is(err, replay.ErrNotReplayable) || !strings.Contains(err.Error(), "lease.acquired a.txt") || !strings.Contains(err.Error(), "kernel clock") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	root, _ = filepath.EvalSymlinks(t.TempDir())
	plain := recordAt(t, root, context.Background(), func(ws string) []step {
		return []step{tool("write_file", map[string]any{"path": filepath.Join(ws, "a.txt"), "content": "x"}), exit("done")}
	}, options{budget: capability.Budget{Seconds: 600}, noLeaseClock: true})
	if report := replayed(t, plain, false); !report.Identical() {
		t.Fatalf("a record without leases: %+v", report)
	}
}

// A record with a restart has a second agent process, which replay does
// not handle; it is refused, naming the restart.
func TestReplayRefusesARestartedRun(t *testing.T) {
	stateDir := copyDir(t, recordRoundTrip(t))
	path := filepath.Join(stateDir, "kernel", "events.json")
	var events []kernel.Event
	raw, _ := os.ReadFile(path)
	json.Unmarshal(raw, &events)
	last := events[len(events)-1]
	events = append(events, kernel.Event{ID: "evt-999999", ProcessID: "pid-000003", Type: agentflow.RestartEvent, Timestamp: last.Timestamp, Data: map[string]string{"predecessor": "pid-000002"}})
	raw, _ = json.Marshal(events)
	os.WriteFile(path, raw, 0o644)
	_, err := replay.Run(context.Background(), replay.Options{StateDir: stateDir})
	if !errors.Is(err, replay.ErrNotReplayable) || !strings.Contains(err.Error(), "restarted an agent") || !strings.Contains(err.Error(), "pid-000002") {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

func oversizeScript(string) []step {
	big := strings.Repeat("x", ipc.MaxBodyBytes+1)
	return []step{
		{action: map[string]any{"type": "send", "send": map[string]any{"to": "pid-000002", "body": big}}},
		tool("message_agent", map[string]any{"to": "pid-000002", "body": big}),
		tool("run", map[string]any{"command": "ls"}),
		exit("checked"),
	}
}

func handlesScript(string) []step {
	return []step{
		tool("publish_artifact", map[string]any{"type": "report", "body": strings.Repeat("é", 40*1024)}),
		tool("list_artifacts", map[string]any{}),
		tool("read_artifact", map[string]any{"id": "art-000999"}),
		{action: map[string]any{"type": "send", "send": map[string]any{"to": "pid-000002", "body": strings.Repeat("x", ipc.MaxBodyBytes+1)}}},
		tool("message_agent", map[string]any{"to": "pid-000002", "body": "attached", "artifacts": "art-000999"}),
		exit("checked"),
	}
}

// A record from before the bounds, with mail over the cap, replays as it ran; so
// does one made with them, whose refusals and reads are on the record.
func TestReplayOfARecordWithoutBusBounds(t *testing.T) {
	opts := options{budget: capability.Budget{Seconds: 600}, mail: true, noBounds: true, maxSteps: 8}
	legacy := recordAt(t, newRoot(t), context.Background(), oversizeScript, opts)
	if runStartedData(t, legacy)[agentflow.BusBoundsField] != "" {
		t.Fatal("the legacy record should have no bus_bounds")
	}
	if events := eventTypes(t, legacy); strings.Contains(events, "ipc.rejected") {
		t.Fatalf("the legacy record refused something: %s", events)
	}
	// Mail's stored timestamps are the wall clock's, so a record with mail matches
	// request for request, as TestReplayOfMailMatchesEveryRequest has it.
	if report := replayed(t, legacy, false); !report.LegacyBusBounds || report.Calls == 0 || report.Matched != report.Calls {
		t.Fatalf("legacy report = %+v, %+v", report, report.Divergence)
	}

	opts.noBounds = false
	current := recordAt(t, newRoot(t), context.Background(), oversizeScript, opts)
	if got := runStartedData(t, current)[agentflow.BusBoundsField]; got != agentflow.BusBoundsV1 {
		t.Fatalf("bus_bounds = %q", got)
	}
	if events := eventTypes(t, current); !strings.Contains(events, "ipc.rejected") {
		t.Fatalf("the record should hold a refusal: %s", events)
	}
	if report := replayed(t, current, false); report.LegacyBusBounds || report.Calls == 0 || report.Matched != report.Calls {
		t.Fatalf("current report = %+v, %+v", report, report.Divergence)
	}
}

func TestReplayOfHandles(t *testing.T) {
	stateDir := recordAt(t, newRoot(t), context.Background(), handlesScript, options{budget: capability.Budget{Seconds: 600}, mail: true, handles: true, maxSteps: 8})
	if report := replayed(t, stateDir, false); report.Calls == 0 || report.Matched != report.Calls {
		t.Fatalf("report = %+v, %+v", report, report.Divergence)
	}
}

// A run.started with or without redelegation_close replays as recorded. A record
// that re-delegated has more than one agent process, and replay refuses those.
func TestReplayOfARecordWithoutRedelegationClose(t *testing.T) {
	opts := options{budget: capability.Budget{Seconds: 600}, noRedelegationClose: true}
	legacy := recordAt(t, newRoot(t), context.Background(), roundTripScript, opts)
	if runStartedData(t, legacy)[agentflow.RedelegationCloseField] != "" {
		t.Fatal("the legacy record should have no redelegation_close")
	}
	if report := replayed(t, legacy, false); !report.Identical() {
		t.Fatalf("legacy report = %+v, %+v", report, report.Divergence)
	}
	opts.noRedelegationClose = false
	current := recordAt(t, newRoot(t), context.Background(), roundTripScript, opts)
	if got := runStartedData(t, current)[agentflow.RedelegationCloseField]; got != agentflow.RedelegationCloseV1 {
		t.Fatalf("redelegation_close = %q", got)
	}
	if report := replayed(t, current, false); !report.Identical() {
		t.Fatalf("current report = %+v, %+v", report, report.Divergence)
	}
}

// A publish by path takes its bytes from the record: the file is gone when it replays,
// and has other contents when it replays again, and the refused one is refused the same.
func TestReplayOfAPublishByPath(t *testing.T) {
	root := newRoot(t)
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("not for this process"), 0o644); err != nil {
		t.Fatal(err)
	}
	var file string
	stateDir, workspace := recordRunIn(t, root, context.Background(), func(ws string) []step {
		file = filepath.Join(ws, "raw_record.txt")
		return []step{
			tool("write_file", map[string]any{"path": file, "content": "render_ms_best=2.64186\n"}),
			tool("publish_artifact", map[string]any{"type": "raw", "path": file}),
			tool("publish_artifact", map[string]any{"type": "raw", "path": outside}),
			tool("publish_artifact", map[string]any{"type": "note", "body": "typed"}),
			exit("published"),
			exit("published"),
		}
	}, options{budget: capability.Budget{Seconds: 600}, mail: true, handles: true})
	_ = workspace
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	var published []artifact.Artifact
	for _, item := range items {
		if strings.HasPrefix(item.Type, artifact.PublishedPrefix) {
			published = append(published, item)
		}
	}
	if len(published) != 2 {
		t.Fatalf("published = %+v", published)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if report := replayed(t, stateDir, false); !report.Identical() {
		t.Fatalf("with the file gone: %+v, %+v", report, report.Divergence)
	}
	if err := os.WriteFile(file, []byte("something else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if report := replayed(t, stateDir, false); !report.Identical() {
		t.Fatalf("with the file changed: %+v, %+v", report, report.Divergence)
	}
}

// A record from before handles at a delegation replays with the prompt it ran with, and
// a record since then with its own.
func TestReplayOfARecordWithoutDelegateHandles(t *testing.T) {
	script := func(string) []step { return []step{exit("done")} }
	opts := options{budget: capability.Budget{Seconds: 600}, delegator: true, noDelegateHandles: true}
	legacy := recordAt(t, newRoot(t), context.Background(), script, opts)
	if runStartedData(t, legacy)[agentflow.DelegateHandlesField] != "" {
		t.Fatal("the legacy record should have no delegate_handles")
	}
	if recordHolds(t, legacy, "A delegate action may also carry") {
		t.Fatal("the legacy record should have the older prompt")
	}
	report := replayed(t, legacy, false)
	if !report.Identical() || !report.LegacyDelegateHandles {
		t.Fatalf("legacy report = %+v, %+v", report, report.Divergence)
	}
	opts.noDelegateHandles = false
	current := recordAt(t, newRoot(t), context.Background(), script, opts)
	if got := runStartedData(t, current)[agentflow.DelegateHandlesField]; got != agentflow.DelegateHandlesV1 {
		t.Fatalf("delegate_handles = %q", got)
	}
	if !recordHolds(t, current, "A delegate action may also carry") {
		t.Fatal("the current record should have the new prompt")
	}
	report = replayed(t, current, false)
	if !report.Identical() || report.LegacyDelegateHandles {
		t.Fatalf("current report = %+v, %+v", report, report.Divergence)
	}
}

// Before delegate_attach, artifacts at the action level were dropped unread, so an exit
// carrying them ended the run. Now it is refused, and the next exit ends it. Each
// replays as it ran, and the record says which it was.
func TestReplayOfARecordWithoutDelegateAttach(t *testing.T) {
	stray := step{action: map[string]any{"type": "exit", "artifacts": "art-000001", "exit_status": map[string]any{"code": 0, "reason": "done"}}}
	script := func(string) []step { return []step{stray, exit("done")} }
	const refused = "only a delegate, spawn_agent and message_agent attach artifacts"
	opts := options{budget: capability.Budget{Seconds: 600}, noDelegateAttach: true}
	legacy := recordAt(t, newRoot(t), context.Background(), script, opts)
	if runStartedData(t, legacy)[agentflow.DelegateAttachField] != "" {
		t.Fatal("the legacy record should have no delegate_attach")
	}
	if recordHolds(t, legacy, refused) {
		t.Fatal("the legacy record should not have refused the exit")
	}
	report := replayed(t, legacy, false)
	if !report.Identical() || !report.LegacyDelegateAttach {
		t.Fatalf("legacy report = %+v, %+v", report, report.Divergence)
	}
	opts.noDelegateAttach = false
	current := recordAt(t, newRoot(t), context.Background(), script, opts)
	if got := runStartedData(t, current)[agentflow.DelegateAttachField]; got != agentflow.DelegateAttachV1 {
		t.Fatalf("delegate_attach = %q", got)
	}
	if !recordHolds(t, current, refused) {
		t.Fatal("the current record should have refused the exit")
	}
	report = replayed(t, current, false)
	if !report.Identical() || report.LegacyDelegateAttach {
		t.Fatalf("current report = %+v, %+v", report, report.Divergence)
	}
}

// recordHolds reports whether any file under dir contains text.
func recordHolds(t *testing.T, dir, text string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found = found || strings.Contains(string(data), text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A record from before tools.DurationArg replays as it ran: the older prompt, and a
// bare or unreadable ttl took the default, with no tool error. Run's own timeout comes
// from the record either way.
func TestReplayOfARecordWithoutDurationArgs(t *testing.T) {
	script := func(string) []step {
		return []step{
			tool("acquire_lease", map[string]any{"resource": "bare", "ttl": "300"}),
			tool("acquire_lease", map[string]any{"resource": "words", "ttl": "soon"}),
			tool("run", map[string]any{"command": "true", "timeout": "300"}),
			exit("done"),
		}
	}
	opts := options{budget: capability.Budget{Seconds: 600}, noDurationArgs: true}
	legacy := recordAt(t, newRoot(t), context.Background(), script, opts)
	if runStartedData(t, legacy)[agentflow.DurationArgsField] != "" {
		t.Fatal("the legacy record should have no duration_args")
	}
	// Recording and replay share this code, so an identical replay alone would not
	// show that the old behaviour is the one being reproduced.
	if recordHolds(t, legacy, "whole seconds") || !recordHolds(t, filepath.Join(legacy, "kernel"), `"words"`) {
		t.Fatal("the legacy record should have the older prompt and the lease on words")
	}
	report := replayed(t, legacy, false)
	if !report.Identical() || !report.LegacyDurationArgs {
		t.Fatalf("legacy report = %+v, %+v", report, report.Divergence)
	}
	opts.noDurationArgs = false
	current := recordAt(t, newRoot(t), context.Background(), script, opts)
	if got := runStartedData(t, current)[agentflow.DurationArgsField]; got != agentflow.DurationArgsV1 {
		t.Fatalf("duration_args = %q", got)
	}
	if !recordHolds(t, current, "whole seconds") || recordHolds(t, filepath.Join(current, "kernel"), `"words"`) {
		t.Fatal("the current record should have the new prompt and no lease on words")
	}
	report = replayed(t, current, false)
	if !report.Identical() || report.LegacyDurationArgs {
		t.Fatalf("current report = %+v, %+v", report, report.Divergence)
	}
}

// A record from before publish_artifact took a path replays as it ran: its prompts and
// reads say nothing of a source. A body with no path is not refused there either.
func TestReplayOfARecordWithoutPublishSource(t *testing.T) {
	script := func(string) []step {
		return []step{
			tool("publish_artifact", map[string]any{"type": "report", "body": "typed"}),
			tool("publish_artifact", map[string]any{"type": "empty"}),
			tool("read_artifact", map[string]any{"id": "art-000002"}),
			exit("checked"),
			exit("checked"),
		}
	}
	opts := options{budget: capability.Budget{Seconds: 600}, mail: true, handles: true, noPublishSource: true}
	legacy := recordAt(t, newRoot(t), context.Background(), script, opts)
	if runStartedData(t, legacy)[agentflow.PublishSourceField] != "" {
		t.Fatal("the legacy record should have no publish_source")
	}
	if report := replayed(t, legacy, false); !report.Identical() {
		t.Fatalf("legacy report = %+v, %+v", report, report.Divergence)
	}
	opts.noPublishSource = false
	current := recordAt(t, newRoot(t), context.Background(), script, opts)
	if got := runStartedData(t, current)[agentflow.PublishSourceField]; got != agentflow.PublishSourceV1 {
		t.Fatalf("publish_source = %q", got)
	}
	if report := replayed(t, current, false); !report.Identical() {
		t.Fatalf("current report = %+v, %+v", report, report.Divergence)
	}
}

// A spec from before the spec contract, which it refuses (an edge without
// spawn_agent), replays under the rules it ran with; recorded since, the
// same spec does not run at all.
func TestReplayOfASpecFromBeforeTheContract(t *testing.T) {
	script := func(string) []step { return []step{exit("done")} }
	legacy := recordAt(t, newRoot(t), context.Background(), script, options{delegator: true, noSpecContract: true})
	if runStartedData(t, legacy)[agentflow.SpecContractField] != "" {
		t.Fatal("the legacy record should have no spec_contract")
	}
	if report := replayed(t, legacy, false); !report.Identical() || !report.LegacySpecContract {
		t.Fatalf("legacy: %+v, %+v", report, report.Divergence)
	}
	current := recordAt(t, newRoot(t), context.Background(), script, options{delegator: true})
	if data := runStartedData(t, current); data[agentflow.SpecContractField] != agentflow.SpecContractV1 {
		t.Fatalf("run.started %v", data)
	}
	if report := replayed(t, current, false); !report.Identical() || report.LegacySpecContract {
		t.Fatalf("current: %+v, %+v", report, report.Divergence)
	}
}
