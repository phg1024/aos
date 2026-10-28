//go:build unix

package lease_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"aos/internal/lease"
)

const contendEnv = "AOS_LEASE_CONTEND"

// TestLeaseContendHelper is a separate manager in its own process: it tries
// to take "shared" rounds times and, while holding it, bumps a counter
// without any lock of its own, recording each token it was granted.
func TestLeaseContendHelper(t *testing.T) {
	spec := os.Getenv(contendEnv)
	if spec == "" {
		t.Skip("subprocess helper")
	}
	dir, owner, _ := strings.Cut(spec, "|")
	manager, err := lease.New(filepath.Join(dir, "leases"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, owner+".tokens"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	counter := filepath.Join(dir, "counter")
	for round := 0; round < 200; round++ {
		granted, err := manager.Acquire("shared", owner, time.Hour)
		if err == lease.ErrLeaseHeld {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(counter)
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		time.Sleep(50 * time.Microsecond)
		if err := os.WriteFile(counter, []byte(strconv.Itoa(n+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(out, granted.Token)
		if err := manager.Release("shared", owner); err != nil {
			t.Fatal(err)
		}
	}
}

// Two managers in separate processes contending for one lease: never both
// hold it (the unlocked counter loses no increment), every grant gets its
// own token, and each process sees its tokens strictly increase.
func TestLeaseExclusiveAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	var helpers []*exec.Cmd
	for _, owner := range []string{"pid-a", "pid-b", "pid-c"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseContendHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), contendEnv+"="+dir+"|"+owner)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		helpers = append(helpers, cmd)
	}
	for _, cmd := range helpers {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper: %v", err)
		}
	}
	var all []int
	for _, owner := range []string{"pid-a", "pid-b", "pid-c"} {
		file, err := os.Open(filepath.Join(dir, owner+".tokens"))
		if err != nil {
			t.Fatal(err)
		}
		previous := 0
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			token, _ := strconv.Atoi(scanner.Text())
			if token <= previous {
				t.Fatalf("%s tokens not strictly increasing: %d after %d", owner, token, previous)
			}
			previous = token
			all = append(all, token)
		}
		file.Close()
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "counter"))
	if count, _ := strconv.Atoi(string(raw)); count != len(all) || len(all) == 0 {
		t.Fatalf("counter %d for %d grants: two holders overlapped", count, len(all))
	}
	t.Logf("%d grants across 3 processes x 200 attempts", len(all))
	sort.Ints(all)
	for i, token := range all {
		if token != i+1 {
			t.Fatalf("tokens should be 1..%d with no repeats, got %v", len(all), all)
		}
	}
}
