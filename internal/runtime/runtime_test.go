package runtime_test

import (
	"context"
	"path/filepath"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

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
