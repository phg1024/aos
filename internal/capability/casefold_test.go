package capability_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"aos/internal/capability"
)

// caseInsensitive reports whether dir's volume opens a path in any case,
// as macOS's default volume does.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// The evaluator judges a path by what it names: a case variant of a
// read-only mount nested in a writable one is not writable, and a case
// variant of a granted scope or mount is granted.
func TestEvaluatorComparesWhatPathsName(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if !caseInsensitive(t, root) {
		t.Skip("the temporary volume is case-sensitive")
	}
	ws, ref := filepath.Join(root, "ws"), filepath.Join(root, "ws", "ref")
	os.MkdirAll(ref, 0o755)
	mounts := []capability.Mount{{Name: "ws", HostPath: ws}, {Name: "ref", HostPath: ref, ReadOnly: true}}
	set := capability.Set{ReadPaths: []string{ws}, WritePaths: []string{ws}}
	var evaluator capability.Evaluator
	for _, path := range []string{filepath.Join(ref, "x"), filepath.Join(ws, "REF", "x"), filepath.Join(ws, "Ref")} {
		if evaluator.CanWrite(path, mounts, set) || !capability.InReadOnlyMount(path, mounts) {
			t.Errorf("%s should be in the read-only mount", path)
		}
	}
	if !evaluator.CanWrite(filepath.Join(root, "WS", "note.txt"), mounts, set) {
		t.Error("a case variant of the writable mount should be writable")
	}
	if evaluator.CanRead(filepath.Join(root, "other"), mounts, set) {
		t.Error("a path outside every mount should not be readable")
	}
}

// Attenuation compares where paths really point. A child's case variant
// of a parent's read-only mount stays read-only and is an excess when
// asked for writable; a scope or mount that is a symlink out of the
// parent's is an excess and is dropped, not granted.
func TestAttenuationComparesWhatPathsName(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	ws, ref, outside := filepath.Join(root, "ws"), filepath.Join(root, "ws", "ref"), filepath.Join(root, "outside")
	os.MkdirAll(ref, 0o755)
	os.MkdirAll(outside, 0o755)
	link := filepath.Join(ws, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	parentMounts := []capability.Mount{{Name: "ws", HostPath: ws}, {Name: "ref", HostPath: ref, ReadOnly: true}}

	if excess := capability.Excess(capability.Set{WritePaths: []string{ws}}, capability.Set{WritePaths: []string{filepath.Join(link, "x")}}); len(excess) != 1 {
		t.Errorf("a scope through a symlink out of the grant should be an excess: %v", excess)
	}
	if derived := capability.DeriveChildSet(capability.Set{ReadPaths: []string{ws}}, capability.Set{ReadPaths: []string{link, filepath.Join(ws, "ref")}}); !reflect.DeepEqual(derived.ReadPaths, []string{filepath.Join(ws, "ref")}) {
		t.Errorf("a scope through a symlink out of the parent's should be dropped: %v", derived.ReadPaths)
	}
	if mounts := capability.AttenuateMounts(parentMounts, []capability.Mount{{Name: "out", HostPath: link}}); len(mounts) != 0 {
		t.Errorf("a mount through a symlink out of the parent's should be dropped: %+v", mounts)
	}
	if excess := capability.MountExcess(parentMounts, []capability.Mount{{Name: "out", HostPath: link}}); len(excess) != 1 {
		t.Errorf("a mount through a symlink out of the parent's should be an excess: %v", excess)
	}

	if !caseInsensitive(t, root) {
		t.Skip("the temporary volume is case-sensitive")
	}
	for _, spelling := range []string{"ref", "REF", "Ref"} {
		requested := []capability.Mount{{Name: "ref", HostPath: filepath.Join(ws, spelling)}}
		if mounts := capability.AttenuateMounts(parentMounts, requested); len(mounts) != 1 || !mounts[0].ReadOnly {
			t.Errorf("%s: a child's mount of the read-only mount should stay read-only: %+v", spelling, mounts)
		}
		if excess := capability.MountExcess(parentMounts, requested); len(excess) != 1 {
			t.Errorf("%s: a writable child mount of the read-only mount should be an excess: %v", spelling, excess)
		}
	}
	upper := filepath.Join(root, "WS", "src")
	if excess := capability.Excess(capability.Set{ReadPaths: []string{ws}}, capability.Set{ReadPaths: []string{upper}}); len(excess) != 0 {
		t.Errorf("a case variant inside the grant is not an excess: %v", excess)
	}
}
