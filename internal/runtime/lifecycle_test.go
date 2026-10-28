package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runtime"
	"aos/internal/statedir"
	"aos/internal/tools"
)

type lifecycleFixture struct {
	root    string
	kernel  *kernel.Kernel
	store   *artifact.Store
	bus     *ipc.Bus
	leases  *lease.Manager
	process kernel.Process
}

func newLifecycleFixture(t *testing.T) lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	bus, err := ipc.New(filepath.Join(root, "bus"))
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	leases, err := lease.New(filepath.Join(root, "leases"), k)
	if err != nil {
		t.Fatalf("new leases: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{
		Role:          "worker",
		CapabilitySet: capability.Set{Tools: []string{"acquire_lease", "release_lease"}},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return lifecycleFixture{root: root, kernel: k, store: store, bus: bus, leases: leases, process: process}
}

func (f lifecycleFixture) runtime(backend runtime.Backend) *runtime.AgentRuntime {
	return &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Leases: f.leases, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 6}
}

func messageResult(text string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: text}}
}

// latestCheckpoint is pid's newest checkpoint artifact.
func latestCheckpoint(t *testing.T, f lifecycleFixture, pid string) string {
	t.Helper()
	checkpoints, err := statedir.LatestCheckpoints(f.root)
	if err != nil {
		t.Fatalf("checkpoints: %v", err)
	}
	return checkpoints[pid]
}

// An infrastructure failure on any path leaves the process exited with a
// retryable runtime-origin record naming its latest checkpoint, never
// running. Each case checkpoints once (a message) and then fails.
func TestInfrastructureFailuresCloseTheProcess(t *testing.T) {
	cases := map[string]func(f lifecycleFixture) runtime.GenerateResult{
		"backend": nil,
		"artifact store": func(f lifecycleFixture) runtime.GenerateResult {
			os.WriteFile(filepath.Join(f.root, "artifacts", "index.json"), []byte("{"), 0o644)
			return messageResult("next")
		},
		"bus": func(f lifecycleFixture) runtime.GenerateResult {
			os.WriteFile(filepath.Join(f.root, "bus", "mailboxes.json"), []byte("{"), 0o644)
			return messageResult("next")
		},
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
				switch {
				case call == 1:
					return messageResult("first"), nil
				case inject == nil:
					return runtime.GenerateResult{}, errors.New("connection refused")
				default:
					return inject(f), nil
				}
			}}
			// The corrupted index cannot be read back, so that case checks
			// only that some checkpoint is named.
			var checkpoint string
			result, err := f.runtime(backend).Run(context.Background(), f.process.PID, nil)
			if err == nil {
				t.Fatal("expected the failure to be returned")
			}
			process, getErr := f.kernel.Get(f.process.PID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if process.State != kernel.StateExited || process.ExitStatus.Origin != "" || !process.ExitStatus.Retryable || process.ExitStatus.Code != 1 {
				t.Fatalf("process = %s %+v", process.State, process.ExitStatus)
			}
			if name != "artifact store" {
				checkpoint = latestCheckpoint(t, f, f.process.PID)
			}
			if process.ExitStatus.Checkpoint == "" || (checkpoint != "" && process.ExitStatus.Checkpoint != checkpoint) || result.CheckpointID != process.ExitStatus.Checkpoint {
				t.Fatalf("exit should name the latest checkpoint %q: record %+v, result %q", checkpoint, process.ExitStatus, result.CheckpointID)
			}
		})
	}
}

// When the kernel itself cannot be written, no exit can be recorded: the
// run returns both errors, and once the run is gone reap closes the
// process with its checkpoint so it can be recovered.
func TestKernelFailureIsReapable(t *testing.T) {
	f := newLifecycleFixture(t)
	kernelDir := filepath.Join(f.root, "kernel")
	defer os.Chmod(kernelDir, 0o755)
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return messageResult("first"), nil
		}
		if err := os.Chmod(kernelDir, 0o555); err != nil {
			t.Fatal(err)
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "wait"}}, nil
	}}
	_, err := f.runtime(backend).Run(context.Background(), f.process.PID, nil)
	if err == nil || errors.Is(err, runtime.ErrAgentBlocked) {
		t.Fatalf("expected the kernel write failure, got %v", err)
	}
	if err := os.Chmod(kernelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reaped, err := statedir.Reap(f.root)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("reap = %v, %v", reaped, err)
	}
	status, err := f.kernel.Await(f.process.PID)
	if err != nil || !status.Retryable || status.Reason != statedir.OrphanReason || status.Checkpoint != latestCheckpoint(t, f, f.process.PID) {
		t.Fatalf("reaped exit = %+v, %v", status, err)
	}
}

// A run of an exited process is refused before any model call or artifact.
func TestRunRefusesExitedProcess(t *testing.T) {
	f := newLifecycleFixture(t)
	if _, err := f.kernel.Exit(f.process.PID, kernel.ExitStatus{Code: 0, Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return messageResult("should not run"), nil
	}}
	_, err := f.runtime(backend).Run(context.Background(), f.process.PID, []runtime.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, runtime.ErrProcessExited) {
		t.Fatalf("expected ErrProcessExited, got %v", err)
	}
	artifacts, _ := f.store.List()
	if backend.calls != 0 || len(artifacts) != 0 {
		t.Fatalf("%d model calls and %d artifacts for an exited process", backend.calls, len(artifacts))
	}
}

// Recover refuses a live process (which would then run twice) and a final
// exit; Resume gives the same typed error.
func TestRecoverRefusesLiveAndFinalProcesses(t *testing.T) {
	f := newLifecycleFixture(t)
	backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return messageResult("parked"), nil
	}}
	agent := f.runtime(backend)
	agent.MaxSteps = 1
	options := runtime.RunOptions{StopOnAssistantMessage: true}
	parked, err := agent.RunWithOptions(context.Background(), f.process.PID, nil, options)
	if err != nil || !parked.Yielded {
		t.Fatalf("park: %+v %v", parked, err)
	}
	before, _ := f.kernel.Processes()
	if _, err := agent.Recover(context.Background(), f.process.PID, parked.CheckpointID, nil, options); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("recovering a live process: %v", err)
	}
	if after, _ := f.kernel.Processes(); len(after) != len(before) {
		t.Fatal("a refused recover spawned a process")
	}
	if _, err := f.kernel.Exit(f.process.PID, kernel.ExitStatus{Code: 1, Reason: "wrong answer", Origin: kernel.ExitOriginAgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Recover(context.Background(), f.process.PID, parked.CheckpointID, nil, options); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("recovering a final exit: %v", err)
	}
	if _, err := agent.Resume(context.Background(), f.process.PID, parked.CheckpointID, nil, options); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("resuming a final exit: %v", err)
	}
	if backend.calls != 1 {
		t.Fatalf("refusals made model calls: %d", backend.calls)
	}
}

// After a retryable exit, Recover's new process holds the old one's leases
// and can release them.
func TestRecoverCarriesLeasesOver(t *testing.T) {
	f := newLifecycleFixture(t)
	first := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return toolAction("acquire_lease", map[string]string{"resource": "src/main.go", "ttl": "1h"}), nil
		}
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	failed, err := f.runtime(first).Run(context.Background(), f.process.PID, nil)
	if err == nil || !failed.ExitStatus.Retryable {
		t.Fatalf("first run = %+v, %v", failed, err)
	}
	original, _, _ := f.leases.Get("src/main.go")
	if original.OwnerPID != "" {
		t.Fatalf("an exited owner's lease should read as free, got %+v", original)
	}
	var heldBy string
	second := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			current, _, _ := f.leases.Get("src/main.go")
			heldBy = current.OwnerPID
			return toolAction("release_lease", map[string]string{"resource": "src/main.go"}), nil
		}
		return exitResult("released"), nil
	}}
	recovered, err := f.runtime(second).Recover(context.Background(), f.process.PID, failed.ExitStatus.Checkpoint, nil, runtime.RunOptions{})
	if err != nil || recovered.ExitStatus.Code != 0 {
		t.Fatalf("recover = %+v, %v", recovered, err)
	}
	if heldBy != recovered.PID || heldBy == f.process.PID {
		t.Fatalf("lease held by %q during the recovered run, want %s", heldBy, recovered.PID)
	}
	if _, held, _ := f.leases.Get("src/main.go"); held {
		t.Fatal("the recovered process should have released the lease")
	}
	events, _ := f.kernel.Events()
	transferred := false
	for _, event := range events {
		if event.Type == "lease.transferred" && event.ProcessID == recovered.PID && event.Data["previous_owner"] == f.process.PID {
			transferred = true
		}
	}
	if !transferred {
		t.Fatal("the transfer should be a lease.transferred kernel event")
	}
}
