package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runtime"
	"aos/internal/tools"
)

type scriptedBackend struct {
	steps []backendStep
	index int
}

type backendStep struct {
	result runtime.GenerateResult
	err    error
}

func (s *scriptedBackend) Generate(_ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
	if s.index >= len(s.steps) {
		return runtime.GenerateResult{
			Action: runtime.GenerateAction{
				Type: "exit",
				ExitStatus: &kernel.ExitStatus{
					Code:   0,
					Reason: "completed",
				},
			},
		}, nil
	}
	step := s.steps[s.index]
	s.index++
	return step.result, step.err
}

func TestAgentRuntimeSingleAgentFlowWithCheckpointRestore(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}

	process, err := k.Spawn(kernel.ProcessSpec{
		Role: "worker",
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			ReadPaths:  []string{workspace},
			WritePaths: []string{workspace},
			Tools:      []string{"write_file", "read_file"},
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	fake := &runtime.FakeBackend{
		Steps: []runtime.GenerateResult{
			{
				Action: runtime.GenerateAction{
					Type: "tool",
					ToolCall: &runtime.ToolCall{
						Name: "write_file",
						Args: map[string]string{
							"path":    filepath.Join(workspace, "plan.txt"),
							"content": "hello from aos",
						},
					},
				},
			},
			{
				Action: runtime.GenerateAction{
					Type: "tool",
					ToolCall: &runtime.ToolCall{
						Name: "read_file",
						Args: map[string]string{
							"path": filepath.Join(workspace, "plan.txt"),
						},
					},
				},
			},
			{
				Action: runtime.GenerateAction{
					Type: "exit",
					ExitStatus: &kernel.ExitStatus{
						Code:   0,
						Reason: "completed",
					},
				},
			},
		},
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend:   fake,
	}

	result, err := agentRuntime.Run(context.Background(), process.PID, []runtime.Message{
		{Role: "user", Content: "write a small plan file"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitStatus.Code != 0 {
		t.Fatalf("unexpected exit status %#v", result.ExitStatus)
	}

	exitStatus, err := k.Await(process.PID)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if exitStatus.Code != 0 {
		t.Fatalf("unexpected stored exit %#v", exitStatus)
	}

	session, err := agentRuntime.RestoreCheckpoint(result.CheckpointID)
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	if len(session.ToolArtifactIDs) != 2 {
		t.Fatalf("expected 2 tool artifacts, got %d", len(session.ToolArtifactIDs))
	}

	artifacts, err := artifactStore.List()
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) < 3 {
		t.Fatalf("expected at least 3 artifacts, got %d", len(artifacts))
	}
}

func TestAgentRuntimeRejectsMalformedBackendOutput(t *testing.T) {
	root := t.TempDir()

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{{}},
		},
	}

	if _, err := agentRuntime.Run(context.Background(), process.PID, nil); err != runtime.ErrMalformedBackendOutput {
		t.Fatalf("expected malformed backend error, got %v", err)
	}

	exitStatus, err := k.Await(process.PID)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if exitStatus.Reason != runtime.ErrMalformedBackendOutput.Error() {
		t.Fatalf("unexpected exit reason %q", exitStatus.Reason)
	}
}

func TestAgentRuntimeSupportsMailboxWaitAndResume(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	bus, err := ipc.New(filepath.Join(root, "bus"))
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "mailbox-agent"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Bus:       bus,
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{
				{Action: runtime.GenerateAction{Type: "wait"}},
				{
					Action: runtime.GenerateAction{
						Type:    "message",
						Message: "hello back",
					},
				},
				{
					Action: runtime.GenerateAction{
						Type: "exit",
						ExitStatus: &kernel.ExitStatus{
							Code:   0,
							Reason: "done",
						},
					},
				},
			},
		},
	}

	result, err := agentRuntime.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "start"}})
	if err != runtime.ErrAgentBlocked {
		t.Fatalf("expected blocked error, got %v", err)
	}
	if !result.Blocked {
		t.Fatalf("expected blocked result %#v", result)
	}
	if _, err := bus.Send("user", process.PID, "resume"); err != nil {
		t.Fatalf("send user message: %v", err)
	}

	resumed, err := agentRuntime.Resume(context.Background(), process.PID, result.CheckpointID, nil, runtime.RunOptions{StopOnAssistantMessage: true})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed.Yielded {
		t.Fatalf("expected yielded result %#v", resumed)
	}
	if got := resumed.Session.Messages[len(resumed.Session.Messages)-1].Content; got != "hello back" {
		t.Fatalf("unexpected assistant message %q", got)
	}

	final, err := agentRuntime.Resume(context.Background(), process.PID, resumed.CheckpointID, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatalf("resume final: %v", err)
	}
	if final.ExitStatus.Code != 0 || final.ExitStatus.Reason != "done" {
		t.Fatalf("unexpected final result %#v", final)
	}
}

func TestAgentRuntimeRuntimeTools(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	bus, err := ipc.New(filepath.Join(root, "bus"))
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	leaseManager, err := lease.New(filepath.Join(root, "leases"))
	if err != nil {
		t.Fatalf("new lease manager: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{
		Role: "runtime-tools",
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			Tools: []string{"publish_artifact", "list_artifacts", "checkpoint", "message_agent", "await_agent", "acquire_lease", "release_lease", "spawn_agent"},
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	child, err := k.Spawn(kernel.ProcessSpec{ParentPID: process.PID, Role: "child"})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	if _, err := k.Exit(child.PID, kernel.ExitStatus{Code: 0, Reason: "done", Retryable: false}); err != nil {
		t.Fatalf("exit child: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Bus:       bus,
		Leases:    leaseManager,
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "publish_artifact", Args: map[string]string{"type": "note", "body": "hello"}}}},
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "list_artifacts", Args: map[string]string{"scope": "process"}}}},
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "checkpoint"}}},
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "message_agent", Args: map[string]string{"to": "proc-external", "body": "ping"}}}},
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "await_agent", Args: map[string]string{"pid": child.PID}}}},
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "acquire_lease", Args: map[string]string{"resource": "file:plan.txt", "ttl": "1m"}}}},
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "release_lease", Args: map[string]string{"resource": "file:plan.txt"}}}},
				{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "spawn_agent", Args: map[string]string{"agent_id": "writer", "goal": "Draft a note"}}}},
				{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done"}}},
			},
		},
		DelegateHandler: func(ctx context.Context, parent kernel.Process, call runtime.DelegateCall, session runtime.Session) (runtime.DelegateResult, error) {
			return runtime.DelegateResult{
				AgentID:      call.AgentID,
				PID:          "proc-child-2",
				Summary:      "delegated ok",
				ResultID:     "art-000123",
				CheckpointID: "art-000124",
			}, nil
		},
	}

	result, err := agentRuntime.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "do work"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitStatus.Code != 0 {
		t.Fatalf("unexpected exit status %#v", result.ExitStatus)
	}
	session, err := agentRuntime.RestoreCheckpoint(result.CheckpointID)
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	joined := make([]string, 0, len(session.Messages))
	for _, message := range session.Messages {
		joined = append(joined, message.Content)
	}
	all := strings.Join(joined, "\n")
	for _, needle := range []string{
		"publish_artifact:",
		"list_artifacts:",
		"checkpoint:",
		"message_agent:",
		"await_agent:",
		"acquire_lease:",
		"release_lease:",
		"spawn_agent:",
	} {
		if !strings.Contains(all, needle) {
			t.Fatalf("expected session to contain %q, got %s", needle, all)
		}
	}
	received, err := bus.ReceiveAll("proc-external")
	if err != nil {
		t.Fatalf("receive mailbox: %v", err)
	}
	if len(received) != 1 || received[0].Body != "ping" {
		t.Fatalf("unexpected mailbox messages %#v", received)
	}
}

func TestRepeatedUnhelpfulToolCallAllowsRetryAfterUsefulInterveningStep(t *testing.T) {
	attempts := []runtime.ToolAttempt{
		{Name: "run", Signature: "run|command=compile", Useful: false, Error: "compile failed"},
		{Name: "write_file", Signature: "write_file|path=/tmp/main.cpp", Useful: true},
	}
	if repeated := runtimeTestRepeatedUnhelpfulToolCall(attempts, "run|command=compile"); repeated {
		t.Fatal("expected compile retry to be allowed after a useful intervening step")
	}
}

func runtimeTestRepeatedUnhelpfulToolCall(attempts []runtime.ToolAttempt, signature string) bool {
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		if attempt.Useful {
			return false
		}
		if attempt.Signature != signature {
			continue
		}
		return !attempt.Useful
	}
	return false
}

func TestAgentRuntimeCanRecoverRetryableProcessFromCheckpoint(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "recoverable"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{
				{Action: runtime.GenerateAction{Type: "message", Message: "partial"}},
			},
		},
		MaxSteps: 1,
	}

	first, err := agentRuntime.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "start"}})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !first.ExitStatus.Retryable {
		t.Fatalf("expected retryable exit %#v", first.ExitStatus)
	}

	agentRuntime.Backend = &runtime.FakeBackend{
		Steps: []runtime.GenerateResult{
			{
				Action: runtime.GenerateAction{
					Type: "exit",
					ExitStatus: &kernel.ExitStatus{
						Code:   0,
						Reason: "recovered",
					},
				},
			},
		},
	}

	recovered, err := agentRuntime.Recover(context.Background(), process.PID, first.CheckpointID, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.ExitStatus.Reason != "recovered" {
		t.Fatalf("unexpected recovered result %#v", recovered)
	}
}

func TestAgentRuntimeCanContinueAfterToolError(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{
		Role: "interactive",
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			ReadPaths: []string{workspace},
			Tools:     []string{"read_file"},
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{
				{
					Action: runtime.GenerateAction{
						Type: "tool",
						ToolCall: &runtime.ToolCall{
							Name: "read_file",
							Args: map[string]string{"path": filepath.Join(workspace, "missing.txt")},
						},
					},
				},
				{
					Action: runtime.GenerateAction{
						Type:    "message",
						Message: "I could not read that file, but I can keep helping.",
					},
				},
				{
					Action: runtime.GenerateAction{
						Type: "exit",
						ExitStatus: &kernel.ExitStatus{
							Code:   0,
							Reason: "done",
						},
					},
				},
			},
		},
	}

	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{
		StopOnAssistantMessage: true,
		ContinueOnToolError:    true,
	})
	if err != nil {
		t.Fatalf("run with tool recovery: %v", err)
	}
	if !result.Yielded {
		t.Fatalf("expected yielded result %#v", result)
	}
	if got := result.Session.Messages[len(result.Session.Messages)-1].Content; got != "I could not read that file, but I can keep helping." {
		t.Fatalf("unexpected assistant message %q", got)
	}
	if len(result.Session.ToolArtifactIDs) != 1 {
		t.Fatalf("expected one tool artifact for error, got %d", len(result.Session.ToolArtifactIDs))
	}
}

func TestAgentRuntimeCanContinueAfterBackendErrorWhenAllowed(t *testing.T) {
	root := t.TempDir()

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "interactive"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &scriptedBackend{
			steps: []backendStep{
				{err: errors.New("unable to repair malformed action JSON")},
				{
					result: runtime.GenerateResult{
						Action: runtime.GenerateAction{
							Type:    "message",
							Message: "retrying with a different approach",
						},
					},
				},
				{
					result: runtime.GenerateResult{
						Action: runtime.GenerateAction{
							Type: "exit",
							ExitStatus: &kernel.ExitStatus{
								Code:      0,
								Reason:    "done",
								Retryable: false,
							},
						},
					},
				},
			},
		},
	}

	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{
		ContinueOnToolError: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitStatus.Reason != "done" {
		t.Fatalf("unexpected result %#v", result)
	}
	if len(result.Session.Messages) == 0 || !strings.Contains(result.Session.Messages[0].Content, "tool_error name=backend") {
		t.Fatalf("expected backend error message in session, got %#v", result.Session.Messages)
	}
}

func TestAgentRuntimeCanContinueAfterMalformedActionWhenAllowed(t *testing.T) {
	root := t.TempDir()

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "interactive"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &scriptedBackend{
			steps: []backendStep{
				{
					result: runtime.GenerateResult{
						Action: runtime.GenerateAction{
							Type: "tool",
						},
					},
				},
				{
					result: runtime.GenerateResult{
						Action: runtime.GenerateAction{
							Type:    "message",
							Message: "recovered from malformed action",
						},
					},
				},
				{
					result: runtime.GenerateResult{
						Action: runtime.GenerateAction{
							Type: "exit",
							ExitStatus: &kernel.ExitStatus{
								Code:      0,
								Reason:    "done",
								Retryable: false,
							},
						},
					},
				},
			},
		},
	}

	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{
		ContinueOnToolError: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitStatus.Reason != "done" {
		t.Fatalf("unexpected result %#v", result)
	}
	found := false
	for _, message := range result.Session.Messages {
		if strings.Contains(message.Content, "tool_error name=backend error=malformed_backend_output") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected malformed backend tool error in session, got %#v", result.Session.Messages)
	}
}

func TestAgentRuntimeAddsToolNoticeForUnhelpfulResult(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{
		Role: "interactive",
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			ReadPaths: []string{workspace},
			Tools:     []string{"find_files"},
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{
				{
					Action: runtime.GenerateAction{
						Type: "tool",
						ToolCall: &runtime.ToolCall{
							Name: "find_files",
							Args: map[string]string{"root": workspace, "pattern": "missing"},
						},
					},
				},
				{
					Action: runtime.GenerateAction{
						Type: "exit",
						ExitStatus: &kernel.ExitStatus{
							Code:   0,
							Reason: "done",
						},
					},
				},
			},
		},
	}

	result, err := agentRuntime.Run(context.Background(), process.PID, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	foundNotice := false
	for _, message := range result.Session.Messages {
		if strings.Contains(message.Content, "tool_notice name=find_files useful=false reason=no_matches") {
			foundNotice = true
			break
		}
	}
	if !foundNotice {
		t.Fatalf("expected tool notice in session %#v", result.Session.Messages)
	}
}

func TestAgentRuntimeInvokesToolCallbacks(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{
		Role: "interactive",
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			WritePaths: []string{workspace},
			Tools:      []string{"write_file"},
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	var called runtime.ToolCall
	var got tools.Result
	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{
				{
					Action: runtime.GenerateAction{
						Type: "tool",
						ToolCall: &runtime.ToolCall{
							Name: "write_file",
							Args: map[string]string{
								"path":    filepath.Join(workspace, "note.txt"),
								"content": "hello",
							},
						},
					},
				},
				{
					Action: runtime.GenerateAction{
						Type:       "exit",
						ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done"},
					},
				},
			},
		},
	}

	_, err = agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{
		OnToolCall: func(call runtime.ToolCall) {
			called = call
		},
		OnToolResult: func(result tools.Result) {
			got = result
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if called.Name != "write_file" {
		t.Fatalf("unexpected tool call %#v", called)
	}
	if got.Tool != "write_file" {
		t.Fatalf("unexpected tool result %#v", got)
	}
}

func TestAgentRuntimeSuppressesRepeatedUnhelpfulToolCall(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	artifactStore, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{
		Role: "interactive",
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			ReadPaths: []string{workspace},
			Tools:     []string{"find_files"},
		},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: artifactStore,
		Tools:     tools.NewRuntime(),
		Backend: &runtime.FakeBackend{
			Steps: []runtime.GenerateResult{
				{
					Action: runtime.GenerateAction{
						Type: "tool",
						ToolCall: &runtime.ToolCall{
							Name: "find_files",
							Args: map[string]string{"root": workspace, "pattern": "missing"},
						},
					},
				},
				{
					Action: runtime.GenerateAction{
						Type: "tool",
						ToolCall: &runtime.ToolCall{
							Name: "find_files",
							Args: map[string]string{"root": workspace, "pattern": "missing"},
						},
					},
				},
				{
					Action: runtime.GenerateAction{
						Type:       "exit",
						ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done"},
					},
				},
			},
		},
	}

	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{
		ContinueOnToolError: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	foundSuppression := false
	for _, message := range result.Session.Messages {
		if strings.Contains(message.Content, "repeated unsuccessful tool call suppressed") {
			foundSuppression = true
			break
		}
	}
	if !foundSuppression {
		t.Fatalf("expected repeated-call suppression in session %#v", result.Session.Messages)
	}
}
