package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func scripted(steps ...runtime.GenerateResult) *funcBackend {
	return &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call > len(steps) {
			return exitResult("done"), nil
		}
		return steps[call-1], nil
	}}
}

func TestPublishArtifactCannotUseReservedTypes(t *testing.T) {
	k, store, _ := newCancelFixture(t)
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker", CapabilitySet: capability.Set{Tools: []string{"publish_artifact"}}})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 4, Backend: scripted(
		toolAction("publish_artifact", map[string]string{"type": "verification", "body": "PASS"}),
		toolAction("publish_artifact", map[string]string{"type": "notes", "body": "hello"}),
	)}
	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var published []artifact.Artifact
	for _, item := range items {
		if item.ProducerPID == process.PID && (item.Type == "verification" || strings.HasPrefix(item.Type, artifact.PublishedPrefix)) {
			published = append(published, item)
		}
	}
	if len(published) != 1 || published[0].Type != "published.notes" {
		t.Fatalf("expected only published.notes, got %+v", published)
	}
	var refused bool
	for _, message := range result.Session.Messages {
		refused = refused || strings.Contains(message.Content, "reserved for the runtime")
	}
	if !refused {
		t.Fatal("the agent should see the refusal")
	}
	if events := deniedEvents(t, k, process.PID); len(events) != 1 || !strings.Contains(events[0].Data["reason"], "reserved artifact type verification") {
		t.Fatalf("expected one capability.denied event, got %+v", events)
	}
}

func TestResumeRequiresAnOwnedCheckpoint(t *testing.T) {
	k, store, owner := newCancelFixture(t)
	other, err := k.Spawn(kernel.ProcessSpec{Role: "intruder", Mounts: owner.Mounts, CapabilitySet: owner.CapabilitySet})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 4,
		Backend: scripted(toolAction("run", map[string]string{"command": "true"}), runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "ok"}})}
	result, err := agentRuntime.RunWithOptions(context.Background(), owner.PID, nil, runtime.RunOptions{StopOnAssistantMessage: true})
	if err != nil || result.CheckpointID == "" {
		t.Fatalf("run: %+v %v", result, err)
	}
	if _, err := agentRuntime.Resume(context.Background(), other.PID, result.CheckpointID, nil, runtime.RunOptions{}); !errors.Is(err, runtime.ErrCheckpointNotOwned) {
		t.Fatalf("resuming another process's checkpoint: got %v", err)
	}
	var toolResult string
	items, _ := store.List()
	for _, item := range items {
		if item.Type == "tool_result" && item.ProducerPID == owner.PID {
			toolResult = item.ID
		}
	}
	if _, err := agentRuntime.Resume(context.Background(), owner.PID, toolResult, nil, runtime.RunOptions{}); !errors.Is(err, runtime.ErrNotACheckpoint) {
		t.Fatalf("resuming from a tool_result: got %v", err)
	}
	if _, err := agentRuntime.Resume(context.Background(), owner.PID, result.CheckpointID, nil, runtime.RunOptions{}); err != nil {
		t.Fatalf("the owner may resume: %v", err)
	}
}

func TestRecoveredProcessMayUseItsPredecessorsCheckpoint(t *testing.T) {
	k, store, process := newCancelFixture(t)
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 1,
		Backend: scripted(toolAction("run", map[string]string{"command": "true"}), toolAction("run", map[string]string{"command": "true"}))}
	first, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{})
	if err != nil || first.ExitStatus.Reason != "step limit exceeded" {
		t.Fatalf("expected a retryable step-limit exit, got %+v %v", first, err)
	}
	recovered, err := agentRuntime.Recover(context.Background(), process.PID, first.CheckpointID, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.PID == process.PID {
		t.Fatal("recover should run a new process")
	}
	// The recovered process may have exited again; whatever else happens,
	// ownership of its predecessor's checkpoint must not be the objection.
	if _, err := agentRuntime.Resume(context.Background(), recovered.PID, first.CheckpointID, nil, runtime.RunOptions{}); errors.Is(err, runtime.ErrCheckpointNotOwned) {
		t.Fatalf("a recovered process owns its predecessor's checkpoints: %v", err)
	}
}

func compactingRuntime(k *kernel.Kernel, store *artifact.Store, backend runtime.Backend) *runtime.AgentRuntime {
	return &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 6, Backend: backend,
		Compaction: runtime.CompactionPolicy{MaxMessages: 6, KeepMessages: 2}}
}

func longHistory(n int) []runtime.Message {
	var messages []runtime.Message
	for i := 0; i < n; i++ {
		messages = append(messages, runtime.Message{Role: "user", Content: "older context"})
	}
	return messages
}

// Compaction needs no writable path, its segments sit outside every mount,
// and read_memory returns only the process's own history.
func TestCompactionIsRuntimeOwned(t *testing.T) {
	k, store, workspaceProcess := newCancelFixture(t)
	readOnly, err := k.Spawn(kernel.ProcessSpec{Role: "reader", CapabilitySet: capability.Set{Tools: []string{"read_file"}}})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	var memoryID string
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			if len(request.MemoryRefs) != 1 || request.MemoryRefs[0].ArtifactID == "" {
				t.Errorf("a process without write paths should still compact, refs %+v", request.MemoryRefs)
			} else {
				memoryID = request.MemoryRefs[0].ArtifactID
			}
			return toolAction("read_memory", map[string]string{"artifact_id": memoryID}), nil
		}
		return exitResult("done"), nil
	}}
	result, err := compactingRuntime(k, store, backend).RunWithOptions(context.Background(), readOnly.PID, longHistory(10), runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var read string
	for _, message := range result.Session.Messages {
		if strings.HasPrefix(message.Content, "read_memory:") {
			read = message.Content
		}
	}
	if !strings.Contains(read, "older context") {
		t.Fatalf("read_memory should return the compacted history without a grant, got %q", read)
	}

	segment, err := store.Metadata(memoryID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	_, err = tools.NewRuntime().Execute(tools.Request{Tool: "write_file", Args: map[string]string{"path": segment.Path, "content": "forged"},
		Mounts: workspaceProcess.Mounts, CapabilitySet: capability.Set{Tools: []string{"write_file"}, WritePaths: workspaceProcess.CapabilitySet.WritePaths}})
	if !errors.Is(err, tools.ErrPathDenied) {
		t.Fatalf("an agent's write_file must not reach a memory segment, got %v", err)
	}

	other, err := k.Spawn(kernel.ProcessSpec{Role: "other"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	stranger := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 2,
		Backend: scripted(toolAction("read_memory", map[string]string{"artifact_id": memoryID}))}
	snooped, err := stranger.RunWithOptions(context.Background(), other.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, message := range snooped.Session.Messages {
		if strings.Contains(message.Content, "older context") {
			t.Fatal("read_memory must not return another process's history")
		}
	}
}

func TestTwoResumesFromOneCheckpointKeepSeparateSegments(t *testing.T) {
	k, store, process := newCancelFixture(t)
	start, err := compactingRuntime(k, store, scripted(runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "paused"}})).
		RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{StopOnAssistantMessage: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var segments []string
	for i := 0; i < 2; i++ {
		backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
			if call == 1 && len(request.MemoryRefs) > 0 {
				segments = append(segments, request.MemoryRefs[len(request.MemoryRefs)-1].ArtifactID)
			}
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "again"}}, nil
		}}
		if _, err := compactingRuntime(k, store, backend).Resume(context.Background(), process.PID, start.CheckpointID, longHistory(10), runtime.RunOptions{StopOnAssistantMessage: true}); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	if len(segments) != 2 || segments[0] == segments[1] {
		t.Fatalf("two resumes should write two segments, got %v", segments)
	}
	for _, id := range segments {
		record, err := store.Metadata(id)
		if err != nil || len(record.ParentIDs) != 1 || record.ParentIDs[0] != start.CheckpointID {
			t.Fatalf("segment %s should be cut from checkpoint %s: %+v %v", id, start.CheckpointID, record, err)
		}
	}
}

// Checkpoints chain to their predecessor and tool results to the
// checkpoint they ran under, so a run's artifacts form a walkable graph.
func TestRunArtifactsFormAChain(t *testing.T) {
	k, store, process := newCancelFixture(t)
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 4,
		Backend: scripted(toolAction("run", map[string]string{"command": "echo one"}), toolAction("run", map[string]string{"command": "echo two"}))}
	if _, err := agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]artifact.Artifact{}
	var checkpoints, results []artifact.Artifact
	for _, item := range items {
		byID[item.ID] = item
		switch item.Type {
		case "checkpoint":
			checkpoints = append(checkpoints, item)
		case "tool_result":
			results = append(results, item)
		}
	}
	sortByID := func(list []artifact.Artifact) {
		for i := range list {
			for j := i + 1; j < len(list); j++ {
				if list[j].ID < list[i].ID {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
	}
	sortByID(checkpoints)
	sortByID(results)
	if len(checkpoints) < 2 || len(results) != 2 {
		t.Fatalf("expected at least two checkpoints and two tool results, got %d and %d", len(checkpoints), len(results))
	}
	for i := 1; i < len(checkpoints); i++ {
		if parents := checkpoints[i].ParentIDs; len(parents) != 1 || parents[0] != checkpoints[i-1].ID {
			t.Fatalf("checkpoint %s should chain to %s, has %v", checkpoints[i].ID, checkpoints[i-1].ID, parents)
		}
	}
	if parents := results[1].ParentIDs; len(parents) != 1 || byID[parents[0]].Type != "checkpoint" {
		t.Fatalf("the second tool result should name the checkpoint it ran under, has %v", parents)
	}
}
