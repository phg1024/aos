package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/tools"
)

var ErrMalformedBackendOutput = errors.New("malformed backend output")
var ErrAgentBlocked = errors.New("agent blocked waiting for input")

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ToolCall struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args,omitempty"`
}

type DelegateCall struct {
	AgentID string `json:"agent_id"`
	Goal    string `json:"goal"`
}

type SendCall struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

type PublishCall struct {
	Topic string `json:"topic"`
	Body  string `json:"body"`
}

type SubscribeCall struct {
	Topic string `json:"topic"`
}

type GenerateRequest struct {
	Process kernel.Process `json:"process"`
	History []Message      `json:"history"`
}

type GenerateAction struct {
	Type       string             `json:"type"`
	Message    string             `json:"message,omitempty"`
	ToolCall   *ToolCall          `json:"tool_call,omitempty"`
	Delegate   *DelegateCall      `json:"delegate,omitempty"`
	Send       *SendCall          `json:"send,omitempty"`
	Publish    *PublishCall       `json:"publish,omitempty"`
	Subscribe  *SubscribeCall     `json:"subscribe,omitempty"`
	ExitStatus *kernel.ExitStatus `json:"exit_status,omitempty"`
}

type GenerateResult struct {
	Action GenerateAction `json:"action"`
}

type Backend interface {
	Generate(ctx context.Context, request GenerateRequest) (GenerateResult, error)
}

type Session struct {
	Messages            []Message     `json:"messages"`
	ToolArtifactIDs     []string      `json:"tool_artifact_ids,omitempty"`
	DelegateArtifactIDs []string      `json:"delegate_artifact_ids,omitempty"`
	MailboxArtifactIDs  []string      `json:"mailbox_artifact_ids,omitempty"`
	ToolAttempts        []ToolAttempt `json:"tool_attempts,omitempty"`
}

type ToolAttempt struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Useful    bool   `json:"useful"`
	Error     string `json:"error,omitempty"`
}

type RunResult struct {
	PID          string            `json:"pid,omitempty"`
	ExitStatus   kernel.ExitStatus `json:"exit_status"`
	CheckpointID string            `json:"checkpoint_id"`
	Session      Session           `json:"session"`
	Yielded      bool              `json:"yielded,omitempty"`
	Blocked      bool              `json:"blocked,omitempty"`
}

type AgentRuntime struct {
	Kernel          *kernel.Kernel
	Artifacts       *artifact.Store
	Tools           *tools.Runtime
	Bus             *ipc.Bus
	Backend         Backend
	DelegateHandler DelegateHandler
	MaxSteps        int
}

type DelegateResult struct {
	AgentID      string `json:"agent_id"`
	PID          string `json:"pid"`
	Summary      string `json:"summary"`
	ResultID     string `json:"result_id,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
}

type DelegateHandler func(ctx context.Context, parent kernel.Process, call DelegateCall, session Session) (DelegateResult, error)

func (r *AgentRuntime) Run(ctx context.Context, pid string, initial []Message) (RunResult, error) {
	return r.runSession(ctx, pid, Session{Messages: append([]Message(nil), initial...)}, RunOptions{})
}

func (r *AgentRuntime) RunWithOptions(ctx context.Context, pid string, initial []Message, options RunOptions) (RunResult, error) {
	return r.runSession(ctx, pid, Session{Messages: append([]Message(nil), initial...)}, options)
}

type RunOptions struct {
	StopOnAssistantMessage bool
	ContinueOnToolError    bool
	OnToolCall             func(ToolCall)
	OnToolResult           func(tools.Result)
	OnToolError            func(ToolCall, error)
	OnAssistantMessage     func(string)
}

func (r *AgentRuntime) Resume(ctx context.Context, pid, checkpointID string, appended []Message, options RunOptions) (RunResult, error) {
	session, err := r.RestoreCheckpoint(checkpointID)
	if err != nil {
		return RunResult{}, err
	}
	session.Messages = append(session.Messages, appended...)
	process, err := r.Kernel.Get(pid)
	if err != nil {
		return RunResult{}, err
	}
	if process.State == kernel.StateBlocked || process.State == kernel.StateSleeping {
		if _, err := r.Kernel.Transition(pid, kernel.StateReady); err != nil {
			return RunResult{}, err
		}
	}
	if process.State == kernel.StateExited {
		if process.ExitStatus == nil || !process.ExitStatus.Retryable {
			return RunResult{}, errors.New("process exited and is not retryable")
		}
		return r.Recover(ctx, pid, checkpointID, appended, options)
	}
	return r.runSession(ctx, pid, session, options)
}

func (r *AgentRuntime) Recover(ctx context.Context, pid, checkpointID string, appended []Message, options RunOptions) (RunResult, error) {
	process, err := r.Kernel.Get(pid)
	if err != nil {
		return RunResult{}, err
	}
	session, err := r.RestoreCheckpoint(checkpointID)
	if err != nil {
		return RunResult{}, err
	}
	session.Messages = append(session.Messages, appended...)
	recovered, err := r.Kernel.Spawn(kernel.ProcessSpec{
		ParentPID:     process.ParentPID,
		Role:          process.Role,
		Metadata:      recoveredMetadata(process.Metadata, pid),
		Mounts:        append([]capability.Mount(nil), process.Mounts...),
		CapabilitySet: process.CapabilitySet,
	})
	if err != nil {
		return RunResult{}, err
	}
	return r.runSession(ctx, recovered.PID, session, options)
}

func (r *AgentRuntime) runSession(ctx context.Context, pid string, session Session, options RunOptions) (RunResult, error) {
	process, err := r.Kernel.Get(pid)
	if err != nil {
		return RunResult{}, err
	}
	if process.State == kernel.StateReady || process.State == kernel.StateBlocked || process.State == kernel.StateSleeping {
		if _, err := r.Kernel.Transition(pid, kernel.StateRunning); err != nil {
			return RunResult{}, err
		}
	}

	var checkpointID string

	maxSteps := r.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 32
	}
	for step := 0; step < maxSteps; step++ {
		var inboxMessages []ipc.Message
		if r.Bus != nil {
			inboxMessages, err = r.Bus.ReceiveAll(pid)
			if err != nil {
				return RunResult{}, err
			}
			session, err = r.appendInboxArtifacts(pid, session, inboxMessages)
			if err != nil {
				return RunResult{}, err
			}
		}

		result, err := r.Backend.Generate(ctx, GenerateRequest{
			Process: process,
			History: append([]Message(nil), session.Messages...),
		})
		if err != nil {
			if options.ContinueOnToolError {
				artifactRecord, recordErr := r.Artifacts.CreateJSON("backend_error", pid, map[string]string{
					"error": err.Error(),
				}, nil, map[string]string{
					"kind": "backend_error",
				})
				if recordErr != nil {
					return RunResult{}, recordErr
				}
				session.ToolArtifactIDs = append(session.ToolArtifactIDs, artifactRecord.ID)
				session.Messages = append(session.Messages, Message{
					Role:    "tool",
					Content: fmt.Sprintf("tool_error name=backend error=%s try_different_strategy=true", err.Error()),
				})
				continue
			}
			exitStatus := kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: true}
			if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
				return RunResult{}, exitErr
			}
			return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, err
		}

		switch result.Action.Type {
		case "message":
			if options.OnAssistantMessage != nil {
				options.OnAssistantMessage(result.Action.Message)
			}
			session.Messages = append(session.Messages, Message{
				Role:    "assistant",
				Content: result.Action.Message,
			})
			artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, nil, map[string]string{
				"kind": "yield",
			})
			if err != nil {
				return RunResult{}, err
			}
			checkpointID = artifactRecord.ID
			if options.StopOnAssistantMessage {
				if _, err := r.Kernel.Transition(pid, kernel.StateReady); err != nil {
					return RunResult{}, err
				}
				return RunResult{PID: pid, CheckpointID: checkpointID, Session: session, Yielded: true}, nil
			}
		case "tool":
			if result.Action.ToolCall == nil || result.Action.ToolCall.Name == "" {
				return r.failMalformed(pid, checkpointID)
			}
			if options.OnToolCall != nil {
				options.OnToolCall(*result.Action.ToolCall)
			}
			signature := toolCallSignature(*result.Action.ToolCall)
			if repeatedUnhelpfulToolCall(session.ToolAttempts, signature) {
				repeatErr := errors.New("repeated unsuccessful tool call suppressed; try different args or another tool")
				if options.OnToolError != nil {
					options.OnToolError(*result.Action.ToolCall, repeatErr)
				}
				artifactRecord, recordErr := r.Artifacts.CreateJSON("tool_error", pid, map[string]string{
					"tool":  result.Action.ToolCall.Name,
					"error": repeatErr.Error(),
				}, nil, map[string]string{
					"tool": result.Action.ToolCall.Name,
				})
				if recordErr != nil {
					return RunResult{}, recordErr
				}
				session.ToolArtifactIDs = append(session.ToolArtifactIDs, artifactRecord.ID)
				session.ToolAttempts = append(session.ToolAttempts, ToolAttempt{
					Name:      result.Action.ToolCall.Name,
					Signature: signature,
					Useful:    false,
					Error:     repeatErr.Error(),
				})
				session.Messages = append(session.Messages, Message{
					Role:    "tool",
					Content: fmt.Sprintf("tool_error name=%s error=%s try_different_strategy=true", result.Action.ToolCall.Name, repeatErr.Error()),
				})
				break
			}
			toolResult, err := r.Tools.Execute(tools.Request{
				Tool:          result.Action.ToolCall.Name,
				Args:          result.Action.ToolCall.Args,
				CapabilitySet: process.CapabilitySet,
				Mounts:        process.Mounts,
			})
			if err != nil {
				if options.OnToolError != nil {
					options.OnToolError(*result.Action.ToolCall, err)
				}
				if options.ContinueOnToolError {
					artifactRecord, recordErr := r.Artifacts.CreateJSON("tool_error", pid, map[string]string{
						"tool":  result.Action.ToolCall.Name,
						"error": err.Error(),
					}, nil, map[string]string{
						"tool": result.Action.ToolCall.Name,
					})
					if recordErr != nil {
						return RunResult{}, recordErr
					}
					session.ToolArtifactIDs = append(session.ToolArtifactIDs, artifactRecord.ID)
					session.ToolAttempts = append(session.ToolAttempts, ToolAttempt{
						Name:      result.Action.ToolCall.Name,
						Signature: signature,
						Useful:    false,
						Error:     err.Error(),
					})
					session.Messages = append(session.Messages, Message{
						Role:    "tool",
						Content: fmt.Sprintf("tool_error name=%s error=%s try_different_strategy=true", result.Action.ToolCall.Name, err.Error()),
					})
					break
				}
				exitStatus := kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: false}
				if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
					return RunResult{}, exitErr
				}
				return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, err
			}
			artifactRecord, err := r.Artifacts.CreateJSON("tool_result", pid, toolResult, nil, map[string]string{
				"tool": toolResult.Tool,
			})
			if err != nil {
				return RunResult{}, err
			}
			if options.OnToolResult != nil {
				options.OnToolResult(toolResult)
			}
			session.ToolArtifactIDs = append(session.ToolArtifactIDs, artifactRecord.ID)
			useful, reason := assessToolResult(toolResult)
			session.ToolAttempts = append(session.ToolAttempts, ToolAttempt{
				Name:      result.Action.ToolCall.Name,
				Signature: signature,
				Useful:    useful,
			})
			session.Messages = append(session.Messages, Message{
				Role:    "tool",
				Content: fmt.Sprintf("%s:%v", toolResult.Tool, toolResult.Output),
			})
			if !useful {
				session.Messages = append(session.Messages, Message{
					Role:    "tool",
					Content: fmt.Sprintf("tool_notice name=%s useful=false reason=%s try_different_strategy=true", toolResult.Tool, reason),
				})
			}
		case "delegate":
			if result.Action.Delegate == nil || result.Action.Delegate.AgentID == "" {
				return r.failMalformed(pid, checkpointID)
			}
			if r.DelegateHandler == nil {
				exitStatus := kernel.ExitStatus{Code: 1, Reason: "delegate handler not configured", Retryable: false}
				if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
					return RunResult{}, exitErr
				}
				return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, errors.New(exitStatus.Reason)
			}
			delegateResult, err := r.DelegateHandler(ctx, process, *result.Action.Delegate, session)
			if err != nil {
				exitStatus := kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: false}
				if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
					return RunResult{}, exitErr
				}
				return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, err
			}
			delegateArtifact, err := r.Artifacts.CreateJSON("delegate_result", pid, delegateResult, nil, map[string]string{
				"agent_id": delegateResult.AgentID,
				"pid":      delegateResult.PID,
			})
			if err != nil {
				return RunResult{}, err
			}
			session.DelegateArtifactIDs = append(session.DelegateArtifactIDs, delegateArtifact.ID)
			session.Messages = append(session.Messages, Message{
				Role: "tool",
				Content: fmt.Sprintf(
					"delegate_result agent=%s pid=%s summary=%s result_artifact=%s checkpoint=%s",
					delegateResult.AgentID,
					delegateResult.PID,
					delegateResult.Summary,
					delegateResult.ResultID,
					delegateResult.CheckpointID,
				),
			})
		case "send":
			if result.Action.Send == nil || result.Action.Send.To == "" {
				return r.failMalformed(pid, checkpointID)
			}
			if r.Bus == nil {
				return r.failRuntime(pid, checkpointID, errors.New("mailbox bus not configured"))
			}
			message, err := r.Bus.Send(pid, result.Action.Send.To, result.Action.Send.Body)
			if err != nil {
				return r.failRuntime(pid, checkpointID, err)
			}
			artifactRecord, err := r.Artifacts.CreateJSON("mailbox_message", pid, message, nil, map[string]string{
				"direction": "send",
				"to":        message.To,
			})
			if err != nil {
				return RunResult{}, err
			}
			session.MailboxArtifactIDs = append(session.MailboxArtifactIDs, artifactRecord.ID)
			session.Messages = append(session.Messages, Message{
				Role:    "tool",
				Content: fmt.Sprintf("send:to=%s id=%s", message.To, message.ID),
			})
		case "publish":
			if result.Action.Publish == nil || result.Action.Publish.Topic == "" {
				return r.failMalformed(pid, checkpointID)
			}
			if r.Bus == nil {
				return r.failRuntime(pid, checkpointID, errors.New("mailbox bus not configured"))
			}
			messages, err := r.Bus.Publish(pid, result.Action.Publish.Topic, result.Action.Publish.Body)
			if err != nil {
				return r.failRuntime(pid, checkpointID, err)
			}
			artifactRecord, err := r.Artifacts.CreateJSON("mailbox_publish", pid, messages, nil, map[string]string{
				"topic": result.Action.Publish.Topic,
			})
			if err != nil {
				return RunResult{}, err
			}
			session.MailboxArtifactIDs = append(session.MailboxArtifactIDs, artifactRecord.ID)
			session.Messages = append(session.Messages, Message{
				Role:    "tool",
				Content: fmt.Sprintf("publish:topic=%s count=%d", result.Action.Publish.Topic, len(messages)),
			})
		case "subscribe":
			if result.Action.Subscribe == nil || result.Action.Subscribe.Topic == "" {
				return r.failMalformed(pid, checkpointID)
			}
			if r.Bus == nil {
				return r.failRuntime(pid, checkpointID, errors.New("mailbox bus not configured"))
			}
			if err := r.Bus.Subscribe(pid, result.Action.Subscribe.Topic); err != nil {
				return r.failRuntime(pid, checkpointID, err)
			}
			artifactRecord, err := r.Artifacts.CreateJSON("mailbox_subscription", pid, result.Action.Subscribe, nil, map[string]string{
				"topic": result.Action.Subscribe.Topic,
			})
			if err != nil {
				return RunResult{}, err
			}
			session.MailboxArtifactIDs = append(session.MailboxArtifactIDs, artifactRecord.ID)
			session.Messages = append(session.Messages, Message{
				Role:    "tool",
				Content: "subscribe:topic=" + result.Action.Subscribe.Topic,
			})
		case "wait":
			artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, nil, map[string]string{
				"kind": "blocked",
			})
			if err != nil {
				return RunResult{}, err
			}
			checkpointID = artifactRecord.ID
			if _, err := r.Kernel.Transition(pid, kernel.StateBlocked); err != nil {
				return RunResult{}, err
			}
			return RunResult{PID: pid, CheckpointID: checkpointID, Session: session, Blocked: true}, ErrAgentBlocked
		case "exit":
			if result.Action.ExitStatus == nil {
				return r.failMalformed(pid, checkpointID)
			}
			artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, nil, map[string]string{
				"kind": "final",
			})
			if err != nil {
				return RunResult{}, err
			}
			checkpointID = artifactRecord.ID
			process, err = r.Kernel.Exit(pid, *result.Action.ExitStatus)
			if err != nil {
				return RunResult{}, err
			}
			return RunResult{PID: pid, ExitStatus: *process.ExitStatus, CheckpointID: checkpointID, Session: session}, nil
		default:
			return r.failMalformed(pid, checkpointID)
		}

		artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, nil, map[string]string{
			"kind": "intermediate",
		})
		if err != nil {
			return RunResult{}, err
		}
		checkpointID = artifactRecord.ID
	}

	exitStatus := kernel.ExitStatus{Code: 1, Reason: "step limit exceeded", Retryable: true}
	if _, err := r.Kernel.Exit(pid, exitStatus); err != nil {
		return RunResult{}, err
	}
	return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID, Session: session}, nil
}

func assessToolResult(result tools.Result) (bool, string) {
	switch result.Tool {
	case "find_files":
		if strings.TrimSpace(result.Output["matches"]) == "" {
			return false, "no_matches"
		}
	case "list_dir":
		if strings.TrimSpace(result.Output["entries"]) == "" {
			return false, "empty_directory"
		}
	case "read_file":
		if strings.TrimSpace(result.Output["content"]) == "" {
			return false, "empty_file"
		}
	case "web_search":
		if strings.TrimSpace(result.Output["result"]) == "" {
			return false, "no_search_hits"
		}
	case "web_fetch":
		status := strings.TrimSpace(result.Output["status"])
		content := strings.TrimSpace(result.Output["content"])
		if strings.HasPrefix(status, "4") || strings.HasPrefix(status, "5") {
			return false, "http_error"
		}
		if content == "" {
			return false, "empty_response"
		}
	case "run":
		exitCode := strings.TrimSpace(result.Output["exit_code"])
		if exitCode != "" && exitCode != "0" {
			return false, "non_zero_exit"
		}
	}
	return true, ""
}

func toolCallSignature(call ToolCall) string {
	if len(call.Args) == 0 {
		return call.Name
	}
	keys := make([]string, 0, len(call.Args))
	for key := range call.Args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString(call.Name)
	for _, key := range keys {
		builder.WriteString("|")
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(call.Args[key])
	}
	return builder.String()
}

func repeatedUnhelpfulToolCall(attempts []ToolAttempt, signature string) bool {
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		if attempt.Signature != signature {
			continue
		}
		return !attempt.Useful
	}
	return false
}

func (r *AgentRuntime) RestoreCheckpoint(id string) (Session, error) {
	var session Session
	err := r.Artifacts.ReadJSON(id, &session)
	return session, err
}

func (r *AgentRuntime) failMalformed(pid, checkpointID string) (RunResult, error) {
	exitStatus := kernel.ExitStatus{Code: 1, Reason: ErrMalformedBackendOutput.Error(), Retryable: false}
	if _, err := r.Kernel.Exit(pid, exitStatus); err != nil {
		return RunResult{}, err
	}
	return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, ErrMalformedBackendOutput
}

func (r *AgentRuntime) failRuntime(pid, checkpointID string, runErr error) (RunResult, error) {
	exitStatus := kernel.ExitStatus{Code: 1, Reason: runErr.Error(), Retryable: false}
	if _, err := r.Kernel.Exit(pid, exitStatus); err != nil {
		return RunResult{}, err
	}
	return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, runErr
}

func (r *AgentRuntime) appendInboxArtifacts(pid string, session Session, messages []ipc.Message) (Session, error) {
	for _, message := range messages {
		artifactRecord, err := r.Artifacts.CreateJSON("mailbox_message", pid, message, nil, map[string]string{
			"direction": "receive",
			"from":      message.From,
			"topic":     message.Topic,
		})
		if err != nil {
			return Session{}, err
		}
		session.MailboxArtifactIDs = append(session.MailboxArtifactIDs, artifactRecord.ID)
		role := "mailbox"
		content := fmt.Sprintf("from=%s topic=%s body=%s", message.From, message.Topic, message.Body)
		if message.From == "user" {
			role = "user"
			content = message.Body
		}
		session.Messages = append(session.Messages, Message{
			Role:    role,
			Content: content,
		})
	}
	return session, nil
}

func recoveredMetadata(in map[string]string, previousPID string) map[string]string {
	out := map[string]string{}
	for key, value := range in {
		out[key] = value
	}
	out["resumed_from"] = previousPID
	return out
}

type FakeBackend struct {
	Steps    []GenerateResult
	Requests []GenerateRequest
	index    int
}

func (f *FakeBackend) Generate(_ context.Context, request GenerateRequest) (GenerateResult, error) {
	f.Requests = append(f.Requests, request)
	if f.index >= len(f.Steps) {
		return GenerateResult{
			Action: GenerateAction{
				Type: "exit",
				ExitStatus: &kernel.ExitStatus{
					Code:   0,
					Reason: "completed",
				},
			},
		}, nil
	}
	step := f.Steps[f.index]
	f.index++
	return step, nil
}
