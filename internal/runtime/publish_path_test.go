package runtime_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// publishFixture runs one process through the given actions, with dir readable and
// the given tools, and returns its store and kernel.
func publishFixture(t *testing.T, dir string, toolNames []string, actions ...runtime.GenerateResult) (*artifact.Store, *kernel.Kernel, string) {
	t.Helper()
	k, store, _ := newCancelFixture(t)
	process, err := k.Spawn(kernel.ProcessSpec{
		Role:          "worker",
		Mounts:        []capability.Mount{{Name: "work", HostPath: dir}},
		CapabilitySet: capability.Set{Tools: toolNames, ReadPaths: []string{dir}},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	toolRuntime := tools.NewRuntime()
	toolRuntime.Protected = []string{filepath.Join(dir, "state")}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: toolRuntime, MaxSteps: len(actions) + 1, Backend: scripted(actions...)}
	if _, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	return store, k, process.PID
}

func recordsOfType(t *testing.T, store *artifact.Store, match func(string) bool) []artifact.Artifact {
	t.Helper()
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []artifact.Artifact
	for _, item := range items {
		if match(item.Type) {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func isPublished(kind string) bool { return strings.HasPrefix(kind, artifact.PublishedPrefix) }

func toolOutputs(t *testing.T, store *artifact.Store, tool string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, record := range recordsOfType(t, store, func(kind string) bool { return kind == "tool_result" }) {
		var result tools.Result
		if err := store.ReadJSON(record.ID, &result); err != nil {
			t.Fatal(err)
		}
		if result.Tool == tool {
			out = append(out, result.Output)
		}
	}
	return out
}

// A file's bytes reach a reader as they were, past a page, and the reader is told they
// came from a file and which hash they have. A typed body is told as typed.
func TestPublishByPathKeepsTheFilesBytes(t *testing.T) {
	dir := t.TempDir()
	// Over a page, and with what JSON or a page cut could change.
	content := strings.Repeat("trial <1> & \"é\" render_ms_best=2.64186\n", 2000)
	path := filepath.Join(dir, "raw_record.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(sum[:])
	k, store, _ := newCancelFixture(t)
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker", Mounts: []capability.Mount{{Name: "work", HostPath: dir}},
		CapabilitySet: capability.Set{Tools: []string{"publish_artifact", "read_file"}, ReadPaths: []string{dir}}})
	if err != nil {
		t.Fatal(err)
	}
	var fileID, noteID string
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		last := ""
		for i := len(request.History) - 1; i >= 0 && last == ""; i-- {
			if strings.Contains(request.History[i].Content, "artifact_id:") {
				last = request.History[i].Content
			}
		}
		switch call {
		case 1:
			return toolAction("publish_artifact", map[string]string{"type": "raw", "path": path}), nil
		case 2:
			fileID = artifactIn(last)
			return toolAction("publish_artifact", map[string]string{"type": "note", "body": "typed words"}), nil
		case 3:
			noteID = artifactIn(last)
			return toolAction("read_artifact", map[string]string{"id": noteID}), nil
		case 4:
			return toolAction("read_artifact", map[string]string{"id": fileID}), nil
		}
		if offset := after(last, "next_offset:"); offset != "" {
			return toolAction("read_artifact", map[string]string{"id": fileID, "offset": offset}), nil
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "read"}}}, nil
	}}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 12, Backend: backend}
	if _, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	published := recordsOfType(t, store, isPublished)
	if len(published) != 2 {
		t.Fatalf("published = %+v", published)
	}
	if published[0].Metadata["source"] != "file" || published[0].Metadata["source_sha256"] != want || published[0].Metadata["source_path"] != path {
		t.Fatalf("file metadata = %v", published[0].Metadata)
	}
	if published[1].Metadata["source"] != "" {
		t.Fatalf("typed metadata = %v", published[1].Metadata)
	}
	var payload struct {
		Body string `json:"body"`
	}
	if err := store.ReadJSON(fileID, &payload); err != nil || payload.Body != content {
		t.Fatalf("stored body differs: %v", err)
	}
	outputs := toolOutputs(t, store, "publish_artifact")
	if outputs[0]["source"] != "file" || outputs[0]["source_sha256"] != want {
		t.Fatalf("publish by path output = %v", outputs[0])
	}
	if _, ok := outputs[1]["source"]; ok {
		t.Fatalf("a typed publish's output changed: %v", outputs[1])
	}
	reads := toolOutputs(t, store, "read_artifact")
	if len(reads) < 3 || reads[0]["source"] != "typed" || reads[0]["source_path"] != "" || reads[0]["body"] != "typed words" {
		t.Fatalf("%d reads, the typed note read as %v", len(reads), reads[0])
	}
	var got strings.Builder
	for _, output := range reads[1:] {
		if output["source"] != "file" || output["source_sha256"] != want || output["source_path"] != path {
			t.Fatalf("read output = source %q %q %q", output["source"], output["source_path"], output["source_sha256"])
		}
		got.WriteString(output["body"])
	}
	if got.String() != content {
		t.Fatalf("%d bytes of %d", got.Len(), len(content))
	}
}

func artifactIn(text string) string {
	at := strings.Index(text, "art-")
	if at < 0 {
		return ""
	}
	return text[at : at+len("art-000000")]
}

func after(text, marker string) string {
	at := strings.LastIndex(text, marker)
	if at < 0 {
		return ""
	}
	rest := text[at+len(marker):]
	return rest[:strings.IndexFunc(rest+" ", func(r rune) bool { return r < '0' || r > '9' })]
}

// Each refusal stores nothing, and a path this process could not read_file is logged
// as the denial it is.
func TestPublishByPathRefuses(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write := func(root, name string, content []byte) string {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write(dir, "good.txt", []byte("fine"))
	secret := write(outside, "secret.txt", []byte("hunter2"))
	state := write(dir, "state/kernel.json", []byte("{}"))
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	both := []string{"publish_artifact", "read_file"}
	for _, test := range []struct {
		name     string
		tools    []string
		args     map[string]string
		wants    string
		denied   string
		deniedBy string
	}{
		{name: "both", tools: both, args: map[string]string{"path": good, "body": "typed"}, wants: "not both"},
		{name: "neither", tools: both, args: map[string]string{}, wants: "requires body or path"},
		{name: "outside the scopes", tools: both, args: map[string]string{"path": secret}, wants: "outside", denied: tools.DeniedOutside, deniedBy: "read_file"},
		{name: "a link out", tools: both, args: map[string]string{"path": link}, wants: "outside", denied: tools.DeniedOutside, deniedBy: "read_file"},
		{name: "the state directory", tools: both, args: map[string]string{"path": state}, wants: "protected", denied: tools.DeniedProtected, deniedBy: "read_file"},
		{name: "no read_file", tools: []string{"publish_artifact"}, args: map[string]string{"path": good}, wants: "needs read_file", denied: "capability not granted", deniedBy: "read_file"},
		{name: "a directory", tools: both, args: map[string]string{"path": dir}, wants: "not a regular file"},
		{name: "a FIFO", tools: both, args: map[string]string{"path": fifo}, wants: "not a regular file"},
		{name: "not UTF-8", tools: both, args: map[string]string{"path": write(dir, "binary", []byte{0xff, 0xfe, 0x00})}, wants: "not UTF-8"},
		{name: "over the cap", tools: both, args: map[string]string{"path": write(dir, "big", make([]byte, tools.MaxSourceBytes+1))}, wants: "over"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, k, pid := publishFixture(t, dir, test.tools, toolAction("publish_artifact", withType(test.args)))
			if published := recordsOfType(t, store, isPublished); len(published) != 0 {
				t.Fatalf("stored %+v", published)
			}
			failures := recordsOfType(t, store, func(kind string) bool { return kind == "tool_error" })
			if len(failures) != 1 {
				t.Fatalf("tool errors = %+v", failures)
			}
			var failure struct {
				Error string `json:"error"`
			}
			if err := store.ReadJSON(failures[0].ID, &failure); err != nil || !strings.Contains(failure.Error, test.wants) {
				t.Fatalf("error = %q, %v", failure.Error, err)
			}
			if strings.Contains(failure.Error, "hunter2") {
				t.Fatalf("the error holds the file: %q", failure.Error)
			}
			events, err := k.Events()
			if err != nil {
				t.Fatal(err)
			}
			var seen []map[string]string
			for _, event := range events {
				if event.Type == "capability.denied" && event.ProcessID == pid {
					seen = append(seen, event.Data)
				}
			}
			if test.denied == "" {
				if len(seen) != 0 {
					t.Fatalf("denials = %v", seen)
				}
				return
			}
			if len(seen) != 1 || seen[0]["reason"] != test.denied || seen[0]["capability"] != test.deniedBy || seen[0]["action"] != "publish_artifact" || seen[0]["path"] == "" {
				t.Fatalf("denials = %v", seen)
			}
		})
	}
}

// The cap is on the file, so the largest allowed goes through.
func TestPublishByPathAtTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "full")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", tools.MaxSourceBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	store, _, _ := publishFixture(t, dir, []string{"publish_artifact", "read_file"}, toolAction("publish_artifact", map[string]string{"type": "raw", "path": path}))
	if published := recordsOfType(t, store, isPublished); len(published) != 1 {
		t.Fatalf("published = %+v", published)
	}
}

func withType(args map[string]string) map[string]string {
	out := map[string]string{"type": "raw"}
	for key, value := range args {
		out[key] = value
	}
	return out
}
