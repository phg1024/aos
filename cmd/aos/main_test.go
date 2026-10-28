package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runlog"
	"aos/internal/runtime"
)

// exitCompleter answers every completion with one exit action.
type exitCompleter struct{ status kernel.ExitStatus }

func (c exitCompleter) CompleteJSON(_ context.Context, _ string, _ string, target any) error {
	bytes, err := json.Marshal(map[string]any{"action": map[string]any{"type": "exit", "exit_status": c.status}})
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func writeSoloWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "solo.json")
	spec := `{"name":"solo","entry_agent":"solver","agents":[{"id":"solver","system_prompt":"Solve.","max_steps":3}]}`
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func TestRunWorkflowReportsEntryFailure(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.html")
	err := runWorkflow(context.Background(), exitCompleter{kernel.ExitStatus{Code: 3, Reason: "tests failed"}}, runtime.CompactionPolicy{}, capability.Budget{},
		writeSoloWorkflow(t, dir), nil, "", report, filepath.Join(dir, "state"), "go", false)
	var failed workflowFailed
	if !errors.As(err, &failed) {
		t.Fatalf("expected workflowFailed, got %v", err)
	}
	if failed.exitCode() != 3 || failed.Error() != "workflow failed: entry agent solver exited with code 3: tests failed" {
		t.Fatalf("unexpected failure %q with exit code %d", failed.Error(), failed.exitCode())
	}
	if _, statErr := os.Stat(report); statErr != nil {
		t.Fatalf("the HTML report should still be written on failure: %v", statErr)
	}
}

func TestRunWorkflowSucceedsWhenEntrySucceeds(t *testing.T) {
	dir := t.TempDir()
	err := runWorkflow(context.Background(), exitCompleter{kernel.ExitStatus{Code: 0, Reason: "done"}}, runtime.CompactionPolicy{}, capability.Budget{},
		writeSoloWorkflow(t, dir), nil, "", "", filepath.Join(dir, "state"), "go", false)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

// failingCompleter fails every completion, as a backend that is down does.
type failingCompleter struct{}

func (failingCompleter) CompleteJSON(context.Context, string, string, any) error {
	return errors.New("backend unreachable")
}

// cancellingCompleter cancels the run during its first completion, as Esc
// or Ctrl-C does.
type cancellingCompleter struct{ cancel context.CancelFunc }

func (c cancellingCompleter) CompleteJSON(ctx context.Context, _ string, _ string, _ any) error {
	c.cancel()
	return ctx.Err()
}

// captureStdout returns what fn printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = writer
	done := make(chan string)
	go func() {
		bytes, _ := io.ReadAll(reader)
		done <- string(bytes)
	}()
	fn()
	os.Stdout = saved
	writer.Close()
	return <-done
}

// A backend that fails every call is a failed workflow, not a Go error:
// the agent runs out of steps and its last checkpoint holds the errors.
func TestRunWorkflowReportsBackendFailure(t *testing.T) {
	dir := t.TempDir()
	var err error
	captureStdout(t, func() {
		err = runWorkflow(context.Background(), failingCompleter{}, runtime.CompactionPolicy{}, capability.Budget{},
			writeSoloWorkflow(t, dir), nil, "", "", filepath.Join(dir, "state"), "go", false)
	})
	var failed workflowFailed
	if !errors.As(err, &failed) || !strings.Contains(failed.Error(), "step limit exceeded") {
		t.Fatalf("expected a step-limit workflow failure, got %v", err)
	}
}

// A run that stops on a Go error after the root started still writes its
// report and summary, leaves no process without an exit record, and the
// trace records the stop against its kernel exit event.
func TestRunWorkflowReportsGoError(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.html")
	stateDir := filepath.Join(dir, "state")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error
	out := captureStdout(t, func() {
		err = runWorkflow(ctx, cancellingCompleter{cancel}, runtime.CompactionPolicy{}, capability.Budget{},
			writeSoloWorkflow(t, dir), nil, "", report, stateDir, "go", false)
	})
	if !errors.Is(err, runtime.ErrRunCancelled) {
		t.Fatalf("expected the cancellation, got %v", err)
	}
	html, readErr := os.ReadFile(report)
	if readErr != nil {
		t.Fatalf("the report should be written on a Go error: %v", readErr)
	}
	if !strings.Contains(string(html), "The run stopped on an error") {
		t.Fatal("the report should name the error")
	}
	var summary map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &summary); jsonErr != nil {
		t.Fatalf("summary is not JSON: %v\n%s", jsonErr, out)
	}
	if summary["root_pid"] != "pid-000001" || !strings.Contains(summary["error"].(string), "cancelled") || summary["trace_path"] != runlog.Path(stateDir, runlog.TraceFile) {
		t.Fatalf("summary = %v", summary)
	}
	if code := summary["exit_status"].(map[string]any)["code"]; code != float64(1) {
		t.Fatalf("root exit code = %v", code)
	}
	trace, _ := runlog.Read[agentflow.TraceRecord](runlog.Path(stateDir, runlog.TraceFile))
	if len(trace) == 0 || trace[len(trace)-1].Kind != "cancelled" || trace[len(trace)-1].KernelEvent == "" {
		t.Fatalf("trace = %+v", trace)
	}
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	processes, _ := k.Processes()
	for _, process := range processes {
		if process.State != kernel.StateExited {
			t.Fatalf("%s left without an exit record", process.PID)
		}
	}
}

// rootWatcher exits at once, noting the workflow root's state as it does.
type rootWatcher struct {
	stateDir string
	seen     *kernel.ProcessState
}

func (c rootWatcher) CompleteJSON(ctx context.Context, system string, user string, target any) error {
	if k, err := kernel.New(filepath.Join(c.stateDir, "kernel")); err == nil {
		if root, err := k.Get("pid-000001"); err == nil {
			*c.seen = root.State
		}
	}
	return exitCompleter{kernel.ExitStatus{Code: 0, Reason: "done"}}.CompleteJSON(ctx, system, user, target)
}

func TestWorkflowRootRunsWhileTheWorkflowDoes(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	var during kernel.ProcessState
	captureStdout(t, func() {
		if err := runWorkflow(context.Background(), rootWatcher{stateDir, &during}, runtime.CompactionPolicy{}, capability.Budget{},
			writeSoloWorkflow(t, dir), nil, "", "", stateDir, "go", false); err != nil {
			t.Errorf("run: %v", err)
		}
	})
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	root, _ := k.Get("pid-000001")
	if during != kernel.StateRunning || root.State != kernel.StateExited {
		t.Fatalf("root was %q during the run and %q after", during, root.State)
	}
}

func TestParseInterspersedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "--tail", "5", "DIR"},
		{"DIR", "--json", "--tail", "5"},
		{"--tail", "5", "DIR", "--json"},
	} {
		fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		tail := fs.Int("tail", 20, "")
		positional := parseInterspersed(fs, args)
		if len(positional) != 1 || positional[0] != "DIR" || !*asJSON || *tail != 5 {
			t.Fatalf("%v: positional %v json=%v tail=%d", args, positional, *asJSON, *tail)
		}
	}
}

func TestWorkflowFailedExitCode(t *testing.T) {
	for code, want := range map[int]int{1: 1, 3: 3, 125: 125, 126: 1, 130: 1, -1: 1} {
		if got := (workflowFailed{status: kernel.ExitStatus{Code: code}}).exitCode(); got != want {
			t.Fatalf("exitCode for root code %d = %d, want %d", code, got, want)
		}
	}
}
