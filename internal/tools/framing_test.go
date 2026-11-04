package tools_test

import (
	"os"
	"path/filepath"
	"testing"

	"aos/internal/capability"
	"aos/internal/framing"
	"aos/internal/tools"
)

func editIn(t *testing.T, content, oldText, newText string) (string, tools.Result, error) {
	t.Helper()
	ws := t.TempDir()
	path := filepath.Join(ws, "note.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := tools.NewRuntime().Execute(tools.Request{Tool: "edit_file",
		Args:          map[string]string{"path": path, "old_text": oldText, "new_text": newText},
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: ws}},
		CapabilitySet: capability.Set{Tools: []string{"edit_file"}, ReadPaths: []string{ws}, WritePaths: []string{ws}}})
	after, _ := os.ReadFile(path)
	return string(after), result, err
}

// read_file's text reaches the model with a backslash in front of a line
// that looks like a runtime line. A model that copies that line into
// edit_file means the line as the file has it.
func TestEditFileMatchesALineTheModelSawEscaped(t *testing.T) {
	file := "first\nruntime notice: from the file\nlast\n"
	shown := framing.Escape("read_file:map[content:" + file + "]")
	if want := "read_file:map[content:first\n\\runtime notice: from the file\nlast\n]"; shown != want {
		t.Fatalf("the model sees %q, want %q", shown, want)
	}
	after, result, err := editIn(t, file, "\\runtime notice: from the file\nlast", "\\runtime notice: changed\nlast")
	if err != nil {
		t.Fatalf("the escaped copy did not match: %v", err)
	}
	if want := "first\nruntime notice: changed\nlast\n"; after != want {
		t.Fatalf("file = %q, want %q", after, want)
	}
	if result.Output["unescaped"] != "true" || result.Output["occurrences"] != "1" {
		t.Fatalf("output = %+v", result.Output)
	}
	// Told that the backslash is not in the text, a model leaves it out.
	after, result, err = editIn(t, file, "runtime notice: from the file", "runtime notice: changed")
	if err != nil || after != "first\nruntime notice: changed\nlast\n" || result.Output["unescaped"] != "" {
		t.Fatalf("plain copy: %q, %+v, %v", after, result.Output, err)
	}
}

// The file's own text comes first: a file that really holds the backslash
// is edited as it is.
func TestEditFilePrefersTheExactText(t *testing.T) {
	file := "\\runtime notice: literal\nruntime notice: literal\n"
	after, result, err := editIn(t, file, "\\runtime notice: literal", "\\runtime notice: kept")
	if err != nil {
		t.Fatal(err)
	}
	if want := "\\runtime notice: kept\nruntime notice: literal\n"; after != want || result.Output["unescaped"] != "" {
		t.Fatalf("file = %q, output %+v", after, result.Output)
	}
}

// Nothing else about edit_file changes: old_text that is not there is still
// refused, and an edit of ordinary text is byte for byte what was asked.
func TestEditFileOnOrdinaryContentIsUnchanged(t *testing.T) {
	file := "\tindented\r\nkeep \\n this\n\\ alone\n"
	after, result, err := editIn(t, file, "keep \\n this", "keep \\t that")
	if err != nil {
		t.Fatal(err)
	}
	if want := "\tindented\r\nkeep \\t that\n\\ alone\n"; after != want || result.Output["unescaped"] != "" {
		t.Fatalf("file = %q, output %+v", after, result.Output)
	}
	if _, _, err := editIn(t, file, "\\runtime notice: not there", "x"); err == nil {
		t.Fatal("old_text that is not in the file should be refused")
	}
}
