//go:build supervisionaudit

// Package supervision_test holds the audit probes behind
// supervision-design.md: each probe runs one claim about supervision,
// leases or recovery against the code as it is and logs what happened as
// "FINDING:" lines. They fail only when a probe cannot run. They are kept
// out of go test ./... by the supervisionaudit tag:
//
//	go test -tags supervisionaudit -count=1 -v ./bench/supervision/
package supervision_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runtime"
	"aos/internal/statedir"
	"aos/internal/tools"
)

type fixture struct {
	kernel    *kernel.Kernel
	store     *artifact.Store
	leases    *lease.Manager
	workspace string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	return newFixtureAt(t, t.TempDir())
}

// newFixtureAt opens a state directory's kernel, artifacts and leases.
func newFixtureAt(t *testing.T, root string) fixture {
	t.Helper()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	leases, err := lease.New(filepath.Join(root, "leases"), k)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return fixture{kernel: k, store: store, leases: leases, workspace: workspace}
}

func (f fixture) spawn(t *testing.T, role string, tools ...string) kernel.Process {
	t.Helper()
	process, err := f.kernel.Spawn(kernel.ProcessSpec{
		Role:          role,
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: f.workspace}},
		CapabilitySet: capability.Set{Tools: tools, ReadPaths: []string{f.workspace}, WritePaths: []string{f.workspace}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func (f fixture) runtime(backend runtime.Backend) *runtime.AgentRuntime {
	return &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Tools: tools.NewRuntime(), Leases: f.leases, Backend: backend, MaxSteps: 8}
}

func (f fixture) events(t *testing.T, prefix string) []kernel.Event {
	t.Helper()
	events, err := f.kernel.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range events {
		if strings.HasPrefix(event.Type, prefix) {
			out = append(out, event)
		}
	}
	return out
}

func tool(name string, args map[string]string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: name, Args: args}}}
}

func exit(code int, retryable bool, reason string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: code, Reason: reason, Retryable: retryable}}}
}

// stepBackend replies with steps in order and runs before[i] first.
type stepBackend struct {
	mu     sync.Mutex
	steps  []runtime.GenerateResult
	before map[int]func()
	calls  int
}

func (b *stepBackend) Generate(ctx context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
	b.mu.Lock()
	i := b.calls
	b.calls++
	b.mu.Unlock()
	if hook := b.before[i]; hook != nil {
		hook()
	}
	if i >= len(b.steps) {
		return exit(0, false, "done"), nil
	}
	return b.steps[i], nil
}

// failedRun runs a process that exits retryably and returns it and its
// last checkpoint.
func failedRun(t *testing.T, f fixture, steps ...runtime.GenerateResult) (kernel.Process, string) {
	t.Helper()
	process := f.spawn(t, "agent:worker", "write_file", "acquire_lease")
	result, err := f.runtime(&stepBackend{steps: append(steps, exit(1, true, "transient failure"))}).RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "work"}}, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.CheckpointID == "" {
		t.Fatal("no checkpoint")
	}
	process, _ = f.kernel.Get(process.PID)
	return process, result.CheckpointID
}

// A1 is a command, not a test: go list -deps ./cmd/aos ./cmd/aos-tui.

// A2: two lease managers in separate OS processes, one attempt each at a
// shared start, 200 trials (the book's experiment).
const contendEnv = "AOS_AUDIT_CONTEND"

func TestAuditLeaseContendHelper(t *testing.T) {
	spec := os.Getenv(contendEnv)
	if spec == "" {
		t.Skip("subprocess helper")
	}
	dir, owner, _ := strings.Cut(spec, "|")
	manager, err := lease.New(filepath.Join(dir, "leases"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
			break
		}
	}
	_, err = manager.Acquire("shared", owner, time.Hour)
	result := "won"
	if errors.Is(err, lease.ErrLeaseHeld) {
		result = "lost"
	} else if err != nil {
		result = "error " + err.Error()
	}
	os.WriteFile(filepath.Join(dir, owner+".result"), []byte(result), 0o644)
}

func TestAuditA2LeaseAcrossProcesses(t *testing.T) {
	const trials = 200
	both, errorsSeen := 0, 0
	for trial := 0; trial < trials; trial++ {
		dir := t.TempDir()
		var helpers []*exec.Cmd
		for _, owner := range []string{"pid-a", "pid-b"} {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAuditLeaseContendHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), contendEnv+"="+dir+"|"+owner)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			helpers = append(helpers, cmd)
		}
		time.Sleep(20 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, "go"), nil, 0o644)
		for _, cmd := range helpers {
			if err := cmd.Wait(); err != nil {
				t.Fatalf("helper: %v", err)
			}
		}
		won := 0
		for _, owner := range []string{"pid-a", "pid-b"} {
			raw, _ := os.ReadFile(filepath.Join(dir, owner+".result"))
			switch {
			case string(raw) == "won":
				won++
			case string(raw) != "lost":
				errorsSeen++
			}
		}
		if won == 2 {
			both++
		}
	}
	t.Logf("FINDING: A2 both managers won in %d of %d trials (%d helper errors)", both, trials, errorsSeen)
}

// A3: a lease whose holder exited is free; a recovered process gets its
// predecessor's lease.
func TestAuditA3ReleaseOnExit(t *testing.T) {
	f := newFixture(t)
	holder := f.spawn(t, "holder")
	if _, err := f.leases.Acquire("src/a.go", holder.PID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.kernel.Exit(holder.PID, kernel.ExitStatus{Code: 1, Reason: "crashed"}); err != nil {
		t.Fatal(err)
	}
	other := f.spawn(t, "other")
	granted, err := f.leases.Acquire("src/a.go", other.PID, time.Hour)
	taken := f.events(t, "lease.taken_over")
	t.Logf("FINDING: A3 after the holder exited, another process acquired: err=%v token=%d taken_over=%d reason=%q", err, granted.Token, len(taken), reason(taken))

	f = newFixture(t)
	failed, checkpoint := failedRun(t, f, tool("acquire_lease", map[string]string{"resource": "src/b.go"}))
	result, err := f.runtime(&stepBackend{steps: []runtime.GenerateResult{tool("acquire_lease", map[string]string{"resource": "src/b.go"})}}).Recover(context.Background(), failed.PID, checkpoint, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	transferred := f.events(t, "lease.transferred")
	renewed := f.events(t, "lease.renewed")
	tokens := ""
	if len(transferred) == 1 {
		tokens = transferred[0].Data["previous_token"] + " -> " + transferred[0].Data["token"]
	}
	t.Logf("FINDING: A3 a lease held by %s at its retryable exit: recovered %s, lease.transferred=%d (token %s), the recovered agent's own acquire_lease renewed=%d", failed.PID, result.PID, len(transferred), tokens, len(renewed))
}

func reason(events []kernel.Event) string {
	if len(events) == 0 {
		return ""
	}
	return events[0].Data["reason"]
}

// A4: fencing tokens exist; does anything check them where the effect
// happens?
func TestAuditA4FencingAtThePointOfEffect(t *testing.T) {
	f := newFixture(t)
	writer := f.spawn(t, "agent:writer", "acquire_lease", "write_file")
	thief := f.spawn(t, "agent:thief")
	target := filepath.Join(f.workspace, "a.txt")
	var acquired string
	backend := &stepBackend{
		steps: []runtime.GenerateResult{
			tool("acquire_lease", map[string]string{"resource": target, "ttl": "1ms"}),
			tool("write_file", map[string]string{"path": target, "content": "stale holder"}),
			exit(0, false, "done"),
		},
		before: map[int]func(){1: func() {
			time.Sleep(5 * time.Millisecond)
			granted, err := f.leases.Acquire(target, thief.PID, time.Hour)
			acquired = fmt.Sprintf("thief token=%d err=%v", granted.Token, err)
		}},
	}
	var outputs []map[string]string
	_, err := f.runtime(backend).RunWithOptions(context.Background(), writer.PID, []runtime.Message{{Role: "user", Content: "work"}}, runtime.RunOptions{
		ContinueOnToolError: true,
		OnToolResult:        func(result tools.Result) { outputs = append(outputs, result.Output) },
	})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(target)
	_, hasToken := outputs[0]["token"]
	t.Logf("FINDING: A4 %s; the writer's lease expired and was taken over, then its write_file wrote %q; acquire_lease output shows a token: %v (keys %v)", acquired, content, hasToken, keys(outputs[0]))
}

func keys(m map[string]string) []string {
	var out []string
	for key := range m {
		out = append(out, key)
	}
	return out
}

// A5: Recover's state checks, plus what they do not cover.
func TestAuditA5RecoverGuards(t *testing.T) {
	f := newFixture(t)
	failed, checkpoint := failedRun(t, f)

	live := f.spawn(t, "agent:live")
	if _, err := f.kernel.Transition(live.PID, kernel.StateRunning); err != nil {
		t.Fatal(err)
	}
	_, liveErr := f.runtime(&stepBackend{}).Recover(context.Background(), live.PID, checkpoint, nil, runtime.RunOptions{})
	final := f.spawn(t, "agent:final")
	f.kernel.Exit(final.PID, kernel.ExitStatus{Code: 2, Reason: "bad input", Retryable: false})
	_, finalErr := f.runtime(&stepBackend{}).Recover(context.Background(), final.PID, checkpoint, nil, runtime.RunOptions{})
	other := f.spawn(t, "agent:other")
	f.kernel.Exit(other.PID, kernel.ExitStatus{Code: 1, Reason: "x", Retryable: true})
	_, foreignErr := f.runtime(&stepBackend{}).Recover(context.Background(), other.PID, checkpoint, nil, runtime.RunOptions{})
	t.Logf("FINDING: A5 Recover of a running process: %v", liveErr)
	t.Logf("FINDING: A5 Recover of a non-retryable exit: %v", finalErr)
	t.Logf("FINDING: A5 Recover from another process's checkpoint: %v", foreignErr)

	// The same exited process recovered twice, one after the other.
	first, err1 := f.runtime(&stepBackend{steps: []runtime.GenerateResult{exit(1, true, "again")}}).Recover(context.Background(), failed.PID, checkpoint, nil, runtime.RunOptions{})
	second, err2 := f.runtime(&stepBackend{}).Recover(context.Background(), failed.PID, checkpoint, nil, runtime.RunOptions{})
	t.Logf("FINDING: A5 sequential double Recover of %s: first=%s (%v) second=%s (%v)", failed.PID, first.PID, err1, second.PID, err2)

	// Twenty concurrent Recovers of one exited process.
	g := newFixture(t)
	target, targetCheckpoint := failedRun(t, g)
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.runtime(&stepBackend{}).Recover(context.Background(), target.PID, targetCheckpoint, nil, runtime.RunOptions{}); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	processes, _ := g.kernel.Processes()
	successors := 0
	for _, process := range processes {
		if process.Metadata["resumed_from"] == target.PID {
			successors++
		}
	}
	t.Logf("FINDING: A5 20 concurrent Recovers of %s: %d succeeded, %d processes now name it resumed_from", target.PID, succeeded, successors)

	// An older checkpoint than the exit names.
	h := newFixture(t)
	older, _ := failedRun(t, h, tool("write_file", map[string]string{"path": filepath.Join(h.workspace, "x"), "content": "1"}))
	checkpoints, _ := h.store.List()
	var ids []string
	for _, record := range checkpoints {
		if record.Type == "checkpoint" && record.ProducerPID == older.PID {
			ids = append(ids, record.ID)
		}
	}
	_, olderErr := h.runtime(&stepBackend{}).Recover(context.Background(), older.PID, ids[0], nil, runtime.RunOptions{})
	t.Logf("FINDING: A5 Recover from the first of %d checkpoints while the exit names %s: err=%v", len(ids), older.ExitStatus.Checkpoint, olderErr)
}

// A6: running an exited process.
func TestAuditA6RunExitedProcess(t *testing.T) {
	f := newFixture(t)
	failed, _ := failedRun(t, f)
	before, _ := f.store.List()
	backend := &stepBackend{steps: []runtime.GenerateResult{tool("write_file", map[string]string{"path": filepath.Join(f.workspace, "late"), "content": "x"})}}
	_, err := f.runtime(backend).RunWithOptions(context.Background(), failed.PID, []runtime.Message{{Role: "user", Content: "again"}}, runtime.RunOptions{ContinueOnToolError: true})
	after, _ := f.store.List()
	_, statErr := os.Stat(filepath.Join(f.workspace, "late"))
	t.Logf("FINDING: A6 RunWithOptions on exited %s: err=%v, model calls=%d, artifacts %d -> %d, file written: %v", failed.PID, err, backend.calls, len(before), len(after), statErr == nil)
}

// A7: a command killed mid-flight, then recovered: what the record holds
// about the side effect after the last checkpoint.
func TestAuditA7KilledMidCommand(t *testing.T) {
	f := newFixture(t)
	process := f.spawn(t, "agent:runner", "run")
	marker := filepath.Join(f.workspace, "effects")
	ctx, cancel := context.WithCancel(context.Background())
	backend := &stepBackend{steps: []runtime.GenerateResult{
		tool("run", map[string]string{"command": "echo once >> " + marker + "; sleep 5"}),
	}}
	agent := f.runtime(backend)
	agent.Tools.ScratchRoot = t.TempDir()
	time.AfterFunc(500*time.Millisecond, cancel)
	result, err := agent.RunWithOptions(ctx, process.PID, []runtime.Message{{Role: "user", Content: "work"}}, runtime.RunOptions{ContinueOnToolError: true})
	exited, _ := f.kernel.Get(process.PID)
	records, _ := f.store.List()
	var kinds []string
	for _, record := range records {
		if record.ProducerPID == process.PID && record.Type != "checkpoint" {
			kinds = append(kinds, record.Type+"("+record.Metadata["tool"]+")")
		}
	}
	session, _ := agent.RestoreCheckpoint(result.CheckpointID)
	last := ""
	if n := len(session.Messages); n > 0 {
		last = session.Messages[n-1].Content
		if len(last) > 160 {
			last = last[:160]
		}
	}
	effects, _ := os.ReadFile(marker)
	t.Logf("FINDING: A7 cancelled mid-command: err=%v state=%s exit=%+v checkpoint=%s", err, exited.State, exited.ExitStatus, result.CheckpointID)
	t.Logf("FINDING: A7 non-checkpoint artifacts: %v; the checkpoint's last message: %q; effects so far: %q", kinds, last, effects)

	// Resume (it parked) or Recover (it exited); a model that repeats the
	// command repeats the effect.
	again := &stepBackend{steps: []runtime.GenerateResult{tool("run", map[string]string{"command": "echo once >> " + marker}), exit(0, false, "done")}}
	recoveredRuntime := f.runtime(again)
	recoveredRuntime.Tools.ScratchRoot = t.TempDir()
	recovered, err := recoveredRuntime.Resume(context.Background(), process.PID, result.CheckpointID, nil, runtime.RunOptions{ContinueOnToolError: true})
	effects, _ = os.ReadFile(marker)
	t.Logf("FINDING: A7 continued as %s (%v): effects now %q", recovered.PID, err, effects)
}

// A7b: the same command stopped the way aos run stops on SIGTERM.
func TestAuditA7TerminatedMidCommand(t *testing.T) {
	f := newFixture(t)
	process := f.spawn(t, "agent:runner", "run")
	ctx, cancel := context.WithCancelCause(context.Background())
	agent := f.runtime(&stepBackend{steps: []runtime.GenerateResult{tool("run", map[string]string{"command": "sleep 5"})}})
	agent.Tools.ScratchRoot = t.TempDir()
	time.AfterFunc(500*time.Millisecond, func() { cancel(fmt.Errorf("%w (SIGTERM)", runtime.ErrTerminated)) })
	result, err := agent.RunWithOptions(ctx, process.PID, []runtime.Message{{Role: "user", Content: "work"}}, runtime.RunOptions{ContinueOnToolError: true})
	exited, _ := f.kernel.Get(process.PID)
	t.Logf("FINDING: A7b terminated mid-command: err=%v state=%s exit=%+v checkpoint=%s", err, exited.State, exited.ExitStatus, result.CheckpointID)
}

// A8: the OS process running an agent is killed with SIGKILL mid-command.
const killEnv = "AOS_AUDIT_KILL"

func TestAuditKillHelper(t *testing.T) {
	root := os.Getenv(killEnv)
	if root == "" {
		t.Skip("subprocess helper")
	}
	f := newFixtureAt(t, root)
	process := f.spawn(t, "agent:runner", "acquire_lease", "write_file", "run")
	marker := filepath.Join(f.workspace, "effects")
	agent := f.runtime(&stepBackend{steps: []runtime.GenerateResult{
		tool("acquire_lease", map[string]string{"resource": "src/c.go", "ttl": "1h"}),
		tool("write_file", map[string]string{"path": filepath.Join(f.workspace, "step2"), "content": "done"}),
		tool("run", map[string]string{"command": "echo once >> " + marker + "; sleep 30"}),
	}})
	agent.Tools.ScratchRoot = filepath.Join(root, "scratch")
	agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "work"}}, runtime.RunOptions{ContinueOnToolError: true})
}

func TestAuditA8KilledProcess(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuditKillHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), killEnv+"="+root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "workspace", "effects")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the helper never reached the command")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cmd.Process.Kill()
	cmd.Wait()

	f := newFixtureAt(t, root)
	process, _ := f.kernel.Get("pid-000001")
	held, ok, _ := f.leases.Get("src/c.go")
	t.Logf("FINDING: A8 after SIGKILL: %s is %s; lease src/c.go held=%v by %s until %s", process.PID, process.State, ok, held.OwnerPID, held.ExpiresAt.Format(time.RFC3339))
	other := f.spawn(t, "agent:other")
	_, err := f.leases.Acquire("src/c.go", other.PID, time.Hour)
	t.Logf("FINDING: A8 another process acquiring the dead holder's lease: %v", err)
	_, recoverErr := f.runtime(&stepBackend{}).Recover(context.Background(), process.PID, "", nil, runtime.RunOptions{})
	t.Logf("FINDING: A8 Recover before reap: %v", recoverErr)

	reaped, err := statedir.Reap(root)
	process, _ = f.kernel.Get("pid-000001")
	t.Logf("FINDING: A8 aos reap: reaped %v (%v); exit %+v", reaped, err, process.ExitStatus)
	session, _ := f.runtime(nil).RestoreCheckpoint(process.ExitStatus.Checkpoint)
	last := session.Messages[len(session.Messages)-1].Content
	if len(last) > 120 {
		last = last[:120]
	}
	t.Logf("FINDING: A8 the checkpoint reap names holds %d messages, the last: %q", len(session.Messages), last)
	recovered, err := f.runtime(&stepBackend{}).Recover(context.Background(), process.PID, process.ExitStatus.Checkpoint, nil, runtime.RunOptions{})
	transferred := f.events(t, "lease.transferred")
	effects, _ := os.ReadFile(marker)
	t.Logf("FINDING: A8 Recover after reap: %s (%v); lease.transferred=%d; effects %q, recorded nowhere but the file", recovered.PID, err, len(transferred), effects)
}
