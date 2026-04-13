package agentflow_test

import (
	"context"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/skills"
)

type repairingCompleter struct {
	raw []string
	idx int
}

func (r *repairingCompleter) CompleteJSON(_ context.Context, _ string, _ string, _ any) error {
	return nil
}

func (r *repairingCompleter) CompleteText(_ context.Context, _ string, _ string) (string, error) {
	value := r.raw[r.idx]
	if r.idx < len(r.raw)-1 {
		r.idx++
	}
	return value, nil
}

type capturingCompleter struct {
	systemPrompt string
	userPrompt   string
}

func (c *capturingCompleter) CompleteJSON(_ context.Context, systemPrompt, userPrompt string, target any) error {
	c.systemPrompt = systemPrompt
	c.userPrompt = userPrompt
	typed := target.(*runtime.GenerateResult)
	typed.Action.Type = "exit"
	typed.Action.ExitStatus = &kernel.ExitStatus{Code: 0, Reason: "captured"}
	return nil
}

func TestPromptBackendRepairsMalformedActionOutput(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{
				`action: delegate to writer`,
				`{"action":{"type":"delegate","delegate":{"agent_id":"writer","goal":"draft it"}}}`,
			},
		},
		Workflow: agentflow.WorkflowSpec{
			Name:       "repair-test",
			EntryAgent: "coordinator",
			Agents: []agentflow.AgentSpec{
				{ID: "coordinator", SystemPrompt: "coordinate", AllowedAgents: []string{"writer"}},
				{ID: "writer", SystemPrompt: "write"},
			},
		},
		Agent: agentflow.AgentSpec{
			ID:            "coordinator",
			SystemPrompt:  "coordinate",
			AllowedAgents: []string{"writer"},
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "delegate" || result.Action.Delegate == nil || result.Action.Delegate.AgentID != "writer" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestPromptBackendAutoExitsWriterAfterSuccessfulWrite(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{raw: []string{`{"action":{"type":"message","message":"should not be used"}}`}},
		Workflow:  agentflow.WorkflowSpec{Name: "writer", EntryAgent: "writer"},
		Agent: agentflow.AgentSpec{
			ID:           "writer",
			SystemPrompt: "write once then exit",
			Capabilities: capabilitySet("write_file"),
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		History: []runtime.Message{
			{Role: "tool", Content: "write_file:map[path:/tmp/out.txt]"},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "exit" || result.Action.ExitStatus == nil || result.Action.ExitStatus.Reason != "write complete" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestPromptBackendFallsBackToMessageForPlainTextModelReply(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{
				"The codebase is configured to use Qwen/Qwen3.5-27B via the local vLLM endpoint.",
				"still not json",
			},
		},
		Workflow: agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "message" {
		t.Fatalf("unexpected action type %#v", result)
	}
	if result.Action.Message == "" {
		t.Fatal("expected message content")
	}
}

func TestPromptBackendAcceptsCommonToolSchemaVariants(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{
				`{"action":{"type":"tool","name":"find_files","arguments":{"root":"/workspace","pattern":"vllm"}}}`,
			},
		},
		Workflow: agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "tool" || result.Action.ToolCall == nil {
		t.Fatalf("unexpected result %#v", result)
	}
	if result.Action.ToolCall.Name != "find_files" || result.Action.ToolCall.Args["pattern"] != "vllm" {
		t.Fatalf("unexpected tool call %#v", result.Action.ToolCall)
	}
}

func TestPromptBackendAcceptsToolNameAsActionType(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{
				`{"action":{"type":"find_files","args":{"root":"/workspace","pattern":"config"}}}`,
			},
		},
		Workflow: agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "tool" || result.Action.ToolCall == nil {
		t.Fatalf("unexpected result %#v", result)
	}
	if result.Action.ToolCall.Name != "find_files" || result.Action.ToolCall.Args["pattern"] != "config" {
		t.Fatalf("unexpected tool call %#v", result.Action.ToolCall)
	}
}

func TestPromptBackendRepairsLooseEmbeddedJSON(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{
				"Reasoning first.\n</think>\n\n{\"action\":{\"type\":\"find_files\",\"args\":{\"pattern\":\"vllm\",root\":\"/workspace\"}}}",
			},
		},
		Workflow: agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "tool" || result.Action.ToolCall == nil {
		t.Fatalf("unexpected result %#v", result)
	}
	if result.Action.ToolCall.Name != "find_files" || result.Action.ToolCall.Args["root"] != "/workspace" {
		t.Fatalf("unexpected tool call %#v", result.Action.ToolCall)
	}
}

func TestPromptBackendAcceptsShorthandRunAction(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{
				`{"action":{"type":"run","command":"python3 /workspace/compute_pi_fixed.py"}}`,
			},
		},
		Workflow: agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "tool" || result.Action.ToolCall == nil {
		t.Fatalf("unexpected result %#v", result)
	}
	if result.Action.ToolCall.Name != "run" || result.Action.ToolCall.Args["command"] != "python3 /workspace/compute_pi_fixed.py" {
		t.Fatalf("unexpected tool call %#v", result.Action.ToolCall)
	}
}

func TestPromptBackendDoesNotLeakMixedReasoningAndActionAsMessage(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{
				"Let me run the fixed script.\n</think>\n\n{\"action\":{\"type\":\"run\",\"command\":\"python3 /workspace/compute_pi_fixed.py\"}}",
			},
		},
		Workflow: agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "tool" || result.Action.ToolCall == nil {
		t.Fatalf("unexpected result %#v", result)
	}
	if result.Action.ToolCall.Name != "run" {
		t.Fatalf("unexpected tool call %#v", result.Action.ToolCall)
	}
}

func TestPromptBackendIncludesLoadedSkillsInSystemPrompt(t *testing.T) {
	completer := &capturingCompleter{}
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
			Skills:       []string{"writer"},
		},
		Skills: []skills.Document{
			{Name: "writer", Path: "/tmp/skills/writer/SKILL.md", Content: "# Writer\nPrefer concise prose."},
		},
	}

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "exit" {
		t.Fatalf("unexpected result %#v", result)
	}
	if got := completer.systemPrompt; !containsAll(got, "Loaded skills:", "Skill: writer", "Prefer concise prose.") {
		t.Fatalf("expected loaded skill in system prompt, got %q", got)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

func capabilitySet(tool string) capability.Set {
	return capability.Set{Tools: []string{tool}}
}
