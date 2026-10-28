package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tui/render"
)

// makeSession writes a session with an interactive process that said the
// given user lines (each answered), checkpointed, and returns its kernel.
func makeSession(t *testing.T, dir string, lines ...string) (*kernel.Kernel, kernel.Process) {
	t.Helper()
	k, err := kernel.New(filepath.Join(dir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	process, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:interactive", Metadata: map[string]string{"agent_id": "interactive"}})
	var session runtime.Session
	for _, line := range lines {
		session.Messages = append(session.Messages,
			runtime.Message{Role: "user", Content: line},
			runtime.Message{Role: "assistant", Content: `{"action":{"type":"tool","tool_call":{"name":"list_dir"}}}`},
			runtime.Message{Role: "tool", Content: "list_dir:..."},
			runtime.Message{Role: "assistant", Content: "answer to " + line})
	}
	if len(lines) > 0 {
		if _, err := store.CreateJSON("checkpoint", process.PID, session, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	return k, process
}

func TestListAndPickSessions(t *testing.T) {
	dir := t.TempDir()
	makeSession(t, filepath.Join(dir, "20261028-100000-strata"), "check round 8", "update the report")
	time.Sleep(20 * time.Millisecond)
	makeSession(t, filepath.Join(dir, "20261028-110000-ollama"), "hello")
	os.MkdirAll(filepath.Join(dir, "not-a-session"), 0o755)
	sessions, err := listSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Name != "20261028-110000-ollama" || sessions[1].Turns != 2 || sessions[1].Preview != "check round 8" || sessions[0].AgentID != "interactive" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if picked, err := pickSession(sessions, "2"); err != nil || picked.Name != "20261028-100000-strata" {
		t.Fatalf("by number: %+v %v", picked, err)
	}
	if picked, err := pickSession(sessions, "20261028-11"); err != nil || picked.Name != "20261028-110000-ollama" {
		t.Fatalf("by prefix: %+v %v", picked, err)
	}
	for _, bad := range []string{"3", "0", "20261028", "nope"} {
		if _, err := pickSession(sessions, bad); err == nil {
			t.Fatalf("%q should not pick a session", bad)
		}
	}
	text := sessionList(testPalette(), sessions, sessions[1].Dir, dir)
	if !strings.Contains(text, "* 2  20261028-100000-strata") || !strings.Contains(text, "2 turns") || !strings.Contains(text, "check round 8") {
		t.Fatalf("listing:\n%s", text)
	}
}

func TestRestartArgsReplaceTheStateDirectory(t *testing.T) {
	got := restartArgs([]string{"--workdir", "/w", "--state-dir", "/old", "--tools=run", "-sessions-dir=/s0", "--agent", "x"}, "/new", "/s")
	want := []string{"--workdir", "/w", "--tools=run", "--agent", "x", "--state-dir", "/new", "--sessions-dir", "/s"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restartArgs = %v", got)
	}
}

func TestResumePoint(t *testing.T) {
	dir := t.TempDir()
	k, process := makeSession(t, dir, "hello")

	// A TUI killed mid-turn left the process running: it is parked again.
	k.Transition(process.PID, kernel.StateRunning)
	got, checkpoint, session, seed, note, ok := resumePoint(k, dir, "interactive")
	if !ok || got.PID != process.PID || got.State != kernel.StateReady || checkpoint == "" || len(session.Messages) != 4 || seed != nil || !strings.Contains(note, "mid-turn") {
		t.Fatalf("running: %+v %q %d %v %q", got, checkpoint, len(session.Messages), seed, note)
	}
	// A retryable exit is kept, so the next message recovers it.
	k.Exit(process.PID, kernel.ExitStatus{Code: 1, Reason: "backend down", Retryable: true})
	got, checkpoint, _, seed, note, _ = resumePoint(k, dir, "interactive")
	if got.PID != process.PID || checkpoint == "" || seed != nil || !strings.Contains(note, "recovers") {
		t.Fatalf("retryable: %+v %q %v %q", got, checkpoint, seed, note)
	}
	// A final exit cannot continue: a new agent starts with its conversation.
	final := t.TempDir()
	k2, p2 := makeSession(t, final, "hello")
	k2.Exit(p2.PID, kernel.ExitStatus{Code: 0, Reason: "done", Origin: kernel.ExitOriginAgent})
	got, checkpoint, _, seed, note, ok = resumePoint(k2, final, "interactive")
	if !ok || got.PID != "" || checkpoint != "" || len(seed) != 4 || !strings.Contains(note, "new agent") {
		t.Fatalf("final: %+v %q %d %q", got, checkpoint, len(seed), note)
	}
	// Another agent's session is not resumed as this one.
	if _, _, _, _, _, ok := resumePoint(k, dir, "other"); ok {
		t.Fatal("a session of another agent should not resume")
	}
}

// testPalette renders without colour, as for a file that is not a terminal.
func testPalette() render.Palette { return render.Palette{} }
