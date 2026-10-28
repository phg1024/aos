package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
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
	Process    kernel.Process `json:"process"`
	History    []Message      `json:"history"`
	MemoryRefs []MemoryRef    `json:"memory_refs,omitempty"`
}

type GenerateAction struct {
	Type string `json:"type"`
	// Notes is the agent's working memory for this step. Hidden reasoning is
	// discarded after each step, so notes are what carries a plan forward.
	Notes      string             `json:"notes,omitempty"`
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
	MemoryRefs          []MemoryRef   `json:"memory_refs,omitempty"`
	MemorySequence      int           `json:"memory_sequence,omitempty"`
}

type MemoryRef struct {
	Path         string `json:"path"`
	Summary      string `json:"summary,omitempty"`
	MessageCount int    `json:"message_count"`
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
	Leases          *lease.Manager
	Backend         Backend
	DelegateHandler DelegateHandler
	MaxSteps        int
	// Compaction defaults to DefaultCompactionPolicy when left zero.
	Compaction CompactionPolicy
}

// CompactionPolicy decides when older history is compacted. A zero
// MaxMessages or MaxChars disables that trigger.
type CompactionPolicy struct {
	MaxMessages  int
	MaxChars     int
	KeepMessages int
}

// EstimatedCharsPerToken is a conservative chars-per-token ratio for the
// code- and JSON-heavy prompts agents see.
const EstimatedCharsPerToken = 3

func DefaultCompactionPolicy() CompactionPolicy {
	return CompactionPolicy{
		MaxMessages:  sessionMemoryMaxMessages,
		MaxChars:     sessionMemoryMaxChars,
		KeepMessages: sessionMemoryKeepMessages,
	}
}

// ContextCompactionPolicy compacts once history reaches ratio of a context
// window of windowTokens tokens, keeping the most recent messages live.
func ContextCompactionPolicy(windowTokens int, ratio float64) CompactionPolicy {
	return CompactionPolicy{
		MaxChars:     int(float64(windowTokens)*ratio) * EstimatedCharsPerToken,
		KeepMessages: sessionMemoryKeepMessages,
	}
}

// ShouldCompact reports whether messages exceed either policy trigger.
func (p CompactionPolicy) ShouldCompact(messages []Message) bool {
	if p.MaxMessages > 0 && len(messages) > p.MaxMessages {
		return true
	}
	if p.MaxChars <= 0 {
		return false
	}
	totalChars := 0
	for _, message := range messages {
		totalChars += len(message.Role) + len(message.Content)
		if totalChars > p.MaxChars {
			return true
		}
	}
	return false
}

func (r *AgentRuntime) compactionPolicy() CompactionPolicy {
	if r.Compaction == (CompactionPolicy{}) {
		return DefaultCompactionPolicy()
	}
	return r.Compaction
}

const (
	sessionMemoryKeepMessages = 12
	sessionMemoryMaxMessages  = 24
	sessionMemoryMaxChars     = 8000
	sessionMemoryDirName      = ".aos-memory"
	sessionMemorySummaryLimit = 180
)

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
	handleMalformed := func() (bool, error) {
		if options.ContinueOnToolError {
			artifactRecord, recordErr := r.Artifacts.CreateJSON("backend_error", pid, map[string]string{
				"error": ErrMalformedBackendOutput.Error(),
			}, nil, map[string]string{
				"kind": "backend_error",
			})
			if recordErr != nil {
				return false, recordErr
			}
			session.ToolArtifactIDs = append(session.ToolArtifactIDs, artifactRecord.ID)
			session.Messages = append(session.Messages, Message{
				Role:    "tool",
				Content: "tool_error name=backend error=malformed_backend_output try_different_strategy=true",
			})
			return true, nil
		}
		return false, nil
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
		session, err = r.compactSessionMemory(process, session)
		if err != nil {
			return RunResult{}, err
		}

		result, err := r.Backend.Generate(ctx, GenerateRequest{
			Process:    process,
			History:    append([]Message(nil), session.Messages...),
			MemoryRefs: append([]MemoryRef(nil), session.MemoryRefs...),
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

		// Record what the agent decided before acting, so later steps see their
		// own notes and actions, not only tool results.
		if record := actionRecord(result.Action); record != "" {
			session.Messages = append(session.Messages, Message{Role: "assistant", Content: record})
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
				if continued, err := handleMalformed(); err != nil {
					return RunResult{}, err
				} else if continued {
					break
				}
				return r.failMalformed(pid, checkpointID)
			}
			if options.OnToolCall != nil {
				options.OnToolCall(*result.Action.ToolCall)
			}
			signature := toolCallSignature(*result.Action.ToolCall)
			loopWarningIssued := lastLoopWarningForSignature(session.ToolAttempts, signature)
			if repeatedUnhelpfulToolCall(session.ToolAttempts, signature) {
				repeatErr := errors.New("repeated unsuccessful tool call suppressed; try different args or another tool")
				if loopWarningIssued {
					repeatErr = errors.New("repeated unsuccessful tool call suppressed after prior loop warning; agent did not adjust")
				}
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
				if loopWarningIssued {
					exitStatus := kernel.ExitStatus{Code: 1, Reason: repeatErr.Error(), Retryable: false}
					artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, nil, map[string]string{
						"kind": "loop_retry_suppressed",
					})
					if err != nil {
						return RunResult{}, err
					}
					checkpointID = artifactRecord.ID
					if _, err := r.Kernel.Exit(pid, exitStatus); err != nil {
						return RunResult{}, err
					}
					return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID, Session: session}, repeatErr
				}
				break
			}
			if repeatedLoopingToolCall(session.ToolAttempts, signature, result.Action.ToolCall.Name) {
				repeatErr := errors.New("repeated successful tool loop suppressed; exit or choose a different action")
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
			toolResult, err := r.executeToolCall(ctx, process, session, checkpointID, *result.Action.ToolCall)
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
				if continued, err := handleMalformed(); err != nil {
					return RunResult{}, err
				} else if continued {
					break
				}
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
				if continued, err := handleMalformed(); err != nil {
					return RunResult{}, err
				} else if continued {
					break
				}
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
				if continued, err := handleMalformed(); err != nil {
					return RunResult{}, err
				} else if continued {
					break
				}
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
				if continued, err := handleMalformed(); err != nil {
					return RunResult{}, err
				} else if continued {
					break
				}
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
				if continued, err := handleMalformed(); err != nil {
					return RunResult{}, err
				} else if continued {
					break
				}
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
			if continued, err := handleMalformed(); err != nil {
				return RunResult{}, err
			} else if continued {
				break
			}
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
	case "list_artifacts":
		if strings.TrimSpace(result.Output["count"]) == "0" {
			return false, "no_artifacts"
		}
	case "acquire_lease":
		if strings.TrimSpace(result.Output["held"]) == "true" {
			return false, "lease_held"
		}
	}
	return true, ""
}

const (
	maxActionNotesChars = 2000
	maxActionArgChars   = 300
)

// bulkToolArgs hold file content; their size is recorded instead of the text,
// which already lives on disk.
var bulkToolArgs = map[string]bool{"content": true, "old_text": true, "new_text": true}

// actionRecord summarizes an action and its notes for the session history.
// Message actions are already recorded verbatim, so only their notes are kept.
func actionRecord(action GenerateAction) string {
	var lines []string
	if notes := strings.TrimSpace(action.Notes); notes != "" {
		lines = append(lines, "notes: "+truncateRecord(notes, maxActionNotesChars))
	}
	if summary := actionSummary(action); summary != "" {
		lines = append(lines, "action: "+summary)
	}
	return strings.Join(lines, "\n")
}

func actionSummary(action GenerateAction) string {
	switch action.Type {
	case "tool":
		if action.ToolCall == nil || action.ToolCall.Name == "" {
			return ""
		}
		keys := make([]string, 0, len(action.ToolCall.Args))
		for key := range action.ToolCall.Args {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := []string{"tool " + action.ToolCall.Name}
		for _, key := range keys {
			value := action.ToolCall.Args[key]
			if bulkToolArgs[key] {
				parts = append(parts, fmt.Sprintf("%s=<%d bytes>", key, len(value)))
				continue
			}
			parts = append(parts, key+"="+truncateRecord(value, maxActionArgChars))
		}
		return strings.Join(parts, " ")
	case "delegate":
		if action.Delegate == nil || action.Delegate.AgentID == "" {
			return ""
		}
		return fmt.Sprintf("delegate to=%s goal=%s", action.Delegate.AgentID, truncateRecord(action.Delegate.Goal, maxActionArgChars))
	case "exit":
		if action.ExitStatus == nil {
			return "exit"
		}
		return fmt.Sprintf("exit code=%d reason=%s", action.ExitStatus.Code, truncateRecord(action.ExitStatus.Reason, maxActionArgChars))
	case "message", "":
		return ""
	default:
		return action.Type
	}
}

func truncateRecord(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "..."
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

func lastLoopWarningForSignature(attempts []ToolAttempt, signature string) bool {
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		if attempt.Signature != signature {
			continue
		}
		return strings.Contains(attempt.Error, "repeated successful tool loop suppressed")
	}
	return false
}

func repeatedLoopingToolCall(attempts []ToolAttempt, signature, name string) bool {
	if !loopSensitiveTool(name) || len(attempts) < 4 {
		return repeatedIdenticalSuccessfulToolCall(attempts, signature, name)
	}
	recent := attempts[len(attempts)-4:]
	for _, attempt := range recent {
		if !attempt.Useful || !loopSensitiveTool(attempt.Name) {
			return repeatedIdenticalSuccessfulToolCall(attempts, signature, name)
		}
	}
	a := recent[0]
	b := recent[1]
	c := recent[2]
	d := recent[3]
	if a.Signature != c.Signature || b.Signature != d.Signature {
		return repeatedIdenticalSuccessfulToolCall(attempts, signature, name)
	}
	if a.Signature == b.Signature {
		return repeatedIdenticalSuccessfulToolCall(attempts, signature, name)
	}
	return signature == a.Signature
}

func repeatedIdenticalSuccessfulToolCall(attempts []ToolAttempt, signature, name string) bool {
	if !loopSensitiveTool(name) || len(attempts) < 3 {
		return false
	}
	recent := attempts[len(attempts)-3:]
	for _, attempt := range recent {
		if !attempt.Useful || attempt.Name != name || attempt.Signature != signature {
			return false
		}
	}
	return true
}

func loopSensitiveTool(name string) bool {
	switch name {
	case "read_file", "write_file", "edit_file", "list_dir":
		return true
	default:
		return false
	}
}

func (r *AgentRuntime) compactSessionMemory(process kernel.Process, session Session) (Session, error) {
	policy := r.compactionPolicy()
	if !policy.ShouldCompact(session.Messages) {
		return session, nil
	}
	if len(session.Messages) <= policy.KeepMessages {
		return session, nil
	}

	root, ok := compactibleMemoryRoot(process)
	if !ok {
		return session, nil
	}

	cut := len(session.Messages) - policy.KeepMessages
	older := append([]Message(nil), session.Messages[:cut]...)
	recent := append([]Message(nil), session.Messages[cut:]...)
	ref, err := persistSessionMemory(root, process.PID, session.MemorySequence+1, older)
	if err != nil {
		return Session{}, err
	}
	session.MemoryRefs = append(session.MemoryRefs, ref)
	session.MemorySequence++
	session.Messages = recent
	return session, nil
}

func compactibleMemoryRoot(process kernel.Process) (string, bool) {
	evaluator := capability.Evaluator{}
	for _, candidate := range process.CapabilitySet.WritePaths {
		candidate = filepath.Clean(candidate)
		if evaluator.CanWrite(candidate, process.Mounts, process.CapabilitySet) && evaluator.CanRead(candidate, process.Mounts, process.CapabilitySet) {
			return candidate, true
		}
	}
	return "", false
}

func persistSessionMemory(root, pid string, sequence int, messages []Message) (MemoryRef, error) {
	dir := filepath.Join(root, sessionMemoryDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return MemoryRef{}, err
	}
	fileName := fmt.Sprintf("%s-memory-%04d.json", sanitizeMemoryFileComponent(pid), sequence)
	path := filepath.Join(dir, fileName)
	payload := struct {
		ProcessID    string    `json:"process_id"`
		Sequence     int       `json:"sequence"`
		MessageCount int       `json:"message_count"`
		Summary      string    `json:"summary,omitempty"`
		Messages     []Message `json:"messages"`
	}{
		ProcessID:    pid,
		Sequence:     sequence,
		MessageCount: len(messages),
		Summary:      summarizeMemoryMessages(messages),
		Messages:     messages,
	}
	content, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return MemoryRef{}, err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return MemoryRef{}, err
	}
	return MemoryRef{
		Path:         path,
		Summary:      payload.Summary,
		MessageCount: len(messages),
	}, nil
}

func sanitizeMemoryFileComponent(value string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-")
	return replacer.Replace(strings.TrimSpace(value))
}

func summarizeMemoryMessages(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	var userItems []string
	var assistantItems []string
	var toolItems []string
	for _, message := range messages {
		item := summarizeMemoryMessage(message)
		if item == "" {
			continue
		}
		switch message.Role {
		case "user":
			userItems = appendUniqueMemoryItem(userItems, item)
		case "assistant":
			assistantItems = appendUniqueMemoryItem(assistantItems, item)
		default:
			toolItems = appendUniqueMemoryItem(toolItems, item)
		}
	}

	var sections []string
	if len(userItems) > 0 {
		sections = append(sections, "Earlier user instructions: "+strings.Join(userItems, " | "))
	}
	if len(assistantItems) > 0 {
		sections = append(sections, "Earlier assistant decisions/messages: "+strings.Join(assistantItems, " | "))
	}
	if len(toolItems) > 0 {
		sections = append(sections, "Earlier tool/delegation outcomes: "+strings.Join(toolItems, " | "))
	}
	return strings.Join(sections, "\n")
}

func summarizeMemoryMessage(message Message) string {
	content := strings.Join(strings.Fields(strings.TrimSpace(message.Content)), " ")
	if content == "" {
		return ""
	}
	return truncateMemorySummary(content, sessionMemorySummaryLimit)
}

func appendUniqueMemoryItem(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	if len(items) >= 6 {
		return items
	}
	return append(items, item)
}

func truncateMemorySummary(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func (r *AgentRuntime) RestoreCheckpoint(id string) (Session, error) {
	var session Session
	err := r.Artifacts.ReadJSON(id, &session)
	return session, err
}

func (r *AgentRuntime) executeToolCall(ctx context.Context, process kernel.Process, session Session, checkpointID string, call ToolCall) (tools.Result, error) {
	if isRuntimeTool(call.Name) {
		return r.executeRuntimeTool(ctx, process, session, checkpointID, call)
	}
	return r.Tools.Execute(tools.Request{
		Tool:          call.Name,
		Args:          call.Args,
		CapabilitySet: process.CapabilitySet,
		Mounts:        process.Mounts,
	})
}

func isRuntimeTool(name string) bool {
	switch name {
	case "spawn_agent", "message_agent", "await_agent", "checkpoint", "list_artifacts", "publish_artifact", "acquire_lease", "release_lease":
		return true
	default:
		return false
	}
}

func (r *AgentRuntime) executeRuntimeTool(ctx context.Context, process kernel.Process, session Session, checkpointID string, call ToolCall) (tools.Result, error) {
	evaluator := capability.Evaluator{}
	if !evaluator.ToolAllowed(call.Name, process.CapabilitySet) {
		return tools.Result{}, tools.ErrToolDenied
	}

	switch call.Name {
	case "spawn_agent":
		if r.DelegateHandler == nil {
			return tools.Result{}, errors.New("delegate handler not configured")
		}
		agentID := strings.TrimSpace(call.Args["agent_id"])
		if agentID == "" {
			return tools.Result{}, errors.New("spawn_agent requires agent_id")
		}
		result, err := r.DelegateHandler(ctx, process, DelegateCall{
			AgentID: agentID,
			Goal:    call.Args["goal"],
		}, session)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "spawned agent",
			Output: map[string]string{
				"agent_id":      result.AgentID,
				"pid":           result.PID,
				"summary":       result.Summary,
				"result_id":     result.ResultID,
				"checkpoint_id": result.CheckpointID,
			},
		}, nil
	case "message_agent":
		if r.Bus == nil {
			return tools.Result{}, errors.New("mailbox bus not configured")
		}
		to := strings.TrimSpace(call.Args["to"])
		if to == "" {
			to = strings.TrimSpace(call.Args["pid"])
		}
		if to == "" {
			return tools.Result{}, errors.New("message_agent requires to")
		}
		message, err := r.Bus.Send(process.PID, to, call.Args["body"])
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "sent agent message",
			Output: map[string]string{
				"id":   message.ID,
				"to":   message.To,
				"body": message.Body,
			},
		}, nil
	case "await_agent":
		pid := strings.TrimSpace(call.Args["pid"])
		if pid == "" {
			return tools.Result{}, errors.New("await_agent requires pid")
		}
		child, err := r.Kernel.Get(pid)
		if err != nil {
			return tools.Result{}, err
		}
		result := tools.Result{
			Tool:    call.Name,
			Summary: "awaited agent",
			Output: map[string]string{
				"pid":   pid,
				"state": string(child.State),
			},
		}
		if child.ExitStatus != nil {
			result.Output["code"] = fmt.Sprintf("%d", child.ExitStatus.Code)
			result.Output["reason"] = child.ExitStatus.Reason
			result.Output["retryable"] = fmt.Sprintf("%t", child.ExitStatus.Retryable)
		}
		return result, nil
	case "checkpoint":
		record, err := r.Artifacts.CreateJSON("checkpoint", process.PID, session, nil, map[string]string{
			"kind": "manual",
		})
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "created checkpoint",
			Output: map[string]string{
				"checkpoint_id": record.ID,
			},
		}, nil
	case "list_artifacts":
		items, err := r.Artifacts.List()
		if err != nil {
			return tools.Result{}, err
		}
		var filtered []artifact.Artifact
		typeFilter := strings.TrimSpace(call.Args["type"])
		pidFilter := strings.TrimSpace(call.Args["producer_pid"])
		if pidFilter == "" && strings.EqualFold(strings.TrimSpace(call.Args["scope"]), "process") {
			pidFilter = process.PID
		}
		for _, item := range items {
			if typeFilter != "" && item.Type != typeFilter {
				continue
			}
			if pidFilter != "" && item.ProducerPID != pidFilter {
				continue
			}
			filtered = append(filtered, item)
		}
		sort.Slice(filtered, func(i, j int) bool {
			return filtered[i].CreatedAt.Before(filtered[j].CreatedAt)
		})
		lines := make([]string, 0, len(filtered))
		for _, item := range filtered {
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s", item.ID, item.Type, item.ProducerPID, item.CreatedAt.Format(time.RFC3339)))
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "listed artifacts",
			Output: map[string]string{
				"count":     fmt.Sprintf("%d", len(filtered)),
				"artifacts": strings.Join(lines, "\n"),
			},
		}, nil
	case "publish_artifact":
		artifactType := strings.TrimSpace(call.Args["type"])
		if artifactType == "" {
			artifactType = "note"
		}
		body := call.Args["body"]
		record, err := r.Artifacts.CreateJSON(artifactType, process.PID, map[string]string{
			"body": body,
		}, nil, map[string]string{
			"kind": "published",
		})
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "published artifact",
			Output: map[string]string{
				"artifact_id": record.ID,
				"type":        record.Type,
				"path":        record.Path,
			},
		}, nil
	case "acquire_lease":
		if r.Leases == nil {
			return tools.Result{}, errors.New("lease manager not configured")
		}
		resource := strings.TrimSpace(call.Args["resource"])
		if resource == "" {
			return tools.Result{}, errors.New("acquire_lease requires resource")
		}
		ttl := 10 * time.Minute
		if raw := strings.TrimSpace(call.Args["ttl"]); raw != "" {
			if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
				ttl = parsed
			}
		}
		leaseRecord, err := r.Leases.Acquire(resource, process.PID, ttl)
		if err != nil {
			if errors.Is(err, lease.ErrLeaseHeld) {
				if current, ok, getErr := r.Leases.Get(resource); getErr == nil && ok {
					return tools.Result{
						Tool:    call.Name,
						Summary: "lease held",
						Output: map[string]string{
							"resource":   resource,
							"owner_pid":  current.OwnerPID,
							"expires_at": current.ExpiresAt.Format(time.RFC3339),
							"held":       "true",
						},
					}, nil
				}
			}
			return tools.Result{}, err
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "lease acquired",
			Output: map[string]string{
				"resource":   leaseRecord.Resource,
				"owner_pid":  leaseRecord.OwnerPID,
				"expires_at": leaseRecord.ExpiresAt.Format(time.RFC3339),
				"held":       "false",
			},
		}, nil
	case "release_lease":
		if r.Leases == nil {
			return tools.Result{}, errors.New("lease manager not configured")
		}
		resource := strings.TrimSpace(call.Args["resource"])
		if resource == "" {
			return tools.Result{}, errors.New("release_lease requires resource")
		}
		if err := r.Leases.Release(resource, process.PID); err != nil {
			return tools.Result{}, err
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "lease released",
			Output: map[string]string{
				"resource": resource,
			},
		}, nil
	default:
		return tools.Result{}, errors.New("unknown runtime tool")
	}
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
