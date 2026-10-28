package agentflow_test

import (
	"context"
	"errors"
	"strconv"
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

func TestPromptBackendCompactsLongHistoryInUserPrompt(t *testing.T) {
	completer := &capturingCompleter{}
	var history []runtime.Message
	for i := 0; i < 16; i++ {
		history = append(history,
			runtime.Message{Role: "user", Content: "user constraint round " + strconv.Itoa(i)},
			runtime.Message{Role: "tool", Content: "read_file:map[path:/workspace/file-" + strconv.Itoa(i) + ".txt content:example output " + strconv.Itoa(i) + "]"},
		)
	}
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	_, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{
			PID:      "pid-1",
			Metadata: map[string]string{"goal": "finish the task"},
		},
		History: history,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(completer.userPrompt, `"history_compacted": true`) {
		t.Fatalf("expected compacted history in user prompt:\n%s", completer.userPrompt)
	}
	if !strings.Contains(completer.userPrompt, `"history_summary":`) {
		t.Fatalf("expected history summary in user prompt:\n%s", completer.userPrompt)
	}
	if strings.Contains(completer.userPrompt, `"content": "user constraint round 0"`) {
		t.Fatalf("expected earliest history to be summarized instead of kept verbatim:\n%s", completer.userPrompt)
	}
	if !strings.Contains(completer.userPrompt, "Earlier tool/delegation outcomes") {
		t.Fatalf("expected summarized tool section in user prompt:\n%s", completer.userPrompt)
	}
}

func TestPromptBackendKeepsShortHistoryVerbatim(t *testing.T) {
	completer := &capturingCompleter{}
	history := []runtime.Message{
		{Role: "user", Content: "first constraint"},
		{Role: "assistant", Content: "working on it"},
		{Role: "tool", Content: "find_files:map[matches:/workspace/aos]"},
	}
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	_, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{
			PID:      "pid-1",
			Metadata: map[string]string{"goal": "finish the task"},
		},
		History: history,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(completer.userPrompt, `"history_compacted": true`) {
		t.Fatalf("did not expect short history compaction:\n%s", completer.userPrompt)
	}
	if !strings.Contains(completer.userPrompt, "first constraint") {
		t.Fatalf("expected verbatim short history in prompt:\n%s", completer.userPrompt)
	}
}

func TestPromptBackendIncludesMemoryRefsInUserPrompt(t *testing.T) {
	completer := &capturingCompleter{}
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"},
		Agent: agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: "help the user",
		},
	}

	_, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{
			PID:      "pid-1",
			Metadata: map[string]string{"goal": "finish the task"},
		},
		History: []runtime.Message{
			{Role: "user", Content: "latest instruction"},
		},
		MemoryRefs: []runtime.MemoryRef{
			{
				Path:         "/workspace/.aos-memory/pid-1-memory-0001.json",
				Summary:      "Earlier user instructions: keep the benchmark harness intact.",
				MessageCount: 18,
			},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(completer.userPrompt, `"memory_refs":`) {
		t.Fatalf("expected memory refs in user prompt:\n%s", completer.userPrompt)
	}
	if !strings.Contains(completer.userPrompt, "pid-1-memory-0001.json") {
		t.Fatalf("expected memory ref path in user prompt:\n%s", completer.userPrompt)
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

func TestPromptBackendHonorsContextCompactionPolicy(t *testing.T) {
	var history []runtime.Message
	for i := 0; i < 16; i++ {
		history = append(history,
			runtime.Message{Role: "user", Content: "user constraint round " + strconv.Itoa(i)},
			runtime.Message{Role: "tool", Content: "read_file:map[path:/workspace/file-" + strconv.Itoa(i) + ".txt content:example output " + strconv.Itoa(i) + "]"},
		)
	}
	request := runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Metadata: map[string]string{"goal": "finish the task"}},
		History: history,
	}
	agent := agentflow.AgentSpec{ID: "interactive", SystemPrompt: "help the user"}
	workflow := agentflow.WorkflowSpec{Name: "interactive", EntryAgent: "interactive"}

	completer := &capturingCompleter{}
	backend := agentflow.PromptBackend{
		Completer:  completer,
		Workflow:   workflow,
		Agent:      agent,
		Compaction: runtime.ContextCompactionPolicy(262144, 0.8),
	}
	if _, err := backend.Generate(context.Background(), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(completer.userPrompt, `"history_compacted": true`) {
		t.Fatalf("expected history to fit a 262k window verbatim:\n%s", completer.userPrompt)
	}
	if !strings.Contains(completer.userPrompt, `"content": "user constraint round 0"`) {
		t.Fatalf("expected earliest history kept verbatim:\n%s", completer.userPrompt)
	}

	completer = &capturingCompleter{}
	backend.Completer = completer
	backend.Compaction = runtime.CompactionPolicy{MaxChars: 200, KeepMessages: 2}
	if _, err := backend.Generate(context.Background(), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(completer.userPrompt, `"history_messages_omitted": 30`) {
		t.Fatalf("expected 30 omitted messages with 2 kept:\n%s", completer.userPrompt)
	}
}

func TestPromptBackendParsesNotesAndDescribesWorkingMemory(t *testing.T) {
	backend := agentflow.PromptBackend{
		Completer: &repairingCompleter{
			raw: []string{`{"action":{"type":"tool","notes":"Plan: vec3, scene, render. Done: vec3. Next: scene.","tool_call":{"name":"list_dir","args":{"path":"/workspace"}}}}`},
		},
		Workflow: agentflow.WorkflowSpec{Name: "notes-test", EntryAgent: "writer"},
		Agent:    agentflow.AgentSpec{ID: "writer", SystemPrompt: "write"},
	}
	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Notes != "Plan: vec3, scene, render. Done: vec3. Next: scene." {
		t.Fatalf("expected notes to be parsed, got %#v", result.Action)
	}

	completer := &capturingCompleter{}
	backend = agentflow.PromptBackend{Completer: completer, Workflow: backend.Workflow, Agent: backend.Agent}
	if _, err := backend.Generate(context.Background(), runtime.GenerateRequest{}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{`"notes":"..."`, "working memory", "hidden reasoning is discarded"} {
		if !strings.Contains(completer.systemPrompt, want) {
			t.Fatalf("expected system prompt to mention %q", want)
		}
	}
}

type messagesCompleter struct {
	transcripts [][]runtime.Message
}

func (m *messagesCompleter) CompleteJSON(_ context.Context, _ string, _ string, _ any) error {
	return errors.New("CompleteJSON should not be used when CompleteMessages is available")
}

func (m *messagesCompleter) CompleteMessages(_ context.Context, messages []runtime.Message) (string, error) {
	m.transcripts = append(m.transcripts, append([]runtime.Message(nil), messages...))
	return `{"action":{"type":"exit","exit_status":{"code":0,"reason":"done"}}}`, nil
}

func TestPromptBackendSendsAppendOnlyTranscript(t *testing.T) {
	completer := &messagesCompleter{}
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "transcript", EntryAgent: "writer"},
		Agent:     agentflow.AgentSpec{ID: "writer", SystemPrompt: "write"},
	}
	process := kernel.Process{PID: "pid-7", Metadata: map[string]string{"goal": "write vec3.h"}}
	step1 := []runtime.Message{
		{Role: "user", Content: "write vec3.h"},
		{Role: "assistant", Content: "notes: Plan: vec3.h. Next: write it.\naction: tool write_file content=<40 bytes> path=/w/vec3.h"},
		{Role: "tool", Content: "write_file:map[bytes:40 path:/w/vec3.h]"},
	}
	step2 := append(append([]runtime.Message(nil), step1...),
		runtime.Message{Role: "assistant", Content: "notes: Done: vec3.h. Next: exit.\naction: tool run command=g++ main.cpp"},
		runtime.Message{Role: "tool", Content: "tool_error name=run error=exit status 1 try_different_strategy=true"},
	)
	for _, history := range [][]runtime.Message{step1, step2} {
		if _, err := backend.Generate(context.Background(), runtime.GenerateRequest{Process: process, History: history}); err != nil {
			t.Fatalf("generate: %v", err)
		}
	}

	first, second := completer.transcripts[0], completer.transcripts[1]
	if len(first) != 2+len(step1) || len(second) != 2+len(step2) {
		t.Fatalf("expected system, header, and one message per history entry; got %d and %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("message %d changed between steps:\n%#v\n%#v", i, first[i], second[i])
		}
	}
	wantRoles := []string{"system", "user", "user", "assistant", "user", "assistant", "user"}
	for i, role := range wantRoles {
		if second[i].Role != role {
			t.Fatalf("message %d: expected role %s, got %s", i, role, second[i].Role)
		}
	}
	if second[6].Content != step2[4].Content {
		t.Fatalf("expected tool result content unchanged, got %q", second[6].Content)
	}
	header := second[1].Content
	if !strings.Contains(header, `"goal": "write vec3.h"`) {
		t.Fatalf("expected goal in task header:\n%s", header)
	}
	for _, perStep := range []string{"tool_errors", "last_tool_observation", `"history"`} {
		if strings.Contains(header, perStep) {
			t.Fatalf("task header must not carry per-step field %s:\n%s", perStep, header)
		}
	}
}

type flakyMessagesCompleter struct {
	transcripts [][]runtime.Message
}

func (f *flakyMessagesCompleter) CompleteJSON(_ context.Context, _ string, _ string, _ any) error {
	return errors.New("unused")
}

func (f *flakyMessagesCompleter) CompleteMessages(_ context.Context, messages []runtime.Message) (string, error) {
	f.transcripts = append(f.transcripts, append([]runtime.Message(nil), messages...))
	if len(f.transcripts) == 1 {
		return "notes: next read a file\naction: tool read_file path=/w/a", nil
	}
	return `{"action":{"type":"tool","tool_call":{"name":"read_file","args":{"path":"/w/a"}}}}`, nil
}

func TestPromptBackendRepairsInsideTheSameTranscript(t *testing.T) {
	completer := &flakyMessagesCompleter{}
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "repair", EntryAgent: "writer"},
		Agent:     agentflow.AgentSpec{ID: "writer", SystemPrompt: "write"},
	}
	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Metadata: map[string]string{"goal": "read a"}},
		History: []runtime.Message{{Role: "user", Content: "read a"}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.ToolCall == nil || result.Action.ToolCall.Name != "read_file" {
		t.Fatalf("expected repaired tool action, got %#v", result.Action)
	}
	first, repair := completer.transcripts[0], completer.transcripts[1]
	if len(repair) != len(first)+2 {
		t.Fatalf("expected repair to append two messages, got %d after %d", len(repair), len(first))
	}
	for i := range first {
		if first[i] != repair[i] {
			t.Fatalf("repair changed message %d", i)
		}
	}
	if repair[len(first)].Role != "assistant" || repair[len(first)+1].Role != "user" {
		t.Fatalf("expected the bad reply then a correction, got %#v", repair[len(first):])
	}
}

func TestPromptBackendListsToolSignatures(t *testing.T) {
	completer := &capturingCompleter{}
	backend := agentflow.PromptBackend{
		Completer: completer,
		Workflow:  agentflow.WorkflowSpec{Name: "signatures", EntryAgent: "writer"},
		Agent: agentflow.AgentSpec{ID: "writer", SystemPrompt: "write", Capabilities: capability.Set{
			Tools: []string{"edit_file", "web_fetch", "custom_tool"},
		}},
	}
	if _, err := backend.Generate(context.Background(), runtime.GenerateRequest{}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"- edit_file(path, old_text, new_text, replace_all?)\n",
		"- web_fetch(url, offset?, timeout?)\n",
		"- custom_tool\n",
		"the marker is never valid content",
	} {
		if !strings.Contains(completer.systemPrompt, want) {
			t.Fatalf("expected system prompt to contain %q:\n%s", want, completer.systemPrompt)
		}
	}
}
