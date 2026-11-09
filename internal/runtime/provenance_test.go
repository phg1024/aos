package runtime_test

import (
	"os"
	"path/filepath"
	"testing"

	"aos/internal/runtime"
)

// RanFiles hashes each existing file a command names, by absolute path or
// relative to the run's directory, with quotes and redirections taken off,
// but the published one.
func TestRanFilesHashesWhatACommandNames(t *testing.T) {
	dir := t.TempDir()
	script, data, out := filepath.Join(dir, "bench.sh"), filepath.Join(dir, "in.txt"), filepath.Join(dir, "out.txt")
	for _, path := range []string{script, data, out} {
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ran := runtime.RanFiles("sh '"+script+"' <in.txt >"+out+" 2>&1 && cat "+filepath.Join(dir, "missing"), out, dir)
	if len(ran) != 2 || len(ran[script]) != 64 || len(ran[data]) != 64 {
		t.Fatalf("ran %v", ran)
	}
	if back := runtime.ParseRan(script + "=" + ran[script] + "\n" + data + "=" + ran[data]); len(back) != 2 || back[script] != ran[script] {
		t.Fatalf("parsed %v", back)
	}
}
