package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/tools"
)

var ErrMalformedBackendOutput = errors.New("malformed backend output")
var ErrAgentBlocked = errors.New("agent blocked waiting for input")

// ErrNotACheckpoint: a restore was asked to use an artifact that is not a
// checkpoint.
var ErrNotACheckpoint = errors.New("artifact is not a checkpoint")

// ErrCheckpointNotOwned: the checkpoint was produced by a process outside
// the resuming process's resumed_from chain.
var ErrCheckpointNotOwned = errors.New("checkpoint belongs to another process")

// maxResumeChain bounds the resumed_from walk.
const maxResumeChain = 64

// messageRecordedNote follows a message action in an autonomous run.
const messageRecordedNote = "message_recorded note=your last message is your result and reaches whoever started you when you exit; if your task is done, exit now instead of repeating the message"

// ErrRunCancelled reports that the caller's context ended a run. The session
// is checkpointed and the process left ready, so Resume continues it.
var ErrRunCancelled = errors.New("run cancelled")

// ErrProcessExited: a run was asked of a process that has already exited.
// Nothing runs; Recover makes a new process from its checkpoint.
var ErrProcessExited = errors.New("process has exited")

// ErrNotRecoverable: Recover (or Resume of an exited process) was asked of
// a process that is still live or whose exit is not retryable.
var ErrNotRecoverable = errors.New("process is not recoverable")

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

// MemoryRef points at a compacted stretch of history: a memory_segment
// artifact the runtime owns, read back with the read_memory tool. Path is
// only set on refs from before segments were artifacts.
type MemoryRef struct {
	ArtifactID   string `json:"artifact_id,omitempty"`
	Path         string `json:"path,omitempty"`
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
	Cancelled    bool              `json:"cancelled,omitempty"`
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
	// Clock, when set, adds a time-budget note to the history each step.
	Clock *Clock
	// OnCapabilityDenied, when set, receives each capability.denied kernel
	// event this runtime records.
	OnCapabilityDenied func(kernel.Event)
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
	sessionMemorySummaryLimit = 180
)

type DelegateResult struct {
	AgentID string `json:"agent_id"`
	PID     string `json:"pid"`
	// ExitStatus is the child's recorded exit, so the parent sees whether it
	// succeeded as data rather than inferring it from Summary.
	ExitStatus   kernel.ExitStatus `json:"exit_status"`
	Summary      string            `json:"summary"`
	ResultID     string            `json:"result_id,omitempty"`
	CheckpointID string            `json:"checkpoint_id,omitempty"`
}

type DelegateHandler func(ctx context.Context, parent kernel.Process, call DelegateCall, session Session) (DelegateResult, error)

func (r *AgentRuntime) Run(ctx context.Context, pid string, initial []Message) (RunResult, error) {
	return r.runSession(ctx, pid, Session{Messages: append([]Message(nil), initial...)}, RunOptions{}, "")
}

func (r *AgentRuntime) RunWithOptions(ctx context.Context, pid string, initial []Message, options RunOptions) (RunResult, error) {
	return r.runSession(ctx, pid, Session{Messages: append([]Message(nil), initial...)}, options, "")
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
	session, err := r.ownedCheckpoint(pid, checkpointID)
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
		return r.Recover(ctx, pid, checkpointID, appended, options)
	}
	return r.runSession(ctx, pid, session, options, checkpointID)
}

// Recover runs a new process from an exited one's checkpoint. Only an
// exited process with a retryable record (runtime- or agent-origin) can be
// recovered; a live one never can, so two processes never run the same
// work. The new process takes over the old one's leases.
func (r *AgentRuntime) Recover(ctx context.Context, pid, checkpointID string, appended []Message, options RunOptions) (RunResult, error) {
	process, err := r.Kernel.Get(pid)
	if err != nil {
		return RunResult{}, err
	}
	if err := recoverable(process); err != nil {
		return RunResult{}, err
	}
	session, err := r.ownedCheckpoint(pid, checkpointID)
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
	if r.Leases != nil {
		if _, err := r.Leases.Transfer(pid, recovered.PID); err != nil {
			status := kernel.ExitStatus{Code: 1, Reason: "lease transfer: " + err.Error(), Retryable: true, Checkpoint: checkpointID}
			_, _ = r.Kernel.Exit(recovered.PID, status)
			return RunResult{PID: recovered.PID, ExitStatus: status, CheckpointID: checkpointID}, err
		}
	}
	return r.runSession(ctx, recovered.PID, session, options, checkpointID)
}

// recoverable refuses a process that is live or whose exit is final.
func recoverable(process kernel.Process) error {
	if process.State != kernel.StateExited || process.ExitStatus == nil {
		return fmt.Errorf("%w: %s is %s; only an exited process can be recovered", ErrNotRecoverable, process.PID, process.State)
	}
	if !process.ExitStatus.Retryable {
		return fmt.Errorf("%w: %s exited with a non-retryable status: %s", ErrNotRecoverable, process.PID, process.ExitStatus.Reason)
	}
	return nil
}

// runSession runs pid from session. resumedFrom is the checkpoint the
// session was restored from, if any; it heads the run's checkpoint chain.
// An exited process is refused before anything runs. Every run ends in a
// recorded exit or a checkpointed park (yield, wait, cancel); closeRun
// exits any path that returned while the process was still running.
func (r *AgentRuntime) runSession(ctx context.Context, pid string, session Session, options RunOptions, resumedFrom string) (RunResult, error) {
	r.bindArtifactProducers()
	process, err := r.Kernel.Get(pid)
	if err != nil {
		return RunResult{}, err
	}
	if process.State == kernel.StateExited {
		return RunResult{}, fmt.Errorf("%w: %s", ErrProcessExited, pid)
	}
	track := runTrack{checkpointID: resumedFrom, session: session}
	result, runErr := r.runSteps(ctx, process, session, options, resumedFrom, &track)
	return r.closeRun(pid, track, result, runErr)
}

// runTrack is the latest checkpoint and session runSteps had, for closeRun.
type runTrack struct {
	checkpointID string
	session      Session
}

// closeRun exits pid if runSteps returned while it was still running, from
// an error (artifact store, kernel, bus) or a path that forgot to close.
// The exit is runtime-origin, names the latest checkpoint, and is
// retryable unless the error is a contract violation. Exits and parks
// runSteps recorded itself pass through.
func (r *AgentRuntime) closeRun(pid string, track runTrack, result RunResult, runErr error) (RunResult, error) {
	process, err := r.Kernel.Get(pid)
	if err != nil {
		return result, errors.Join(runErr, err)
	}
	if process.State != kernel.StateRunning {
		return result, runErr
	}
	if runErr == nil {
		runErr = errors.New("run returned without exiting or parking")
	}
	checkpointID := track.checkpointID
	if result.CheckpointID != "" {
		checkpointID = result.CheckpointID
	}
	status := kernel.ExitStatus{Code: 1, Reason: runErr.Error(), Retryable: retryableError(runErr), Checkpoint: checkpointID}
	if _, err := r.Kernel.Exit(pid, status); err != nil {
		return result, errors.Join(runErr, err)
	}
	return RunResult{PID: pid, ExitStatus: status, CheckpointID: checkpointID, Session: track.session}, runErr
}

// retryableError reports whether a run that failed with err may succeed if
// recovered: infrastructure failures (storage, kernel, bus, backend) may;
// contract violations, such as a tampered or foreign artifact or an
// invalid transition, will fail the same way again.
func retryableError(err error) bool {
	for _, violation := range []error{
		artifact.ErrArtifactTampered, artifact.ErrReservedType, artifact.ErrUnknownParent, artifact.ErrUnknownProducer,
		ErrNotACheckpoint, ErrCheckpointNotOwned, ErrMalformedBackendOutput, kernel.ErrInvalidTransition,
	} {
		if errors.Is(err, violation) {
			return false
		}
	}
	return true
}

// runSteps is the agent loop. Exits it records itself (agent exit, step
// limit, refusals) name the latest checkpoint; anything else that returns
// with the process running is closed by closeRun.
func (r *AgentRuntime) runSteps(ctx context.Context, process kernel.Process, session Session, options RunOptions, resumedFrom string, track *runTrack) (RunResult, error) {
	pid := process.PID
	var err error
	if process.State == kernel.StateReady || process.State == kernel.StateBlocked || process.State == kernel.StateSleeping {
		if _, err := r.Kernel.Transition(pid, kernel.StateRunning); err != nil {
			return RunResult{}, err
		}
	}

	var checkpointID string
	defer func() {
		if checkpointID != "" {
			track.checkpointID = checkpointID
		}
		track.session = session
	}()
	// lastCheckpoint is the newest checkpoint of this run, or the one it
	// resumed from; latest gives it as the parent of new checkpoints and
	// records, so a run's artifacts form a chain an operator can walk.
	lastCheckpoint := func() string {
		if checkpointID != "" {
			return checkpointID
		}
		return resumedFrom
	}
	latest := func() []string {
		return nonEmpty(lastCheckpoint())
	}

	maxSteps := r.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 32
	}
	handleMalformed := func() (bool, error) {
		if options.ContinueOnToolError {
			artifactRecord, recordErr := r.Artifacts.CreateJSON("backend_error", pid, map[string]string{
				"error": ErrMalformedBackendOutput.Error(),
			}, latest(), map[string]string{
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
		if ctx.Err() != nil {
			return r.cancelRun(ctx, pid, session, latest())
		}
		if r.Clock != nil {
			session.Messages = append(session.Messages, Message{Role: "tool", Content: r.Clock.Note()})
		}
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
		session, err = r.compactSessionMemory(process, session, latest())
		if err != nil {
			return RunResult{}, err
		}

		callCtx := WithCallInfo(ctx, CallInfo{PID: pid, AgentID: process.Metadata["agent_id"]})
		result, err := r.Backend.Generate(callCtx, GenerateRequest{
			Process:    process,
			History:    append([]Message(nil), session.Messages...),
			MemoryRefs: append([]MemoryRef(nil), session.MemoryRefs...),
		})
		if ctx.Err() != nil {
			// Drop whatever the interrupted call produced, action or error.
			return r.cancelRun(ctx, pid, session, latest())
		}
		if err != nil {
			if options.ContinueOnToolError {
				artifactRecord, recordErr := r.Artifacts.CreateJSON("backend_error", pid, map[string]string{
					"error": err.Error(),
				}, latest(), map[string]string{
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
			exitStatus.Checkpoint = lastCheckpoint()
			if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
				return RunResult{}, exitErr
			}
			return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: lastCheckpoint()}, err
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
			artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, latest(), map[string]string{
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
			// Nobody answers a message in an autonomous run. Without this
			// note, agents told to "return X in a message, then exit" kept
			// resending the message until their step limit.
			session.Messages = append(session.Messages, Message{Role: "tool", Content: messageRecordedNote})
		case "tool":
			if result.Action.ToolCall == nil || result.Action.ToolCall.Name == "" {
				if continued, err := handleMalformed(); err != nil {
					return RunResult{}, err
				} else if continued {
					break
				}
				return r.failMalformed(pid, lastCheckpoint())
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
				}, latest(), map[string]string{
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
					artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, latest(), map[string]string{
						"kind": "loop_retry_suppressed",
					})
					if err != nil {
						return RunResult{}, err
					}
					checkpointID = artifactRecord.ID
					exitStatus.Checkpoint = lastCheckpoint()
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
				}, latest(), map[string]string{
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
				if ctx.Err() != nil {
					session.Messages = append(session.Messages, Message{
						Role:    "tool",
						Content: fmt.Sprintf("tool_cancelled name=%s reason=interrupted_by_user", result.Action.ToolCall.Name),
					})
					return r.cancelRun(ctx, pid, session, latest())
				}
				if options.ContinueOnToolError {
					artifactRecord, recordErr := r.Artifacts.CreateJSON("tool_error", pid, map[string]string{
						"tool":  result.Action.ToolCall.Name,
						"error": err.Error(),
					}, latest(), map[string]string{
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
				exitStatus.Checkpoint = lastCheckpoint()
				if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
					return RunResult{}, exitErr
				}
				return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: lastCheckpoint()}, err
			}
			artifactRecord, err := r.Artifacts.CreateJSON("tool_result", pid, toolResult, latest(), map[string]string{
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
				return r.failMalformed(pid, lastCheckpoint())
			}
			if err := r.authorize(process, "spawn_agent", "delegate"); err != nil {
				if continued, failed, failErr := r.refuseAction(pid, lastCheckpoint(), &session, "delegate", err, options); !continued {
					return failed, failErr
				}
				break
			}
			if r.DelegateHandler == nil {
				exitStatus := kernel.ExitStatus{Code: 1, Reason: "delegate handler not configured", Retryable: false}
				exitStatus.Checkpoint = lastCheckpoint()
				if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
					return RunResult{}, exitErr
				}
				return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: lastCheckpoint()}, errors.New(exitStatus.Reason)
			}
			delegateResult, err := r.DelegateHandler(ctx, process, *result.Action.Delegate, session)
			if err != nil && ctx.Err() != nil {
				session.Messages = append(session.Messages, Message{
					Role:    "tool",
					Content: fmt.Sprintf("delegate_cancelled agent=%s reason=interrupted_by_user", result.Action.Delegate.AgentID),
				})
				return r.cancelRun(ctx, pid, session, latest())
			}
			if err != nil {
				exitStatus := kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: false}
				exitStatus.Checkpoint = lastCheckpoint()
				if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
					return RunResult{}, exitErr
				}
				return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: lastCheckpoint()}, err
			}
			delegateArtifact, err := r.Artifacts.CreateJSON("delegate_result", pid, delegateResult, nonEmpty(delegateResult.CheckpointID), map[string]string{
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
					"delegate_result agent=%s pid=%s exit_code=%d exit_reason=%q exit_retryable=%t summary=%s result_artifact=%s checkpoint=%s",
					delegateResult.AgentID,
					delegateResult.PID,
					delegateResult.ExitStatus.Code,
					delegateResult.ExitStatus.Reason,
					delegateResult.ExitStatus.Retryable,
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
				return r.failMalformed(pid, lastCheckpoint())
			}
			if err := r.authorize(process, "message_agent", "send"); err != nil {
				if continued, failed, failErr := r.refuseAction(pid, lastCheckpoint(), &session, "send", err, options); !continued {
					return failed, failErr
				}
				break
			}
			if r.Bus == nil {
				return r.failRuntime(pid, lastCheckpoint(), errors.New("mailbox bus not configured"))
			}
			message, err := r.Bus.Send(pid, result.Action.Send.To, result.Action.Send.Body)
			if err != nil {
				return r.failRuntime(pid, lastCheckpoint(), err)
			}
			artifactRecord, err := r.Artifacts.CreateJSON("mailbox_message", pid, message, latest(), map[string]string{
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
				return r.failMalformed(pid, lastCheckpoint())
			}
			if err := r.authorize(process, "message_agent", "publish"); err != nil {
				if continued, failed, failErr := r.refuseAction(pid, lastCheckpoint(), &session, "publish", err, options); !continued {
					return failed, failErr
				}
				break
			}
			if r.Bus == nil {
				return r.failRuntime(pid, lastCheckpoint(), errors.New("mailbox bus not configured"))
			}
			messages, err := r.Bus.Publish(pid, result.Action.Publish.Topic, result.Action.Publish.Body)
			if err != nil {
				return r.failRuntime(pid, lastCheckpoint(), err)
			}
			artifactRecord, err := r.Artifacts.CreateJSON("mailbox_publish", pid, messages, latest(), map[string]string{
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
				return r.failMalformed(pid, lastCheckpoint())
			}
			if err := r.authorize(process, "message_agent", "subscribe"); err != nil {
				if continued, failed, failErr := r.refuseAction(pid, lastCheckpoint(), &session, "subscribe", err, options); !continued {
					return failed, failErr
				}
				break
			}
			if r.Bus == nil {
				return r.failRuntime(pid, lastCheckpoint(), errors.New("mailbox bus not configured"))
			}
			if err := r.Bus.Subscribe(pid, result.Action.Subscribe.Topic); err != nil {
				return r.failRuntime(pid, lastCheckpoint(), err)
			}
			artifactRecord, err := r.Artifacts.CreateJSON("mailbox_subscription", pid, result.Action.Subscribe, latest(), map[string]string{
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
			// A question asked while waiting is the agent's reply to the user.
			if result.Action.Message != "" {
				if options.OnAssistantMessage != nil {
					options.OnAssistantMessage(result.Action.Message)
				}
				session.Messages = append(session.Messages, Message{Role: "assistant", Content: result.Action.Message})
			}
			artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, latest(), map[string]string{
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
				return r.failMalformed(pid, lastCheckpoint())
			}
			artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, latest(), map[string]string{
				"kind": "final",
			})
			if err != nil {
				return RunResult{}, err
			}
			checkpointID = artifactRecord.ID
			// The one place an agent's own exit is recorded; Origin marks it so
			// whatever the model put there is overwritten.
			agentExit := *result.Action.ExitStatus
			agentExit.Origin = kernel.ExitOriginAgent
			agentExit.Checkpoint = checkpointID
			process, err = r.Kernel.Exit(pid, agentExit)
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
			return r.failMalformed(pid, lastCheckpoint())
		}

		artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, latest(), map[string]string{
			"kind": "intermediate",
		})
		if err != nil {
			return RunResult{}, err
		}
		checkpointID = artifactRecord.ID
	}

	// Checkpoint the last steps too: ones that ended in a backend error
	// continued without one, and may be every step the agent took.
	artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, latest(), map[string]string{
		"kind": "final",
	})
	if err != nil {
		return RunResult{}, err
	}
	checkpointID = artifactRecord.ID
	exitStatus := kernel.ExitStatus{Code: 1, Reason: "step limit exceeded", Retryable: true, Checkpoint: checkpointID}
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

// actionRecord renders an action for the session history exactly as the
// agent sent it, in the action JSON the agent is asked to produce. Nothing is
// elided: models copy whatever shape their own past turns have, so a summary
// or placeholder here gets written back as real output. Message actions are
// recorded verbatim elsewhere and return "".
func actionRecord(action GenerateAction) string {
	if !recordableAction(action) {
		return ""
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(GenerateResult{Action: action}); err != nil {
		return ""
	}
	return strings.TrimSpace(buffer.String())
}

// actionRecordPrefix starts every action record actionRecord writes.
const actionRecordPrefix = `{"action":`

// IsActionRecord reports whether an assistant history entry is the record
// of a non-message action (tool, delegate, exit, ...) rather than something
// the agent said. A child's result is its last real message.
func IsActionRecord(content string) bool {
	return strings.HasPrefix(content, actionRecordPrefix)
}

// WaitText is what a run that ended in a wait has to say: the message the
// agent asked with, or failing that the notes of its wait action.
func WaitText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Role != "assistant" {
			continue
		}
		if !IsActionRecord(message.Content) {
			return message.Content
		}
		var record GenerateResult
		if json.Unmarshal([]byte(message.Content), &record) == nil && record.Action.Type == "wait" {
			if record.Action.Message != "" {
				return record.Action.Message
			}
			return record.Action.Notes
		}
		return ""
	}
	return ""
}

func recordableAction(action GenerateAction) bool {
	switch action.Type {
	case "tool":
		return action.ToolCall != nil && action.ToolCall.Name != ""
	case "delegate":
		return action.Delegate != nil && action.Delegate.AgentID != ""
	case "message", "":
		return false
	default:
		return true
	}
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

// compactSessionMemory moves older history into a memory_segment artifact
// the runtime owns, cut from the checkpoint named by parents. It needs no
// writable path, the agent's file tools cannot reach it, and every
// compaction gets its own artifact, so resumes never collide.
func (r *AgentRuntime) compactSessionMemory(process kernel.Process, session Session, parents []string) (Session, error) {
	policy := r.compactionPolicy()
	if !policy.ShouldCompact(session.Messages) {
		return session, nil
	}
	if len(session.Messages) <= policy.KeepMessages {
		return session, nil
	}

	cut := len(session.Messages) - policy.KeepMessages
	older := append([]Message(nil), session.Messages[:cut]...)
	recent := append([]Message(nil), session.Messages[cut:]...)
	segment := memorySegment{
		ProcessID:    process.PID,
		Sequence:     session.MemorySequence + 1,
		MessageCount: len(older),
		Summary:      summarizeMemoryMessages(older),
		Messages:     older,
	}
	record, err := r.Artifacts.CreateJSON("memory_segment", process.PID, segment, parents, map[string]string{
		"sequence": strconv.Itoa(segment.Sequence),
	})
	if err != nil {
		return Session{}, err
	}
	session.MemoryRefs = append(session.MemoryRefs, MemoryRef{
		ArtifactID:   record.ID,
		Summary:      segment.Summary,
		MessageCount: segment.MessageCount,
	})
	session.MemorySequence++
	session.Messages = recent
	return session, nil
}

// memorySegment is the payload of a memory_segment artifact.
type memorySegment struct {
	ProcessID    string    `json:"process_id"`
	Sequence     int       `json:"sequence"`
	MessageCount int       `json:"message_count"`
	Summary      string    `json:"summary,omitempty"`
	Messages     []Message `json:"messages"`
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

// cancelRun checkpoints session and parks the process as ready so a later
// Resume picks up where the cancelled run stopped.
func (r *AgentRuntime) cancelRun(ctx context.Context, pid string, session Session, parents []string) (RunResult, error) {
	artifactRecord, err := r.Artifacts.CreateJSON("checkpoint", pid, session, parents, map[string]string{
		"kind": "cancelled",
	})
	if err != nil {
		return RunResult{}, err
	}
	if _, err := r.Kernel.Transition(pid, kernel.StateReady); err != nil {
		return RunResult{}, err
	}
	return RunResult{PID: pid, CheckpointID: artifactRecord.ID, Session: session, Cancelled: true}, fmt.Errorf("%w: %w", ErrRunCancelled, context.Cause(ctx))
}

// RestoreCheckpoint reads a checkpoint artifact; anything else is refused.
// Resume and Recover additionally require the checkpoint to be the
// process's own (ownedCheckpoint).
func (r *AgentRuntime) RestoreCheckpoint(id string) (Session, error) {
	record, err := r.Artifacts.Metadata(id)
	if err != nil {
		return Session{}, err
	}
	if record.Type != "checkpoint" {
		return Session{}, fmt.Errorf("%w: %s is a %s", ErrNotACheckpoint, id, record.Type)
	}
	var session Session
	err = r.Artifacts.ReadJSON(id, &session)
	return session, err
}

// ownedCheckpoint restores id for pid only if it is a checkpoint produced
// by pid or a process pid was recovered from (its resumed_from chain).
func (r *AgentRuntime) ownedCheckpoint(pid, id string) (Session, error) {
	record, err := r.Artifacts.Metadata(id)
	if err != nil {
		return Session{}, err
	}
	if err := r.ownedBy(pid, record); err != nil {
		return Session{}, err
	}
	return r.RestoreCheckpoint(id)
}

// ownedBy reports whether record was produced by pid or its resumed_from
// chain.
func (r *AgentRuntime) ownedBy(pid string, record artifact.Artifact) error {
	current := pid
	for hops := 0; current != "" && hops < maxResumeChain; hops++ {
		if record.ProducerPID == current {
			return nil
		}
		process, err := r.Kernel.Get(current)
		if err != nil {
			return err
		}
		current = process.Metadata["resumed_from"]
	}
	return fmt.Errorf("%w: %s was produced by %s, not %s", ErrCheckpointNotOwned, record.ID, record.ProducerPID, pid)
}

// bindArtifactProducers makes the artifact store refuse producer PIDs the
// kernel does not know.
func (r *AgentRuntime) bindArtifactProducers() {
	if r.Artifacts == nil || r.Kernel == nil {
		return
	}
	k := r.Kernel
	r.Artifacts.BindProducers(func(pid string) error {
		_, err := k.Get(pid)
		return err
	})
}

func (r *AgentRuntime) executeToolCall(ctx context.Context, process kernel.Process, session Session, checkpointID string, call ToolCall) (tools.Result, error) {
	if err := r.authorize(process, call.Name, call.Name); err != nil {
		return tools.Result{}, err
	}
	if isRuntimeTool(call.Name) {
		return r.executeRuntimeTool(ctx, process, session, checkpointID, call)
	}
	result, err := r.Tools.ExecuteContext(ctx, tools.Request{
		Tool:          call.Name,
		Args:          call.Args,
		CapabilitySet: process.CapabilitySet,
		Mounts:        process.Mounts,
		PID:           process.PID,
	})
	switch {
	case errors.Is(err, tools.ErrPathDenied):
		path := call.Args["path"]
		if path == "" {
			path = call.Args["root"]
		}
		r.logDenial(process.PID, call.Name, call.Name, path, "path outside the granted scopes or mounts")
	case errors.Is(err, tools.ErrNetworkDenied):
		r.logDenial(process.PID, call.Name, "network", call.Args["url"], err.Error())
	case errors.Is(err, tools.ErrRunUnconfined):
		r.logDenial(process.PID, call.Name, "unconfined_run", "", err.Error())
	}
	return result, err
}

// authorize is the single capability check shared by tool calls and by the
// delegate (spawn_agent) and send/publish/subscribe (message_agent) actions,
// so no action reaches what its tool form would be denied.
func (r *AgentRuntime) authorize(process kernel.Process, capabilityName, action string) error {
	// read_memory only returns the process's own compacted history, which was
	// already in its context, so every agent may use it.
	if capabilityName == "read_memory" || (capability.Evaluator{}).ToolAllowed(capabilityName, process.CapabilitySet) {
		return nil
	}
	r.logDenial(process.PID, action, capabilityName, "", "capability not granted")
	return fmt.Errorf("%w: %s needs %s", tools.ErrToolDenied, action, capabilityName)
}

// logDenial records a refused capability check in the kernel event log.
func (r *AgentRuntime) logDenial(pid, action, capabilityName, path, reason string) {
	data := map[string]string{"action": action, "capability": capabilityName, "reason": reason}
	if path != "" {
		data["path"] = path
	}
	event, err := r.Kernel.RecordEvent(pid, "capability.denied", data)
	if err == nil && r.OnCapabilityDenied != nil {
		r.OnCapabilityDenied(event)
	}
}

// refuseAction handles a denied action the way a failed tool call is
// handled: the agent sees a tool_error and continues when allowed to,
// otherwise the process exits. continued reports which happened.
func (r *AgentRuntime) refuseAction(pid, checkpointID string, session *Session, action string, err error, options RunOptions) (continued bool, result RunResult, failErr error) {
	if options.OnToolError != nil {
		options.OnToolError(ToolCall{Name: action}, err)
	}
	session.Messages = append(session.Messages, Message{
		Role:    "tool",
		Content: fmt.Sprintf("tool_error name=%s error=%s try_different_strategy=true", action, err.Error()),
	})
	if options.ContinueOnToolError {
		return true, RunResult{}, nil
	}
	exitStatus := kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: false}
	exitStatus.Checkpoint = checkpointID
	if _, exitErr := r.Kernel.Exit(pid, exitStatus); exitErr != nil {
		return false, RunResult{}, exitErr
	}
	return false, RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, err
}

func isRuntimeTool(name string) bool {
	switch name {
	case "spawn_agent", "message_agent", "await_agent", "checkpoint", "list_artifacts", "publish_artifact", "acquire_lease", "release_lease", "read_memory":
		return true
	default:
		return false
	}
}

// executeRuntimeTool runs a runtime tool; executeToolCall has already
// authorized it.
func (r *AgentRuntime) executeRuntimeTool(ctx context.Context, process kernel.Process, session Session, checkpointID string, call ToolCall) (tools.Result, error) {

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
		record, err := r.Artifacts.CreateJSON("checkpoint", process.PID, session, nonEmpty(checkpointID), map[string]string{
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
	case "read_memory":
		id := strings.TrimSpace(call.Args["artifact_id"])
		record, err := r.Artifacts.Metadata(id)
		if err != nil {
			return tools.Result{}, fmt.Errorf("read_memory %q: %w", id, err)
		}
		if record.Type != "memory_segment" {
			return tools.Result{}, fmt.Errorf("read_memory: %s is not a memory segment", id)
		}
		if err := r.ownedBy(process.PID, record); err != nil {
			return tools.Result{}, err
		}
		var segment memorySegment
		if err := r.Artifacts.ReadJSON(id, &segment); err != nil {
			return tools.Result{}, err
		}
		lines := make([]string, 0, len(segment.Messages))
		for _, message := range segment.Messages {
			lines = append(lines, message.Role+": "+message.Content)
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "read memory segment",
			Output: map[string]string{
				"artifact_id":   id,
				"message_count": strconv.Itoa(segment.MessageCount),
				"messages":      strings.Join(lines, "\n"),
			},
		}, nil
	case "publish_artifact":
		requested := strings.TrimPrefix(strings.TrimSpace(call.Args["type"]), artifact.PublishedPrefix)
		if requested == "" {
			requested = "note"
		}
		if artifact.IsReserved(requested) {
			r.logDenial(process.PID, call.Name, call.Name, "", "reserved artifact type "+requested)
			return tools.Result{}, fmt.Errorf("%w: %s", artifact.ErrReservedType, requested)
		}
		body := call.Args["body"]
		// Agent publications live under their own prefix, so they can never
		// pass for a checkpoint, verdict or other runtime record.
		record, err := r.Artifacts.CreateJSON(artifact.PublishedPrefix+requested, process.PID, map[string]string{
			"body": body,
		}, nonEmpty(checkpointID), map[string]string{
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
	exitStatus.Checkpoint = checkpointID
	if _, err := r.Kernel.Exit(pid, exitStatus); err != nil {
		return RunResult{}, err
	}
	return RunResult{PID: pid, ExitStatus: exitStatus, CheckpointID: checkpointID}, ErrMalformedBackendOutput
}

func (r *AgentRuntime) failRuntime(pid, checkpointID string, runErr error) (RunResult, error) {
	exitStatus := kernel.ExitStatus{Code: 1, Reason: runErr.Error(), Retryable: false}
	exitStatus.Checkpoint = checkpointID
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

// nonEmpty is a one-element parent list, or nil for no parent.
func nonEmpty(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
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
