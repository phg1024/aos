//go:build unix

package kernel_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"aos/internal/kernel"
)

const helperRootEnv = "AOS_KERNEL_HELPER_ROOT"

const cyclesPerWorker = 15

// runCycles spawns, runs and exits processes, logging one caller event each.
func runCycles(root, worker string) error {
	k, err := kernel.New(root)
	if err != nil {
		return err
	}
	for i := 0; i < cyclesPerWorker; i++ {
		process, err := k.Spawn(kernel.ProcessSpec{Role: worker})
		if err != nil {
			return err
		}
		if _, err := k.Transition(process.PID, kernel.StateRunning); err != nil {
			return err
		}
		if err := k.LogEvent(process.PID, "test.note", map[string]string{"worker": worker}); err != nil {
			return err
		}
		if _, err := k.Exit(process.PID, kernel.ExitStatus{Code: 0, Reason: worker + "-" + strconv.Itoa(i)}); err != nil {
			return err
		}
	}
	return nil
}

// TestKernelHelperProcess is the body of each subprocess.
func TestKernelHelperProcess(t *testing.T) {
	root := os.Getenv(helperRootEnv)
	if root == "" {
		t.Skip("subprocess helper")
	}
	if err := runCycles(root, os.Getenv("AOS_KERNEL_HELPER_WORKER")); err != nil {
		t.Fatal(err)
	}
}

// Separate OS processes and separate Kernel instances on one state
// directory leave a consistent table and a gap-free event sequence.
func TestConcurrentWritersShareStateDirectory(t *testing.T) {
	root := t.TempDir()
	if _, err := kernel.New(root); err != nil {
		t.Fatalf("new: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	workers := []string{}
	for i := 0; i < 3; i++ {
		worker := fmt.Sprintf("proc%d", i)
		workers = append(workers, worker)
		cmd := exec.Command(os.Args[0], "-test.run=^TestKernelHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), helperRootEnv+"="+root, "AOS_KERNEL_HELPER_WORKER="+worker)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("%s: %v\n%s", worker, err, out)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		worker := fmt.Sprintf("inproc%d", i)
		workers = append(workers, worker)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runCycles(root, worker); err != nil {
				errs <- fmt.Errorf("%s: %w", worker, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	k, err := kernel.New(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	events, err := k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	total := len(workers) * cyclesPerWorker
	if len(events) != total*4 {
		t.Fatalf("expected %d events (no lost writes), got %d", total*4, len(events))
	}
	byProcess := map[string][]string{}
	for idx, event := range events {
		if want := fmt.Sprintf("evt-%06d", idx+1); event.ID != want {
			t.Fatalf("event %d has ID %s, want %s: IDs must be gap-free and increasing", idx, event.ID, want)
		}
		byProcess[event.ProcessID] = append(byProcess[event.ProcessID], event.Type)
	}
	processes, err := k.Processes()
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	if len(processes) != total {
		t.Fatalf("expected %d processes, got %d", total, len(processes))
	}
	perWorker := map[string]int{}
	for _, process := range processes {
		perWorker[process.Role]++
		if process.State != kernel.StateExited || process.ExitStatus == nil || !strings.HasPrefix(process.ExitStatus.Reason, process.Role+"-") {
			t.Fatalf("inconsistent record %+v", process)
		}
		if got := strings.Join(byProcess[process.PID], ","); got != "process.spawned,process.transition,test.note,process.exited" {
			t.Fatalf("%s events out of order: %s", process.PID, got)
		}
	}
	sort.Strings(workers)
	for _, worker := range workers {
		if perWorker[worker] != cyclesPerWorker {
			t.Fatalf("worker %s has %d processes, want %d", worker, perWorker[worker], cyclesPerWorker)
		}
	}
}

const supersedeTargetEnv = "AOS_KERNEL_SUPERSEDE_PID"

// TestKernelSupersedeHelper is a subprocess's one attempt to supersede a
// process; it prints the successor or the refusal.
func TestKernelSupersedeHelper(t *testing.T) {
	root, pid := os.Getenv(helperRootEnv), os.Getenv(supersedeTargetEnv)
	if root == "" || pid == "" {
		t.Skip("subprocess helper")
	}
	k, err := kernel.New(root)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := k.Supersede(pid, kernel.ProcessSpec{Role: "worker", Metadata: map[string]string{"resumed_from": pid}}, nil)
	switch {
	case err == nil:
		fmt.Println("SUCCESSOR", successor.PID)
	case errors.Is(err, kernel.ErrSuperseded):
		fmt.Println("REFUSED")
	default:
		t.Fatal(err)
	}
}

// Supersede is exclusive across OS processes and in-process goroutines
// alike: of 6 subprocesses and 6 goroutines racing on one exited process,
// exactly one gets a successor.
func TestSupersedeIsExclusiveAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Exit(target.PID, kernel.ExitStatus{Code: 1, Reason: "transient", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, refused := 0, 0
	errs := make(chan error, 12)
	for i := 0; i < 6; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestKernelSupersedeHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), helperRootEnv+"="+root, supersedeTargetEnv+"="+target.PID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("%v\n%s", err, out)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(string(out), "SUCCESSOR"):
				won++
			case strings.Contains(string(out), "REFUSED"):
				refused++
			}
		}()
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local, err := kernel.New(root)
			if err != nil {
				errs <- err
				return
			}
			_, err = local.Supersede(target.PID, kernel.ProcessSpec{Role: "worker", Metadata: map[string]string{"resumed_from": target.PID}}, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, kernel.ErrSuperseded):
				refused++
			default:
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if won != 1 || refused != 11 {
		t.Fatalf("%d successors and %d refusals, want 1 and 11", won, refused)
	}
	processes, err := k.Processes()
	if err != nil {
		t.Fatal(err)
	}
	successors := 0
	for _, process := range processes {
		if process.Metadata["resumed_from"] == target.PID {
			successors++
		}
	}
	got, _ := k.Get(target.PID)
	if successors != 1 || got.SupersededBy == "" {
		t.Fatalf("%d processes name %s resumed_from; superseded_by=%q", successors, target.PID, got.SupersededBy)
	}
}
