//go:build unix

package artifact_test

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"

	"aos/internal/artifact"
)

const (
	helperRootEnv    = "AOS_ARTIFACT_HELPER_ROOT"
	createsPerWorker = 25
)

func createMany(root, worker string) error {
	store, err := artifact.New(root)
	if err != nil {
		return err
	}
	for i := 0; i < createsPerWorker; i++ {
		if _, err := store.CreateJSON("note", worker, fmt.Sprintf("%s-%d", worker, i), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// TestArtifactHelperProcess is the body of each subprocess.
func TestArtifactHelperProcess(t *testing.T) {
	root := os.Getenv(helperRootEnv)
	if root == "" {
		t.Skip("subprocess helper")
	}
	if err := createMany(root, os.Getenv("AOS_ARTIFACT_HELPER_WORKER")); err != nil {
		t.Fatal(err)
	}
}

// Separate processes and separate Store instances on one root lose no
// entries and allocate unique, gap-free IDs.
func TestConcurrentStoresShareARoot(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	workers := 0
	for i := 0; i < 3; i++ {
		worker := fmt.Sprintf("proc%d", i)
		cmd := exec.Command(os.Args[0], "-test.run=^TestArtifactHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), helperRootEnv+"="+root, "AOS_ARTIFACT_HELPER_WORKER="+worker)
		workers++
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
		workers++
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := createMany(root, worker); err != nil {
				errs <- fmt.Errorf("%s: %w", worker, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	store, err := artifact.New(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	total := workers * createsPerWorker
	if len(items) != total {
		t.Fatalf("expected %d artifacts (no lost entries), got %d", total, len(items))
	}
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.ID] = true
		var value string
		if err := store.ReadJSON(item.ID, &value); err != nil {
			t.Fatalf("read %s: %v", item.ID, err)
		}
	}
	for i := 1; i <= total; i++ {
		if id := fmt.Sprintf("art-%06d", i); !seen[id] {
			t.Fatalf("missing %s: IDs must be unique and gap-free", id)
		}
	}
}
