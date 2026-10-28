package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/skills"
)

type JSONCompleter interface {
	CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error
}

type RawCompleter interface {
	CompleteText(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// MessagesCompleter sends a whole chat transcript. Backends that implement it
// get an append-only multi-turn prompt, which lets servers reuse the previous
// step's prompt cache instead of re-reading the session every step.
type MessagesCompleter interface {
	CompleteMessages(ctx context.Context, messages []runtime.Message) (string, error)
}

type PromptBackend struct {
	Completer JSONCompleter
	Workflow  WorkflowSpec
	Agent     AgentSpec
	Skills    []skills.Document
	// Compaction defaults to the prompt-history limits below when left zero.
	Compaction runtime.CompactionPolicy
}

var bareKeyPattern = regexp.MustCompile(`([,{]\s*)([A-Za-z_][A-Za-z0-9_]*)\s*:`)
var halfQuotedKeyPattern = regexp.MustCompile(`([,{]\s*)([A-Za-z_][A-Za-z0-9_]*)":`)
var trailingCommaPattern = regexp.MustCompile(`,\s*([}\]])`)

const (
	maxPromptHistoryMessages = 12
	maxPromptHistoryChars    = 4000
	maxSummaryItemsPerRole   = 6
)

func (b PromptBackend) Generate(ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	if result, ok := b.autoExitForCompletedTool(request); ok {
		return result, nil
	}
	if messagesCompleter, ok := b.Completer.(MessagesCompleter); ok {
		return b.generateFromMessages(ctx, messagesCompleter, request)
	}
	if rawCompleter, ok := b.Completer.(RawCompleter); ok {
		return b.generateFromRaw(ctx, rawCompleter, request)
	}
	var result runtime.GenerateResult
	if err := b.Completer.CompleteJSON(ctx, b.systemPrompt(), b.userPrompt(request), &result); err != nil {
		return runtime.GenerateResult{}, err
	}
	return result, nil
}

// PromptMessages returns the prompt as an append-only transcript: a static
// system prompt and task header, then one message per history entry. Each
// step only appends to the previous step's messages, so nothing that changes
// per step may precede the history.
func (b PromptBackend) PromptMessages(request runtime.GenerateRequest) []runtime.Message {
	compacted := compactHistory(request.History, b.compactionPolicy())
	messages := []runtime.Message{
		{Role: "system", Content: b.systemPrompt()},
		{Role: "user", Content: b.taskHeader(request, compacted)},
	}
	for _, message := range compacted.Recent {
		// Tool results go out as user turns: the OpenAI "tool" role needs a
		// tool_call_id that AOS JSON actions do not have.
		role := "user"
		if message.Role == "assistant" {
			role = "assistant"
		}
		messages = append(messages, runtime.Message{Role: role, Content: message.Content})
	}
	return messages
}

// taskHeader holds the session facts that stay fixed between compactions.
func (b PromptBackend) taskHeader(request runtime.GenerateRequest, compacted compactedHistory) string {
	type headerContext struct {
		Workflow               string              `json:"workflow"`
		AgentID                string              `json:"agent_id"`
		ProcessID              string              `json:"process_id"`
		Goal                   string              `json:"goal"`
		InputPaths             []string            `json:"input_paths,omitempty"`
		OutputPath             string              `json:"output_path,omitempty"`
		Metadata               map[string]string   `json:"metadata,omitempty"`
		ReadPaths              []string            `json:"read_paths,omitempty"`
		WritePaths             []string            `json:"write_paths,omitempty"`
		HistorySummary         string              `json:"history_summary,omitempty"`
		HistoryMessagesOmitted int                 `json:"history_messages_omitted,omitempty"`
		MemoryRefs             []runtime.MemoryRef `json:"memory_refs,omitempty"`
	}
	payload := headerContext{
		Workflow:               b.Workflow.Name,
		AgentID:                b.Agent.ID,
		ProcessID:              request.Process.PID,
		Goal:                   request.Process.Metadata["goal"],
		InputPaths:             splitList(request.Process.Metadata["input_paths"]),
		OutputPath:             request.Process.Metadata["output_path"],
		Metadata:               cloneMap(request.Process.Metadata),
		ReadPaths:              append([]string(nil), request.Process.CapabilitySet.ReadPaths...),
		WritePaths:             append([]string(nil), request.Process.CapabilitySet.WritePaths...),
		HistorySummary:         compacted.Summary,
		HistoryMessagesOmitted: compacted.Omitted,
		MemoryRefs:             append([]runtime.MemoryRef(nil), request.MemoryRefs...),
	}
	bytes, _ := json.MarshalIndent(payload, "", "  ")
	return fmt.Sprintf(
		"Here is your task context.\n\n%s\n\n"+
			"The messages that follow are your session history: assistant turns are your own earlier notes and actions, "+
			"user turns are instructions, messages, and tool results. Reply to the latest turn with the next action JSON only.",
		string(bytes),
	)
}

func (b PromptBackend) generateFromMessages(ctx context.Context, completer MessagesCompleter, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	messages := b.PromptMessages(request)
	raw, err := completer.CompleteMessages(ctx, messages)
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	if parsed, ok := parseGenerateResult(raw); ok {
		return parsed, nil
	}

	// Ask for a correction inside the same conversation, so the server can
	// resume from the transcript it just read instead of starting over.
	repaired, err := completer.CompleteMessages(runtime.AsRepair(ctx), append(append([]runtime.Message(nil), messages...),
		runtime.Message{Role: "assistant", Content: raw},
		runtime.Message{Role: "user", Content: repairInstruction},
	))
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	if parsed, ok := parseGenerateResult(repaired); ok {
		return parsed, nil
	}
	if message, ok := fallbackMessage(raw); ok {
		return message, nil
	}
	if message, ok := fallbackMessage(repaired); ok {
		return message, nil
	}
	return runtime.GenerateResult{}, errors.New("unable to repair malformed action JSON")
}

const repairInstruction = "Your previous reply was not a valid AOS action. Reply again with exactly one action JSON object " +
	"matching the schema in the system prompt, such as {\"action\":{\"type\":\"tool\",\"notes\":\"...\",\"tool_call\":{...}}}, and nothing else."

func (b PromptBackend) generateFromRaw(ctx context.Context, completer RawCompleter, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	systemPrompt := b.systemPrompt()
	userPrompt := b.userPrompt(request)
	raw, err := completer.CompleteText(ctx, systemPrompt, userPrompt)
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	if parsed, ok := parseGenerateResult(raw); ok {
		return parsed, nil
	}

	repaired, err := completer.CompleteText(runtime.AsRepair(ctx), repairSystemPrompt(), repairUserPrompt(systemPrompt, userPrompt, raw))
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	if parsed, ok := parseGenerateResult(repaired); ok {
		return parsed, nil
	}
	if message, ok := fallbackMessage(raw); ok {
		return message, nil
	}
	if message, ok := fallbackMessage(repaired); ok {
		return message, nil
	}
	return runtime.GenerateResult{}, errors.New("unable to repair malformed action JSON")
}

func (b PromptBackend) systemPrompt() string {
	var builder strings.Builder
	builder.WriteString("You are an AOS agent. ")
	builder.WriteString("Return only JSON matching this schema, with no extra whitespace around the action envelope: ")
	builder.WriteString("{\"action\":{\"type\":\"message|tool|delegate|send|publish|subscribe|wait|exit\",")
	builder.WriteString("\"notes\":\"...\",")
	builder.WriteString("\"message\":\"...\",")
	builder.WriteString("\"tool_call\":{\"name\":\"...\",\"args\":{\"key\":\"value\"}},")
	builder.WriteString("\"delegate\":{\"agent_id\":\"...\",\"goal\":\"...\"},")
	builder.WriteString("\"send\":{\"to\":\"...\",\"body\":\"...\"},")
	builder.WriteString("\"publish\":{\"topic\":\"...\",\"body\":\"...\"},")
	builder.WriteString("\"subscribe\":{\"topic\":\"...\"},")
	builder.WriteString("\"exit_status\":{\"code\":0,\"reason\":\"...\",\"retryable\":false}}}. ")
	builder.WriteString("Choose exactly one action per response. ")
	builder.WriteString("Your hidden reasoning is discarded after every response; only the action and its notes are kept. ")
	builder.WriteString("Use notes as your working memory: a few short lines holding the plan, what is already done, and the next step, so you never have to re-derive them. ")
	builder.WriteString("Write notes on every action of a multi-step task and update them as the plan changes. ")
	builder.WriteString("History entries from the assistant role are your own earlier notes and actions; trust them and continue from them instead of re-reading or rewriting files you already handled. ")
	builder.WriteString("Keeping the envelope compact never applies to string values: file content passed to write_file or edit_file must be formatted exactly as it should appear on disk, with normal line breaks and indentation encoded as \\n in the JSON string. Never squash source code onto fewer lines to shorten the response. ")
	builder.WriteString("Use 'tool' only for allowed tools. ")
	builder.WriteString("Use 'delegate' only for allowed agents. ")
	builder.WriteString("Use 'send', 'publish', or 'subscribe' only when mailbox collaboration is actually needed. ")
	builder.WriteString("Use 'wait' when you need more user or mailbox input before acting further, and put what you need in its message: the user sees that message, not your notes. ")
	builder.WriteString("Use 'message' only for user-facing output or a concise visible status update when the user would benefit from seeing it. ")
	builder.WriteString("Do not use 'message' for hidden chain-of-thought or internal scratch work. ")
	builder.WriteString("Use 'exit' only when your assigned task is complete or blocked. ")
	builder.WriteString("If the user adds, tightens, or corrects a requirement later in the conversation, treat the latest user instruction as authoritative and treat any earlier completion as invalid until the new requirement is satisfied. ")
	builder.WriteString("When a user says things like 'no', 'instead', 'you must', 'the deliverable is', or otherwise corrects your approach, you must re-evaluate the task against that updated requirement before exiting. ")
	builder.WriteString("Never repeat the same successful tool call if the latest tool result already accomplished the task. ")
	builder.WriteString("In particular, after a successful write_file result, do not write the same file again with the same content; continue with any checks your task requires, such as compiling or running what you wrote, and exit once those are done. ")
	builder.WriteString("If a tool fails, times out, returns no matches, returns empty content, or reports a non-zero exit, treat that as a signal to try a different strategy rather than exiting immediately. ")
	builder.WriteString("Prefer at least two materially different attempts before giving up when tools are available. ")
	builder.WriteString("Do not repeat the exact same failing call with unchanged arguments more than once. ")
	builder.WriteString("When inspecting a codebase, prefer using list_dir or find_files before guessing file paths. ")
	builder.WriteString("History entries beginning with tool_notice or tool_error mean the previous approach was not enough and you should adapt. ")
	builder.WriteString("If memory_refs are present, older session context has been compacted into runtime-owned memory segments. Call the read_memory tool with a ref's artifact_id when you need deeper prior context beyond the live history and summary; it is always available. ")
	builder.WriteString("Do not wrap JSON in markdown.\n\n")
	builder.WriteString("Valid examples:\n")
	builder.WriteString("{\"action\":{\"type\":\"delegate\",\"delegate\":{\"agent_id\":\"writer\",\"goal\":\"Draft the final summary\"}}}\n")
	builder.WriteString("{\"action\":{\"type\":\"tool\",\"tool_call\":{\"name\":\"find_files\",\"args\":{\"root\":\"/workspace\",\"pattern\":\"vllm\"}}}}\n")
	builder.WriteString("{\"action\":{\"type\":\"tool\",\"notes\":\"Plan: read note.txt, then summarize it into summary.txt. Done: nothing yet. Next: read note.txt.\",\"tool_call\":{\"name\":\"read_file\",\"args\":{\"path\":\"/workspace/note.txt\"}}}}\n")
	builder.WriteString("{\"action\":{\"type\":\"exit\",\"exit_status\":{\"code\":0,\"reason\":\"task complete\",\"retryable\":false}}}\n\n")
	builder.WriteString("Agent instructions:\n")
	builder.WriteString(b.Agent.SystemPrompt)
	if len(b.Skills) > 0 {
		builder.WriteString("\n\nLoaded skills:\n")
		for _, skill := range b.Skills {
			builder.WriteString("\n")
			builder.WriteString("Skill: ")
			builder.WriteString(skill.Name)
			builder.WriteString("\n")
			builder.WriteString("Path: ")
			builder.WriteString(skill.Path)
			builder.WriteString("\n")
			builder.WriteString(skill.Content)
			builder.WriteString("\n")
		}
	}
	builder.WriteString("\n\nAllowed tools (tool_call args; ? marks optional args):\n")
	for _, tool := range sortedStrings(b.Agent.Capabilities.Tools) {
		builder.WriteString("- ")
		builder.WriteString(toolSignature(tool))
		builder.WriteString("\n")
	}
	builder.WriteString("\nAllowed agents:\n")
	builder.WriteString(strings.Join(sortedStrings(b.Agent.AllowedAgents), ", "))
	return builder.String()
}

// toolArgs lists each tool's tool_call args, so agents do not guess names.
var toolArgs = map[string]string{
	"read_file":        "path",
	"write_file":       "path, content",
	"edit_file":        "path, old_text, new_text, replace_all?",
	"list_dir":         "path",
	"find_files":       "root, pattern",
	"run":              "command, timeout?",
	"web_search":       "query, timeout?",
	"web_fetch":        "url, offset?, timeout?",
	"spawn_agent":      "agent_id, goal",
	"message_agent":    "to, body",
	"await_agent":      "pid",
	"checkpoint":       "",
	"list_artifacts":   "type?, producer_pid?, scope?",
	"publish_artifact": "type, body",
	"read_memory":      "artifact_id",
	"acquire_lease":    "resource, ttl?",
	"release_lease":    "resource",
}

func toolSignature(tool string) string {
	args, ok := toolArgs[tool]
	if !ok {
		return tool
	}
	return tool + "(" + args + ")"
}

func (b PromptBackend) userPrompt(request runtime.GenerateRequest) string {
	type promptContext struct {
		Workflow               string              `json:"workflow"`
		AgentID                string              `json:"agent_id"`
		ProcessID              string              `json:"process_id"`
		Goal                   string              `json:"goal"`
		InputPaths             []string            `json:"input_paths,omitempty"`
		OutputPath             string              `json:"output_path,omitempty"`
		Metadata               map[string]string   `json:"metadata,omitempty"`
		History                []runtime.Message   `json:"history"`
		HistorySummary         string              `json:"history_summary,omitempty"`
		HistoryCompacted       bool                `json:"history_compacted,omitempty"`
		HistoryMessagesOmitted int                 `json:"history_messages_omitted,omitempty"`
		MemoryRefs             []runtime.MemoryRef `json:"memory_refs,omitempty"`
		ReadPaths              []string            `json:"read_paths,omitempty"`
		WritePaths             []string            `json:"write_paths,omitempty"`
		ToolErrors             int                 `json:"tool_errors"`
		LastTool               string              `json:"last_tool_observation,omitempty"`
	}
	compacted := compactHistory(request.History, b.compactionPolicy())
	toolErrors, lastTool := summarizeToolHistory(request.History)
	payload := promptContext{
		Workflow:               b.Workflow.Name,
		AgentID:                b.Agent.ID,
		ProcessID:              request.Process.PID,
		Goal:                   request.Process.Metadata["goal"],
		InputPaths:             splitList(request.Process.Metadata["input_paths"]),
		OutputPath:             request.Process.Metadata["output_path"],
		Metadata:               cloneMap(request.Process.Metadata),
		History:                compacted.Recent,
		HistorySummary:         compacted.Summary,
		HistoryCompacted:       compacted.Compacted,
		HistoryMessagesOmitted: compacted.Omitted,
		MemoryRefs:             append([]runtime.MemoryRef(nil), request.MemoryRefs...),
		ReadPaths:              append([]string(nil), request.Process.CapabilitySet.ReadPaths...),
		WritePaths:             append([]string(nil), request.Process.CapabilitySet.WritePaths...),
		ToolErrors:             toolErrors,
		LastTool:               lastTool,
	}
	bytes, _ := json.MarshalIndent(payload, "", "  ")
	return fmt.Sprintf(
		"Here is your current session context.\n\n%s\n\nRespond with the next action JSON only.",
		string(bytes),
	)
}

type compactedHistory struct {
	Recent    []runtime.Message
	Summary   string
	Compacted bool
	Omitted   int
}

func (b PromptBackend) compactionPolicy() runtime.CompactionPolicy {
	if b.Compaction == (runtime.CompactionPolicy{}) {
		return runtime.CompactionPolicy{
			MaxMessages:  maxPromptHistoryMessages,
			MaxChars:     maxPromptHistoryChars,
			KeepMessages: maxPromptHistoryMessages,
		}
	}
	return b.Compaction
}

func compactHistory(history []runtime.Message, policy runtime.CompactionPolicy) compactedHistory {
	if len(history) == 0 {
		return compactedHistory{}
	}
	if !policy.ShouldCompact(history) {
		return compactedHistory{Recent: append([]runtime.Message(nil), history...)}
	}

	start := len(history) - policy.KeepMessages
	if start < 0 {
		start = 0
	}
	recent := append([]runtime.Message(nil), history[start:]...)
	older := history[:start]
	return compactedHistory{
		Recent:    recent,
		Summary:   summarizeHistory(older),
		Compacted: true,
		Omitted:   len(older),
	}
}

func summarizeHistory(history []runtime.Message) string {
	if len(history) == 0 {
		return ""
	}
	var userItems []string
	var assistantItems []string
	var toolItems []string
	for _, message := range history {
		item := summarizeHistoryMessage(message)
		if item == "" {
			continue
		}
		switch message.Role {
		case "user":
			userItems = appendUniqueLimited(userItems, item, maxSummaryItemsPerRole)
		case "assistant":
			assistantItems = appendUniqueLimited(assistantItems, item, maxSummaryItemsPerRole)
		case "tool":
			toolItems = appendUniqueLimited(toolItems, item, maxSummaryItemsPerRole)
		}
	}

	var sections []string
	if len(userItems) > 0 {
		sections = append(sections, "Earlier user instructions:\n- "+strings.Join(userItems, "\n- "))
	}
	if len(assistantItems) > 0 {
		sections = append(sections, "Earlier assistant decisions/messages:\n- "+strings.Join(assistantItems, "\n- "))
	}
	if len(toolItems) > 0 {
		sections = append(sections, "Earlier tool/delegation outcomes:\n- "+strings.Join(toolItems, "\n- "))
	}
	return strings.Join(sections, "\n\n")
}

func summarizeHistoryMessage(message runtime.Message) string {
	content := strings.TrimSpace(message.Content)
	if content == "" {
		return ""
	}
	switch message.Role {
	case "user", "assistant":
		return summarizeText(content, 180)
	case "tool":
		return summarizeToolMessage(content)
	default:
		return summarizeText(content, 180)
	}
}

func summarizeToolMessage(content string) string {
	content = strings.TrimSpace(content)
	switch {
	case strings.HasPrefix(content, "tool_error "),
		strings.HasPrefix(content, "tool_notice "),
		strings.HasPrefix(content, "delegate_result "),
		strings.HasPrefix(content, "send:"),
		strings.HasPrefix(content, "publish:"),
		strings.HasPrefix(content, "subscribe:"):
		return summarizeText(content, 180)
	default:
		colon := strings.Index(content, ":")
		if colon > 0 {
			return content[:colon] + ": " + summarizeText(content[colon+1:], 160)
		}
		return summarizeText(content, 180)
	}
}

func summarizeText(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func appendUniqueLimited(items []string, item string, limit int) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	if len(items) >= limit {
		return items
	}
	return append(items, item)
}

func summarizeToolHistory(history []runtime.Message) (int, string) {
	toolErrors := 0
	lastTool := ""
	for _, message := range history {
		if message.Role != "tool" {
			continue
		}
		if strings.HasPrefix(message.Content, "tool_error ") {
			toolErrors++
		}
		lastTool = message.Content
	}
	if len(lastTool) > 400 {
		lastTool = lastTool[:400] + "..."
	}
	return toolErrors, lastTool
}

func splitList(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, "\n")
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func (b PromptBackend) autoExitForCompletedTool(request runtime.GenerateRequest) (runtime.GenerateResult, bool) {
	if len(b.Agent.AllowedAgents) != 0 {
		return runtime.GenerateResult{}, false
	}
	if len(b.Agent.Capabilities.Tools) != 1 || b.Agent.Capabilities.Tools[0] != "write_file" {
		return runtime.GenerateResult{}, false
	}
	if len(request.History) == 0 {
		return runtime.GenerateResult{}, false
	}
	last := request.History[len(request.History)-1]
	if last.Role != "tool" || !strings.HasPrefix(last.Content, "write_file:") {
		return runtime.GenerateResult{}, false
	}
	return runtime.GenerateResult{
		Action: runtime.GenerateAction{
			Type: "exit",
			ExitStatus: &kernel.ExitStatus{
				Code:      0,
				Reason:    "write complete",
				Retryable: false,
			},
		},
	}, true
}

func repairSystemPrompt() string {
	return "You repair malformed AOS action outputs. Return only valid compact JSON matching the AOS action schema. Do not add explanation."
}

func repairUserPrompt(systemPrompt, userPrompt, raw string) string {
	return "Original system prompt:\n" + systemPrompt + "\n\nOriginal user prompt:\n" + userPrompt + "\n\nMalformed output:\n" + raw + "\n\nRewrite the malformed output as one valid AOS action JSON object."
}

func parseGenerateResult(raw string) (runtime.GenerateResult, bool) {
	raw = repairLooseJSONObject(extractTrailingJSONObject(raw))

	var nested runtime.GenerateResult
	if err := json.Unmarshal([]byte(raw), &nested); err == nil && actionUsable(nested.Action) {
		return nested, true
	}

	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err == nil {
		if normalized, ok := normalizeGenericAction(generic); ok {
			return normalized, true
		}
	}

	var flat struct {
		Type      string                 `json:"type"`
		Message   string                 `json:"message"`
		ToolCall  *runtime.ToolCall      `json:"tool_call"`
		Delegate  *runtime.DelegateCall  `json:"delegate"`
		Send      *runtime.SendCall      `json:"send"`
		Publish   *runtime.PublishCall   `json:"publish"`
		Subscribe *runtime.SubscribeCall `json:"subscribe"`
		Reason    string                 `json:"reason"`
		Retryable bool                   `json:"retryable"`
		Code      int                    `json:"code"`
	}
	if err := json.Unmarshal([]byte(raw), &flat); err == nil && flat.Type != "" {
		return runtime.GenerateResult{
			Action: runtime.GenerateAction{
				Type:       flat.Type,
				Message:    flat.Message,
				ToolCall:   flat.ToolCall,
				Delegate:   flat.Delegate,
				Send:       flat.Send,
				Publish:    flat.Publish,
				Subscribe:  flat.Subscribe,
				ExitStatus: exitStatusFor(flat.Type, flat.Code, flat.Reason, flat.Retryable),
			},
		}, true
	}

	var legacy struct {
		Action    string `json:"action"`
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
		Code      int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err == nil && legacy.Action != "" {
		return runtime.GenerateResult{
			Action: runtime.GenerateAction{
				Type:       legacy.Action,
				ExitStatus: exitStatusFor(legacy.Action, legacy.Code, legacy.Reason, legacy.Retryable),
			},
		}, true
	}
	return runtime.GenerateResult{}, false
}

func repairLooseJSONObject(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return trimmed
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start == -1 || end <= start {
		return trimmed
	}
	candidate := trimmed[start : end+1]
	candidate = halfQuotedKeyPattern.ReplaceAllString(candidate, `${1}"${2}":`)
	candidate = bareKeyPattern.ReplaceAllString(candidate, `${1}"${2}":`)
	candidate = trailingCommaPattern.ReplaceAllString(candidate, `$1`)
	if json.Valid([]byte(candidate)) {
		return candidate
	}
	return trimmed
}

func fallbackMessage(raw string) (runtime.GenerateResult, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return runtime.GenerateResult{}, false
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return runtime.GenerateResult{}, false
	}
	if strings.Contains(trimmed, "</think>") || strings.Contains(trimmed, "<think>") {
		return runtime.GenerateResult{}, false
	}
	if strings.Contains(trimmed, `{"action"`) || strings.Contains(trimmed, `"tool_call"`) {
		return runtime.GenerateResult{}, false
	}
	return runtime.GenerateResult{
		Action: runtime.GenerateAction{
			Type:    "message",
			Message: trimmed,
		},
	}, true
}

func normalizeGenericAction(data map[string]any) (runtime.GenerateResult, bool) {
	actionData := data
	if nested, ok := data["action"].(map[string]any); ok {
		actionData = nested
	}

	actionType := firstString(actionData, "type", "action", "kind")
	if actionType == "" {
		return runtime.GenerateResult{}, false
	}
	result := runtime.GenerateResult{
		Action: runtime.GenerateAction{
			Type:  strings.ToLower(strings.TrimSpace(actionType)),
			Notes: firstString(actionData, "notes"),
		},
	}

	switch result.Action.Type {
	case "message":
		result.Action.Message = firstString(actionData, "message", "content", "text")
	case "tool":
		callData := mapValue(actionData, "tool_call", "tool", "call")
		if len(callData) == 0 {
			callData = actionData
		}
		name := firstString(callData, "name", "tool", "tool_name")
		args := stringMap(value(callData, "args", "arguments", "tool_args"))
		if len(args) == 0 {
			args = implicitToolArgs(callData, "type", "action", "kind", "name", "tool", "tool_name", "tool_call", "call", "args", "arguments", "tool_args")
		}
		if name == "" {
			return runtime.GenerateResult{}, false
		}
		result.Action.ToolCall = &runtime.ToolCall{Name: name, Args: args}
	case "delegate":
		callData := mapValue(actionData, "delegate")
		if len(callData) == 0 {
			callData = actionData
		}
		agentID := firstString(callData, "agent_id", "agent", "to")
		goal := firstString(callData, "goal", "task", "message")
		if agentID == "" {
			return runtime.GenerateResult{}, false
		}
		result.Action.Delegate = &runtime.DelegateCall{AgentID: agentID, Goal: goal}
	case "send":
		callData := mapValue(actionData, "send")
		if len(callData) == 0 {
			callData = actionData
		}
		to := firstString(callData, "to", "agent_id", "recipient")
		body := firstString(callData, "body", "message", "content", "text")
		if to == "" {
			return runtime.GenerateResult{}, false
		}
		result.Action.Send = &runtime.SendCall{To: to, Body: body}
	case "publish":
		callData := mapValue(actionData, "publish")
		if len(callData) == 0 {
			callData = actionData
		}
		topic := firstString(callData, "topic", "channel")
		body := firstString(callData, "body", "message", "content", "text")
		if topic == "" {
			return runtime.GenerateResult{}, false
		}
		result.Action.Publish = &runtime.PublishCall{Topic: topic, Body: body}
	case "subscribe":
		callData := mapValue(actionData, "subscribe")
		if len(callData) == 0 {
			callData = actionData
		}
		topic := firstString(callData, "topic", "channel")
		if topic == "" {
			return runtime.GenerateResult{}, false
		}
		result.Action.Subscribe = &runtime.SubscribeCall{Topic: topic}
	case "wait":
		// What the agent is waiting for; in a terminal it is all the user sees.
		result.Action.Message = firstString(actionData, "message", "content", "text", "question")
	case "exit":
		statusData := mapValue(actionData, "exit_status")
		if len(statusData) == 0 {
			statusData = actionData
		}
		result.Action.ExitStatus = &kernel.ExitStatus{
			Code:      firstInt(statusData, "code"),
			Reason:    firstString(statusData, "reason", "message"),
			Retryable: firstBool(statusData, "retryable"),
		}
	default:
		args := stringMap(value(actionData, "args", "arguments", "tool_args"))
		if len(args) == 0 {
			args = implicitToolArgs(actionData, "type", "action", "kind", "args", "arguments", "tool_args")
		}
		if len(args) == 0 {
			return runtime.GenerateResult{}, false
		}
		result.Action.Type = "tool"
		result.Action.ToolCall = &runtime.ToolCall{
			Name: actionType,
			Args: args,
		}
	}

	return result, true
}

func implicitToolArgs(data map[string]any, exclude ...string) map[string]string {
	skip := make(map[string]struct{}, len(exclude))
	for _, key := range exclude {
		skip[key] = struct{}{}
	}
	args := map[string]string{}
	for key, raw := range data {
		if _, ignored := skip[key]; ignored {
			continue
		}
		if value := scalarString(raw); value != "" {
			args[key] = value
		}
	}
	if len(args) == 0 {
		return nil
	}
	return args
}

func actionUsable(action runtime.GenerateAction) bool {
	switch action.Type {
	case "message", "wait":
		return true
	case "tool":
		return action.ToolCall != nil && action.ToolCall.Name != ""
	case "delegate":
		return action.Delegate != nil && action.Delegate.AgentID != ""
	case "send":
		return action.Send != nil && action.Send.To != ""
	case "publish":
		return action.Publish != nil && action.Publish.Topic != ""
	case "subscribe":
		return action.Subscribe != nil && action.Subscribe.Topic != ""
	case "exit":
		return action.ExitStatus != nil
	default:
		return false
	}
}

func mapValue(data map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if nested, ok := data[key].(map[string]any); ok {
			return nested
		}
	}
	return nil
}

func value(data map[string]any, keys ...string) any {
	for _, key := range keys {
		if candidate, ok := data[key]; ok {
			return candidate
		}
	}
	return nil
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key]; ok {
			if text := scalarString(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return trimmed
		}
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	}
	return ""
}

func firstInt(data map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := data[key]; ok {
			switch typed := value.(type) {
			case float64:
				return int(typed)
			case int:
				return typed
			case int64:
				return int(typed)
			case json.Number:
				if parsed, err := typed.Int64(); err == nil {
					return int(parsed)
				}
			case string:
				if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
					return parsed
				}
			}
		}
	}
	return 0
}

func firstBool(data map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := data[key]; ok {
			switch typed := value.(type) {
			case bool:
				return typed
			case string:
				parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
				if err == nil {
					return parsed
				}
			}
		}
	}
	return false
}

func stringMap(value any) map[string]string {
	if value == nil {
		return nil
	}
	if typed, ok := value.(map[string]string); ok {
		return typed
	}
	nested, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(nested))
	for key, candidate := range nested {
		switch typed := candidate.(type) {
		case string:
			out[key] = typed
		case json.Number:
			out[key] = typed.String()
		case float64:
			out[key] = strconv.FormatFloat(typed, 'f', -1, 64)
		case bool:
			if typed {
				out[key] = "true"
			} else {
				out[key] = "false"
			}
		}
	}
	return out
}

func exitStatusFor(action string, code int, reason string, retryable bool) *kernel.ExitStatus {
	if action != "exit" {
		return nil
	}
	return &kernel.ExitStatus{
		Code:      code,
		Reason:    reason,
		Retryable: retryable,
	}
}

func extractTrailingJSONObject(input string) string {
	start := strings.LastIndex(input, "{")
	if start == -1 {
		return strings.TrimSpace(input)
	}
	for candidateStart := start; candidateStart >= 0; candidateStart = previousBrace(input, candidateStart) {
		candidate := strings.TrimSpace(input[candidateStart:])
		if json.Valid([]byte(candidate)) {
			return candidate
		}
	}
	return strings.TrimSpace(input)
}

func previousBrace(input string, before int) int {
	for i := before - 1; i >= 0; i-- {
		if input[i] == '{' {
			if utf8.ValidString(input[i:]) {
				return i
			}
			return i
		}
	}
	return -1
}
