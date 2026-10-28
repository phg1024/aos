package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/statedir"
)

// A session is one TUI state directory: its kernel holds the interactive
// agent's process, and its artifacts the checkpoints of the conversation.
// /resume lists the sessions in the sessions directory and switches to one
// by starting the TUI again on it.

type sessionInfo struct {
	Dir     string
	Name    string
	AgentID string
	Updated time.Time
	Turns   int
	Preview string
	InUse   bool
}

// listSessions returns the sessions under dir, newest first: directories
// whose kernel has an interactive process (a top-level agent).
func listSessions(dir string) ([]sessionInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var sessions []sessionInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		root, ok := interactiveRoot(path, "")
		if !ok {
			continue
		}
		info := sessionInfo{Dir: path, Name: entry.Name(), AgentID: root.Metadata["agent_id"], Updated: root.UpdatedAt}
		if stat, err := os.Stat(filepath.Join(path, "kernel", "events.json")); err == nil {
			info.Updated = stat.ModTime()
		}
		info.InUse, _ = statedir.InUse(path)
		if session, _, err := latestSession(path, root.PID); err == nil {
			for _, turn := range conversation(session) {
				if turn.user {
					if info.Turns == 0 {
						info.Preview = turn.text
					}
					info.Turns++
				}
			}
		}
		sessions = append(sessions, info)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Updated.After(sessions[j].Updated) })
	return sessions, nil
}

// interactiveRoot is the newest top-level agent process in the session,
// for agentID if given. It reads the process table without locking or
// repairing, so listing never writes to another session.
func interactiveRoot(dir, agentID string) (kernel.Process, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "kernel", "processes.json"))
	if err != nil {
		return kernel.Process{}, false
	}
	var processes map[string]kernel.Process
	if json.Unmarshal(raw, &processes) != nil {
		return kernel.Process{}, false
	}
	var best kernel.Process
	found := false
	for _, process := range processes {
		if process.ParentPID != "" || !strings.HasPrefix(process.Role, "agent:") {
			continue
		}
		if agentID != "" && process.Metadata["agent_id"] != agentID {
			continue
		}
		if !found || pidNumber(process.PID) > pidNumber(best.PID) {
			best, found = process, true
		}
	}
	return best, found
}

func pidNumber(pid string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(pid, "pid-"))
	return n
}

// latestSession reads pid's newest checkpoint in dir.
func latestSession(dir, pid string) (runtime.Session, string, error) {
	checkpoints, err := statedir.LatestCheckpoints(dir)
	if err != nil {
		return runtime.Session{}, "", err
	}
	id := checkpoints[pid]
	if id == "" {
		return runtime.Session{}, "", errors.New("no checkpoint yet")
	}
	store, err := artifact.New(filepath.Join(dir, "artifacts"))
	if err != nil {
		return runtime.Session{}, "", err
	}
	var session runtime.Session
	if err := store.ReadJSON(id, &session); err != nil {
		return runtime.Session{}, "", err
	}
	return session, id, nil
}

type turn struct {
	user bool
	text string
}

// conversation is what the user and the agent said, without tool traffic,
// action records or notices.
func conversation(session runtime.Session) []turn {
	var turns []turn
	for _, message := range session.Messages {
		text := strings.TrimSpace(strings.TrimPrefix(message.Content, interruptedPrefix))
		switch {
		case text == "":
		case message.Role == "user":
			turns = append(turns, turn{user: true, text: text})
		case message.Role == "assistant" && !runtime.IsActionRecord(text):
			turns = append(turns, turn{text: text})
		}
	}
	return turns
}

// resumePoint decides how the TUI continues the session in its state
// directory: the interactive process to use, the checkpoint to resume it
// from, and a note for the user. A process left running by a TUI that was
// killed is parked again. A process whose exit is final cannot continue,
// so a new one starts with its conversation (seed).
func resumePoint(k *kernel.Kernel, stateDir, agentID string) (process kernel.Process, checkpointID string, session runtime.Session, seed []runtime.Message, note string, ok bool) {
	root, found := interactiveRoot(stateDir, agentID)
	if !found {
		return kernel.Process{}, "", runtime.Session{}, nil, "", false
	}
	session, checkpointID, err := latestSession(stateDir, root.PID)
	if err != nil {
		// Nothing was said yet; keep using the process as it is.
		checkpointID = ""
	}
	current, err := k.Get(root.PID)
	if err != nil {
		return kernel.Process{}, "", runtime.Session{}, nil, "", false
	}
	switch {
	case current.State == kernel.StateRunning:
		if _, err := k.Transition(current.PID, kernel.StateReady); err == nil {
			current.State = kernel.StateReady
		}
		note = "the previous TUI stopped mid-turn; continuing from its last checkpoint"
	case current.State == kernel.StateExited && (checkpointID == "" || current.ExitStatus == nil || !current.ExitStatus.Retryable):
		reason := ""
		if current.ExitStatus != nil {
			reason = current.ExitStatus.Reason
		}
		note = fmt.Sprintf("the previous agent %s exited (%s); a new agent continues its conversation", current.PID, reason)
		return kernel.Process{}, "", session, session.Messages, note, true
	case current.State == kernel.StateExited:
		note = fmt.Sprintf("the previous agent %s exited (%s); your next message recovers it from its checkpoint", current.PID, current.ExitStatus.Reason)
	}
	return current, checkpointID, session, nil, note, true
}

// pickSession finds the session named by arg: its number in the listing,
// or a unique prefix of its name.
func pickSession(sessions []sessionInfo, arg string) (sessionInfo, error) {
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(sessions) {
			return sessionInfo{}, fmt.Errorf("no session %d; /resume lists %d", n, len(sessions))
		}
		return sessions[n-1], nil
	}
	var matches []sessionInfo
	for _, session := range sessions {
		if session.Name == arg {
			return session, nil
		}
		if strings.HasPrefix(session.Name, arg) {
			matches = append(matches, session)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return sessionInfo{}, fmt.Errorf("no session named %q", arg)
	}
	return sessionInfo{}, fmt.Errorf("%q matches %d sessions; give more of the name or its number", arg, len(matches))
}

// restartArgs are the TUI's arguments with the state and sessions
// directories replaced, to start it again on another session.
func restartArgs(args []string, stateDir, sessionsDir string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name := strings.TrimLeft(args[i], "-")
		flagName, _, hasValue := strings.Cut(name, "=")
		if strings.HasPrefix(args[i], "-") && (flagName == "state-dir" || flagName == "sessions-dir") {
			if !hasValue {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return append(out, "--state-dir", stateDir, "--sessions-dir", sessionsDir)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("Jan 2 15:04")
}
