package agentflow_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/tools"
)

type scriptedCompleter struct {
	calls int
}

func (s *scriptedCompleter) CompleteJSON(_ context.Context, _ string, _ string, target any) error {
	s.calls++
	var payload any
	switch s.calls {
	case 1:
		payload = map[string]any{
			"action": map[string]any{
				"type": "delegate",
				"delegate": map[string]any{
					"agent_id": "writer",
					"goal":     "Write the combined output file",
				},
			},
		}
	case 2:
		payload = map[string]any{
			"action": map[string]any{
				"type": "tool",
				"tool_call": map[string]any{
					"name": "write_file",
					"args": map[string]string{
						"path":    filepath.Join(os.TempDir(), "should-not-be-used"),
						"content": "placeholder",
					},
				},
			},
		}
	case 3:
		payload = map[string]any{
			"action": map[string]any{
				"type": "exit",
				"exit_status": map[string]any{
					"code":      0,
					"reason":    "writer-complete",
					"retryable": false,
				},
			},
		}
	default:
		payload = map[string]any{
			"action": map[string]any{
				"type": "exit",
				"exit_status": map[string]any{
					"code":      0,
					"reason":    "coordinator-complete",
					"retryable": false,
				},
			},
		}
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func TestWorkflowRunSupportsGenericDelegation(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}

	outputPath := filepath.Join(root, "workspace", "combined.txt")
	completer := &scriptedCompleter{}
	engine := agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Tools:     tools.NewRuntime(),
		Factory:   agentflow.StaticBackendFactory{Completer: completer},
	}

	spec := agentflow.WorkflowSpec{
		Name:       "generic-collab",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{
				ID:            "coordinator",
				SystemPrompt:  "Delegate work and finish when the delegated agent returns.",
				AllowedAgents: []string{"writer"},
				Capabilities: capability.Set{
					Tools: []string{},
				},
			},
			{
				ID:           "writer",
				SystemPrompt: "Write the requested file and then exit.",
				Capabilities: capability.Set{
					Tools: []string{"write_file"},
				},
			},
		},
	}

	// The scripted completer writes to the request output path by reading it from prompt context.
	engine.Factory = agentflow.StaticBackendFactory{Completer: completerWithOutput{base: completer, outputPath: outputPath}}

	summary, err := engine.Run(context.Background(), spec, agentflow.RunRequest{
		Goal:       "Combine and write the output file",
		OutputPath: outputPath,
	})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if summary.Workflow != "generic-collab" {
		t.Fatalf("unexpected workflow %q", summary.Workflow)
	}
	if summary.EntryPID == "" || summary.ResultID == "" {
		t.Fatalf("expected populated summary %#v", summary)
	}
	bytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(bytes) != "writer artifact" {
		t.Fatalf("unexpected output %q", string(bytes))
	}

	processes, err := k.Processes()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	if len(processes) < 3 {
		t.Fatalf("expected root + two agents, got %d", len(processes))
	}
	artifacts, err := store.List()
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) < 4 {
		t.Fatalf("expected workflow artifacts, got %d", len(artifacts))
	}
}

type completerWithOutput struct {
	base       *scriptedCompleter
	outputPath string
}

func (c completerWithOutput) CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error {
	if c.base.calls == 1 {
		_ = ctx
	}
	if c.base.calls == 1 {
		// no-op; handled by base
	}
	if c.base.calls == 1 {
	}
	if c.base.calls == 1 {
	}
	err := c.base.CompleteJSON(ctx, systemPrompt, userPrompt, target)
	if err != nil {
		return err
	}
	if c.base.calls == 2 {
		bytes, err := json.Marshal(map[string]any{
			"action": map[string]any{
				"type": "tool",
				"tool_call": map[string]any{
					"name": "write_file",
					"args": map[string]string{
						"path":    c.outputPath,
						"content": "writer artifact",
					},
				},
			},
		})
		if err != nil {
			return err
		}
		return json.Unmarshal(bytes, target)
	}
	return nil
}

func TestWorkflowSpecValidationRejectsUnknownDelegateTarget(t *testing.T) {
	spec := agentflow.WorkflowSpec{
		Name:       "broken",
		EntryAgent: "root",
		Agents: []agentflow.AgentSpec{
			{
				ID:            "root",
				SystemPrompt:  "delegate",
				AllowedAgents: []string{"missing"},
			},
		},
	}
	if err := spec.Validate(); err != agentflow.ErrWorkflowSpecInvalid {
		t.Fatalf("expected invalid spec, got %v", err)
	}
}
