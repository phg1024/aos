//go:build unix

package kernel_test

import (
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
