//go:build darwin

// Package lpprobes holds the least-privilege probes for chapter 4 of the
// book (aos book's request of 2026-10-31): scripted agents, no model, run
// through the real AgentRuntime against a throwaway workspace and state
// directory, with each attempt's record written to results/<probe>.json.
//
// Run from the repository root, in this order (P2 last: it stops at the
// first write that lands):
//
//	go test ./reports/least-privilege-probes/ -count=1 -v -run 'TestP1|TestP3|TestP4'
//	go test ./reports/least-privilege-probes/ -count=1 -v -run TestP2
package lpprobes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/sandbox"
	"aos/internal/tools"
)

// step is one scripted tool call and what the request expects of it.
type step struct {
	ID, Attempt, Expect string
	Tool                string
	Args                map[string]string
	// Dynamic, when set, builds Args when the step is played, from the
	// disk as it is then.
	Dynamic func() map[string]string
}

// Record is what one attempt did, as the runtime recorded it.
type Record struct {
	ID      string            `json:"id"`
	Attempt string            `json:"attempt"`
	Tool    string            `json:"tool"`
	Args    map[string]string `json:"args"`
	Expect  string            `json:"expect"`
	// Saw is the exact tool message the agent got back.
	Saw string `json:"saw"`
	// Disk lists files created, changed or removed by the attempt, outside
	// the runtime's own state files and the run scratch directory.
	Disk       []string            `json:"disk"`
	ToolError  bool                `json:"tool_error_artifact"`
	ToolResult bool                `json:"tool_result_artifact"`
	Artifact   string              `json:"artifact,omitempty"`
	Denied     []map[string]string `json:"capability_denied,omitempty"`
	Extra      map[string]string   `json:"extra,omitempty"`
}

// Run is one scripted process.
type Run struct {
	Name     string            `json:"name"`
	Grants   string            `json:"grants"`
	Records  []Record          `json:"records"`
	Exit     kernel.ExitStatus `json:"exit"`
	RunError string            `json:"run_error,omitempty"`
	// DeniedEvents counts the process's capability.denied events in the
	// kernel log, as a cross-check of the per-attempt attribution.
	DeniedEvents int    `json:"denied_events"`
	EventsError  string `json:"events_error,omitempty"`
	Stopped      string `json:"stopped,omitempty"`
}

// world is a throwaway layout: root/ws (workspace), root/ref (reference),
// root/outside (outside every mount), the state directory (default
// ws/.aos-state, holding the kernel and artifact stores, protected as
// cmd/aos protects it) and root/scratch for run's scratch directories.
type world struct {
	t                                      *testing.T
	root, ws, ref, outside, state, scratch string
	k                                      *kernel.Kernel
	store                                  *artifact.Store
	tools                                  *tools.Runtime
	extraWatch                             []string
}

// newWorld lays the world out; ref and state, when not empty, place the
// reference tree and the state directory relative to ws instead.
func newWorld(t *testing.T, ref, state string) *world {
	t.Helper()
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, root: root, ws: filepath.Join(root, "ws"), outside: filepath.Join(root, "outside"), scratch: filepath.Join(root, "scratch")}
	w.ref = filepath.Join(root, "ref")
	if ref != "" {
		w.ref = filepath.Join(w.ws, ref)
	}
	w.state = filepath.Join(w.ws, ".aos-state")
	if state != "" {
		w.state = filepath.Join(w.ws, state)
	}
	for _, dir := range []string{filepath.Join(w.ws, "out"), w.ref, w.outside, w.scratch} {
		must(t, os.MkdirAll(dir, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(w.ref, "reference.txt"), []byte("reference image\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(w.outside, "secret.txt"), []byte("hunter2\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(w.outside, "movable.txt"), []byte("movable\n"), 0o644))
	if w.k, err = kernel.New(filepath.Join(w.state, "kernel")); err != nil {
		t.Fatal(err)
	}
	if w.store, err = artifact.New(filepath.Join(w.state, "artifacts")); err != nil {
		t.Fatal(err)
	}
	w.tools = tools.NewRuntime()
	w.tools.Protected = []string{w.state, w.state + ".lock"}
	w.tools.ScratchRoot = w.scratch
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// snapshot hashes every file under root and the extra watched paths,
// except the run scratch directories and the runtime's own state files
// (any state file whose name does not start with "probe").
func (w *world) snapshot() map[string]string {
	out := map[string]string{}
	add := func(path string, d fs.DirEntry) {
		if d.IsDir() {
			out[path] = "dir"
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			out[path] = "unreadable: " + err.Error()
			return
		}
		sum := sha256.Sum256(raw)
		out[path] = hex.EncodeToString(sum[:8])
	}
	filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if capability.Within(w.scratch, path) {
			return filepath.SkipDir
		}
		if capability.Within(w.state, path) && path != w.state && !strings.HasPrefix(d.Name(), "probe") {
			return nil // the runtime's own state; keep walking for probe files
		}
		add(path, d)
		return nil
	})
	for _, path := range w.extraWatch {
		if info, err := os.Lstat(path); err == nil {
			add(path, fs.FileInfoToDirEntry(info))
		}
	}
	return out
}

func diff(before, after map[string]string) []string {
	var out []string
	for path, sum := range after {
		switch old, ok := before[path]; {
		case !ok:
			out = append(out, "created "+path)
		case old != sum:
			out = append(out, "changed "+path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			out = append(out, "removed "+path)
		}
	}
	sort.Strings(out)
	return out
}

// scripted plays the steps in order, then exits 0. Before each model call
// it snapshots the disk, so the difference between two calls is what the
// step between them did; stopAfter, when set, ends the run early (exit 0)
// once a step's change satisfies it.
type scripted struct {
	w         *world
	steps     []step
	snaps     []map[string]string
	stopAfter func(changes []string) bool
	stopped   string
}

func (s *scripted) Generate(_ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
	s.snaps = append(s.snaps, s.w.snapshot())
	i := len(s.snaps) - 1
	if i > 0 && s.stopAfter != nil && s.stopped == "" {
		if changes := diff(s.snaps[i-1], s.snaps[i]); s.stopAfter(changes) {
			s.stopped = s.steps[i-1].ID + ": " + strings.Join(changes, "; ")
		}
	}
	if i >= len(s.steps) || s.stopped != "" {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "script done"}}}, nil
	}
	if s.steps[i].Dynamic != nil {
		s.steps[i].Args = s.steps[i].Dynamic()
	}
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: s.steps[i].Tool, Args: s.steps[i].Args}}}, nil
}

// run spawns one process with the grants and plays the steps through the
// AgentRuntime, as agentflow does (ContinueOnToolError), and returns what
// each step left in the artifact store, the kernel log and on disk.
func (w *world) run(name, grants string, set capability.Set, mounts []capability.Mount, steps []step, stopAfter func([]string) bool) Run {
	t := w.t
	t.Helper()
	process, err := w.k.Spawn(kernel.ProcessSpec{Role: "probe", Mounts: mounts, CapabilitySet: set})
	must(t, err)
	backend := &scripted{w: w, steps: steps, stopAfter: stopAfter}
	current := -1
	denied := map[int][]map[string]string{}
	// Compaction limits high enough that nothing is archived, so every tool
	// message the agent got is still in the session afterwards.
	agent := &runtime.AgentRuntime{Kernel: w.k, Artifacts: w.store, Tools: w.tools, Backend: backend, MaxSteps: len(steps) + 2,
		Compaction: runtime.CompactionPolicy{MaxMessages: 1 << 20, MaxChars: 1 << 30},
		OnCapabilityDenied: func(event kernel.Event) {
			if event.ProcessID == process.PID {
				denied[current] = append(denied[current], event.Data)
			}
		}}
	result, runErr := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{
		ContinueOnToolError: true,
		OnToolCall:          func(runtime.ToolCall) { current++ },
	})
	done := len(backend.snaps) - 1 // steps played
	if done > len(steps) {
		done = len(steps)
	}
	out := Run{Name: name, Grants: grants, Exit: result.ExitStatus, Stopped: backend.stopped}
	if runErr != nil {
		out.RunError = runErr.Error()
	}
	// One tool_result or tool_error artifact per tool call, in order.
	items, err := w.store.List()
	must(t, err)
	var records []artifact.Artifact
	for _, item := range items {
		if item.ProducerPID == process.PID && (item.Type == "tool_result" || item.Type == "tool_error") {
			records = append(records, item)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	// One tool message per call (a tool_notice after it belongs to it).
	var saw []string
	for _, message := range result.Session.Messages {
		if message.Role != "tool" {
			continue
		}
		if strings.HasPrefix(message.Content, "tool_notice") && len(saw) > 0 {
			saw[len(saw)-1] += "\n" + message.Content
			continue
		}
		saw = append(saw, message.Content)
	}
	if len(records) != done || len(saw) != done {
		t.Errorf("%s: %d steps played, %d tool artifacts, %d tool messages", name, done, len(records), len(saw))
	}
	for i := 0; i < done; i++ {
		s := steps[i]
		rec := Record{ID: s.ID, Attempt: s.Attempt, Tool: s.Tool, Args: s.Args, Expect: s.Expect, Denied: denied[i]}
		if i < len(saw) {
			rec.Saw = saw[i]
		}
		if i < len(records) {
			rec.Artifact = records[i].ID + " (" + records[i].Type + ")"
			rec.ToolError = records[i].Type == "tool_error"
			rec.ToolResult = records[i].Type == "tool_result"
		}
		if i+1 < len(backend.snaps) {
			rec.Disk = diff(backend.snaps[i], backend.snaps[i+1])
		}
		out.Records = append(out.Records, rec)
	}
	events, err := w.k.Events()
	if err != nil {
		out.EventsError = err.Error()
	}
	for _, event := range events {
		if event.ProcessID == process.PID && event.Type == "capability.denied" {
			out.DeniedEvents++
		}
	}
	return out
}

// write saves a probe's runs to results/<probe>.json.
func write(t *testing.T, probe string, value any) {
	t.Helper()
	must(t, os.MkdirAll("results", 0o755))
	raw, err := json.MarshalIndent(value, "", " ")
	must(t, err)
	must(t, os.WriteFile(filepath.Join("results", probe+".json"), append(raw, '\n'), 0o644))
}

func tool(id, attempt, expect, name string, args ...string) step {
	m := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		m[args[i]] = args[i+1]
	}
	return step{ID: id, Attempt: attempt, Expect: expect, Tool: name, Args: m}
}

func runCmd(id, attempt, expect, command string) step {
	return tool(id, attempt, expect, "run", "command", command, "timeout", "30s")
}

func writeFile(id, attempt, expect, path string) step {
	return tool(id, attempt, expect, "write_file", "path", path, "content", "probe\n")
}
