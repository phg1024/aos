package capability_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"aos/internal/capability"
)

func TestDeriveChildSetUsesPathContainment(t *testing.T) {
	parent := capability.Set{ReadPaths: []string{"/ws"}, WritePaths: []string{"/ws/out"}}
	requested := capability.Set{ReadPaths: []string{"/ws/src", "/etc"}, WritePaths: []string{"/ws"}}
	derived := capability.DeriveChildSet(parent, requested)
	if !reflect.DeepEqual(derived.ReadPaths, []string{"/ws/src"}) {
		t.Fatalf("a narrower requested scope is kept and an outside one dropped, got %v", derived.ReadPaths)
	}
	if !reflect.DeepEqual(derived.WritePaths, []string{"/ws/out"}) {
		t.Fatalf("a wider requested scope is cut to the parent's, got %v", derived.WritePaths)
	}
}

func TestExcessNamesWhatExceedsTheGrant(t *testing.T) {
	grantable := capability.Union(
		capability.Set{Tools: []string{"read_file"}, WritePaths: []string{"/ws/ledger.txt"}},
		capability.Set{Tools: []string{"run"}, WritePaths: []string{"/ws/src"}},
	)
	if excess := capability.Excess(grantable, capability.Set{Tools: []string{"run", "read_file"}, WritePaths: []string{"/ws/src/a.c"}}); len(excess) != 0 {
		t.Fatalf("a request within capabilities and delegable authority has no excess, got %v", excess)
	}
	excess := capability.Excess(grantable, capability.Set{Tools: []string{"web_fetch"}, WritePaths: []string{"/ws"}, AllowNetwork: true})
	if strings.Join(excess, "; ") != "tool web_fetch; write path /ws; network" {
		t.Fatalf("unexpected excess %v", excess)
	}
}

func TestAttenuateMounts(t *testing.T) {
	parent := []capability.Mount{{Name: "ws", HostPath: "/ws"}, {Name: "ref", HostPath: "/ref", ReadOnly: true}}
	requested := []capability.Mount{
		{Name: "src", HostPath: "/ws/src"},
		{Name: "ref", HostPath: "/ref"},
		{Name: "all", HostPath: "/"},
		{Name: "etc", HostPath: "/etc"},
	}
	got := capability.AttenuateMounts(parent, requested)
	want := []capability.Mount{
		{Name: "src", HostPath: "/ws/src"},
		{Name: "ref", HostPath: "/ref", ReadOnly: true},
		{Name: "all", HostPath: "/ws"},
		{Name: "all", HostPath: "/ref", ReadOnly: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttenuateMounts = %+v, want %+v", got, want)
	}
	excess := capability.MountExcess(parent, []capability.Mount{{Name: "ref", HostPath: "/ref/data"}, {Name: "etc", HostPath: "/etc", ReadOnly: true}})
	if len(excess) != 2 || !strings.Contains(excess[0], "writable mount ref") || !strings.Contains(excess[1], "mount etc (/etc) outside") {
		t.Fatalf("unexpected mount excess %v", excess)
	}
}

// Scopes match by prefix, so a single file can be a write scope.
func TestFilePathScope(t *testing.T) {
	mounts := []capability.Mount{{Name: "ws", HostPath: "/ws"}}
	set := capability.Set{WritePaths: []string{"/ws/ledger.txt"}}
	evaluator := capability.Evaluator{}
	if !evaluator.CanWrite("/ws/ledger.txt", mounts, set) {
		t.Fatal("the scoped file itself should be writable")
	}
	for _, path := range []string{"/ws/src/main.cpp", "/ws/ledger.txt.bak", "/ws"} {
		if evaluator.CanWrite(path, mounts, set) {
			t.Fatalf("%s should not be writable under a single-file scope", path)
		}
	}
}

// unconfined_run, the network allowlist and the run timeout bound attenuate
// like the rest of a set: a child gets them only as far as its parent may
// grant them.
func TestRunAndNetworkFieldsAttenuate(t *testing.T) {
	parent := capability.Set{Tools: []string{"run"}, UnconfinedRun: true, AllowNetwork: true, NetworkAllow: []string{"10.0.0.0/8", "localhost"}, MaxRunSeconds: 900}
	derived := capability.DeriveChildSet(parent, capability.Set{Tools: []string{"run"}, UnconfinedRun: true, AllowNetwork: true, NetworkAllow: []string{"localhost", "192.168.1.5"}, MaxRunSeconds: 3600})
	if !derived.UnconfinedRun || !reflect.DeepEqual(derived.NetworkAllow, []string{"localhost"}) || derived.MaxRunSeconds != 900 {
		t.Fatalf("derived = %+v", derived)
	}
	narrow := capability.DeriveChildSet(capability.Set{Tools: []string{"run"}}, capability.Set{Tools: []string{"run"}, UnconfinedRun: true})
	if narrow.UnconfinedRun || narrow.MaxRunSeconds != 0 || narrow.RunTimeoutBound().Seconds() != capability.DefaultMaxRunSeconds {
		t.Fatalf("a parent without unconfined_run cannot pass it on: %+v", narrow)
	}
	excess := capability.Excess(capability.Set{Tools: []string{"run"}}, capability.Set{Tools: []string{"run"}, UnconfinedRun: true, NetworkAllow: []string{"localhost"}, MaxRunSeconds: 1200})
	if strings.Join(excess, "; ") != "network allow localhost; unconfined_run; max_run_seconds 1200" {
		t.Fatalf("excess = %v", excess)
	}
	if excess := capability.Excess(parent, capability.Set{UnconfinedRun: true, MaxRunSeconds: 60}); len(excess) != 0 {
		t.Fatalf("within the grant: %v", excess)
	}
}

func TestRunScopes(t *testing.T) {
	mounts := []capability.Mount{{Name: "ws", HostPath: "/ws"}, {Name: "ref", HostPath: "/ref", ReadOnly: true}}
	set := capability.Set{ReadPaths: []string{"/", "/elsewhere"}, WritePaths: []string{"/ws/out", "/ref"}}
	read, write, readOnly := capability.RunScopes(set, mounts)
	if !reflect.DeepEqual(read, []string{"/ws", "/ref"}) || !reflect.DeepEqual(write, []string{"/ws/out"}) || !reflect.DeepEqual(readOnly, []string{"/ref"}) {
		t.Fatalf("read %v write %v read-only %v", read, write, readOnly)
	}
}

// Budgets attenuate like cgroups: a child gets at most its parent's, a
// strict parent makes the child strict, a request for more is an excess,
// and a delegable set naming no budget grants none beyond the parent's own.
func TestBudgetsAttenuate(t *testing.T) {
	parent := capability.Set{Budget: capability.Budget{Tokens: 1000, Seconds: 600, StrictTime: true}}
	derived := capability.DeriveChildSet(parent, capability.Set{Budget: capability.Budget{Tokens: 5000, Seconds: 60}})
	if derived.Budget != (capability.Budget{Tokens: 1000, Seconds: 60, StrictTime: true}) {
		t.Fatalf("derived = %+v", derived.Budget)
	}
	if inherited := capability.DeriveChildSet(parent, capability.Set{}); inherited.Budget != parent.Budget {
		t.Fatalf("a child naming no budget inherits the parent's: %+v", inherited.Budget)
	}
	grantable := capability.Union(capability.Set{Budget: capability.Budget{Tokens: 1000}}, capability.Set{Tools: []string{"run"}})
	if grantable.Budget.Tokens != 1000 {
		t.Fatalf("a delegable set without a budget must not make the grant unlimited: %+v", grantable.Budget)
	}
	excess := capability.Excess(grantable, capability.Set{Budget: capability.Budget{Tokens: 5000}})
	if strings.Join(excess, "; ") != "budget tokens 5000 (at most 1000)" {
		t.Fatalf("excess = %v", excess)
	}
	if excess := capability.Excess(grantable, capability.Set{}); len(excess) != 0 {
		t.Fatalf("asking for no budget is inheriting, not excess: %v", excess)
	}
}

// allow_gpu is granted like the network, card by card: a child holds a card only if its
// parent can grant it, and a parent that holds one does not force it on a child.
func TestAllowGPUAttenuates(t *testing.T) {
	both := capability.Set{Tools: []string{"run"}, AllowGPU: []string{"GPU-a", "GPU-b"}}
	one := capability.Set{Tools: []string{"run"}, AllowGPU: []string{"GPU-a"}}
	other := capability.Set{Tools: []string{"run"}, AllowGPU: []string{"GPU-c"}}
	plain := capability.Set{Tools: []string{"run"}}
	if got := capability.DeriveChildSet(both, one).AllowGPU; !reflect.DeepEqual(got, []string{"GPU-a"}) {
		t.Fatalf("a child asking for one of its parent's cards gets %v", got)
	}
	if got := capability.DeriveChildSet(one, both).AllowGPU; !reflect.DeepEqual(got, []string{"GPU-a"}) {
		t.Fatalf("a child asking for more than its parent holds gets %v", got)
	}
	if got := capability.DeriveChildSet(plain, one).AllowGPU; len(got) != 0 {
		t.Fatalf("a parent without allow_gpu passed on %v", got)
	}
	if got := capability.DeriveChildSet(one, other).AllowGPU; len(got) != 0 {
		t.Fatalf("a parent passed on a card it does not hold: %v", got)
	}
	if got := capability.DeriveChildSet(one, plain).AllowGPU; len(got) != 0 {
		t.Fatalf("a child that did not ask got %v", got)
	}
	if excess := capability.Excess(plain, one); strings.Join(excess, "; ") != "allow_gpu GPU-a" {
		t.Fatalf("excess = %v", excess)
	}
	if excess := capability.Excess(one, other); strings.Join(excess, "; ") != "allow_gpu GPU-c" {
		t.Fatalf("excess = %v", excess)
	}
	if excess := capability.Excess(both, one); len(excess) != 0 {
		t.Fatalf("excess = %v", excess)
	}
	if got := capability.Union(plain, one).AllowGPU; !reflect.DeepEqual(got, []string{"GPU-a"}) {
		t.Fatalf("what a parent can grant includes what it may delegate: %v", got)
	}
	// A set without it reads and writes as it always did, so old records stay the same.
	raw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "allow_gpu") {
		t.Fatalf("marshalled: %s", raw)
	}
}
