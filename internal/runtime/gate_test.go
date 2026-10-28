package runtime_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func action(kind string, fields map[string]any) runtime.GenerateResult {
	result := runtime.GenerateResult{Action: runtime.GenerateAction{Type: kind}}
	switch kind {
	case "delegate":
		result.Action.Delegate = &runtime.DelegateCall{AgentID: "writer", Goal: "write"}
	case "send":
		result.Action.Send = &runtime.SendCall{To: "pid-9", Body: "hi"}
	case "publish":
		result.Action.Publish = &runtime.PublishCall{Topic: "news", Body: "hi"}
	case "subscribe":
		result.Action.Subscribe = &runtime.SubscribeCall{Topic: "news"}
	}
	return result
}

func deniedEvents(t *testing.T, k *kernel.Kernel, pid string) []kernel.Event {
	t.Helper()
	events, err := k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var denied []kernel.Event
	for _, event := range events {
		if event.Type == "capability.denied" && event.ProcessID == pid {
			denied = append(denied, event)
		}
	}
	return denied
}

// A process without spawn_agent or message_agent cannot reach delegation or
// messaging through the action forms, and each refusal is logged.
func TestActionsShareTheToolCapabilityGate(t *testing.T) {
	k, store, process := newCancelFixture(t)
	steps := []runtime.GenerateResult{action("delegate", nil), action("send", nil), action("publish", nil), action("subscribe", nil), exitResult("done")}
	backend := &funcBackend{fn: func(call int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return steps[call-1], nil
	}}
	agentRuntime := &runtime.AgentRuntime{
		Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 8,
		DelegateHandler: func(context.Context, kernel.Process, runtime.DelegateCall, runtime.Session) (runtime.DelegateResult, error) {
			t.Fatal("a process without spawn_agent reached the delegate handler")
			return runtime.DelegateResult{}, nil
		},
	}
	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var refused []string
	for _, message := range result.Session.Messages {
		if strings.HasPrefix(message.Content, "tool_error name=") {
			refused = append(refused, message.Content)
		}
	}
	want := []string{"delegate needs spawn_agent", "send needs message_agent", "publish needs message_agent", "subscribe needs message_agent"}
	if len(refused) != len(want) {
		t.Fatalf("expected %d refusals, got %v", len(want), refused)
	}
	for idx, fragment := range want {
		if !strings.Contains(refused[idx], fragment) {
			t.Fatalf("refusal %d = %q, want it to mention %q", idx, refused[idx], fragment)
		}
	}
	events := deniedEvents(t, k, process.PID)
	if len(events) != 4 || events[0].Data["action"] != "delegate" || events[0].Data["capability"] != "spawn_agent" || events[3].Data["capability"] != "message_agent" {
		t.Fatalf("unexpected capability.denied events %+v", events)
	}
}

func TestDeniedActionEndsRunWithoutContinueOnToolError(t *testing.T) {
	k, store, process := newCancelFixture(t)
	backend := &funcBackend{fn: func(_ int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return action("delegate", nil), nil
	}}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 3}
	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{})
	if err == nil || result.ExitStatus.Code != 1 || !strings.Contains(result.ExitStatus.Reason, "delegate needs spawn_agent") {
		t.Fatalf("expected the denied delegate to end the run, got %+v %v", result.ExitStatus, err)
	}
}

func TestPathDenialIsLogged(t *testing.T) {
	k, store, process := newCancelFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	steps := []runtime.GenerateResult{toolAction("write_file", map[string]string{"path": outside, "content": "x"}), exitResult("done")}
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return steps[call-1], nil
	}}
	granted, err := k.Spawn(kernel.ProcessSpec{Role: "writer", Mounts: process.Mounts, CapabilitySet: capability.Set{Tools: []string{"write_file"}, WritePaths: process.CapabilitySet.WritePaths}})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 3}
	if _, err := agentRuntime.RunWithOptions(context.Background(), granted.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	events := deniedEvents(t, k, granted.PID)
	if len(events) != 1 || events[0].Data["action"] != "write_file" || events[0].Data["path"] != outside {
		t.Fatalf("expected one path denial event, got %+v", events)
	}
}

// In an autonomous run a message is followed by a note that it was
// recorded, so the agent exits instead of resending it; an interactive run
// yields to the user instead.
func TestMessageRecordedNoteOnlyInAutonomousRuns(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		k, store, process := newCancelFixture(t)
		steps := []runtime.GenerateResult{{Action: runtime.GenerateAction{Type: "message", Message: "<html>result</html>"}}, exitResult("done")}
		backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
			return steps[call-1], nil
		}}
		agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 4}
		result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{StopOnAssistantMessage: interactive})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		messages := result.Session.Messages
		index := -1
		for i, message := range messages {
			if message.Content == "<html>result</html>" {
				index = i
			}
		}
		hasNote := index >= 0 && index+1 < len(messages) && strings.HasPrefix(messages[index+1].Content, "message_recorded ")
		if hasNote == interactive {
			t.Fatalf("interactive=%v: note present=%v, history %+v", interactive, hasNote, messages)
		}
	}
}
