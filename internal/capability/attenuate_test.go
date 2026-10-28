package capability_test

import (
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
