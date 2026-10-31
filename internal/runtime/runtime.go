package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	// Seq numbers the message within its process's session, from 1, in
	// the order messages were appended. It never changes, survives
	// compaction, Resume and Recover, and addresses archived history.
	// Zero means not yet numbered (numberMessages assigns it).
	Seq int `json:"seq,omitempty"`
	// ArchiveAs, when set, is stored in a memory segment in place of
	// Content. A read_memory result sets it to a reference to what it
	// read, so compaction never copies one segment into another.
	ArchiveAs string `json:"archive_as,omitempty"`
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
	// WorkingMemory is the snapshot the header shows: the version current
	// at the last compaction. Later updates are in History.
	WorkingMemory *WorkingMemory `json:"working_memory,omitempty"`
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

// HeaderSizer is a backend that can say how large the parts of its prompt
// header that change at compaction are, in bytes by name. The runtime logs
// them as a memory.header event whenever they change.
type HeaderSizer interface {
	HeaderSizes(request GenerateRequest) map[string]int
}

type Session struct {
	Messages            []Message     `json:"messages"`
	ToolArtifactIDs     []string      `json:"tool_artifact_ids,omitempty"`
	DelegateArtifactIDs []string      `json:"delegate_artifact_ids,omitempty"`
	MailboxArtifactIDs  []string      `json:"mailbox_artifact_ids,omitempty"`
	ToolAttempts        []ToolAttempt `json:"tool_attempts,omitempty"`
	MemoryRefs          []MemoryRef   `json:"memory_refs,omitempty"`
	MemorySequence      int           `json:"memory_sequence,omitempty"`
	// NextSeq is the last message sequence number assigned.
	NextSeq int `json:"next_seq,omitempty"`
	// WorkingMemory is the current working_memory version, and
	// MemorySnapshot the version the header shows, as of the last
	// compaction, so the prompt prefix only changes at compaction.
	WorkingMemory  string `json:"working_memory,omitempty"`
	MemorySnapshot string `json:"memory_snapshot,omitempty"`
	// HandoffSeq is set while a compaction waits on its handoff step: the
	// last message the notice said would be archived. HandoffUpdates
	// counts the working-memory updates made since the notice.
	HandoffSeq     int `json:"handoff_seq,omitempty"`
	HandoffUpdates int `json:"handoff_updates,omitempty"`
	// HandoffSteps counts the steps taken since the notice, HandoffWrote
	// says the last of them wrote memory, HandoffRefused that it tried to
	// and was refused or had index lines rejected, HandoffRetried that
	// the step a refusal earns was given, HandoffRenoticed that the notice
	// was repeated, and HandoffIndex is the agent's index of the messages
	// the handoff archives, stored with the segment once it is written.
	HandoffSteps     int         `json:"handoff_steps,omitempty"`
	HandoffWrote     bool        `json:"handoff_wrote,omitempty"`
	HandoffRefused   bool        `json:"handoff_refused,omitempty"`
	HandoffRetried   bool        `json:"handoff_retried,omitempty"`
	HandoffRenoticed bool        `json:"handoff_renoticed,omitempty"`
	HandoffIndex     []IndexLine `json:"handoff_index,omitempty"`
	// MailboxSeen lists the IDs of the messages already in this session, so
	// a message delivered again (the bus delivers at least once) is
	// recorded only once.
	MailboxSeen []string `json:"mailbox_seen,omitempty"`
	// Usage is what the process has consumed, so a resumed or recovered
	// process continues against the same budget and step limit. StepBase
	// is where the current turn's steps began (an interactive turn starts
	// a fresh step allowance; nothing else does), Extended what an
	// operator added, and BudgetWarned which final warnings were given.
	Usage        Usage     `json:"usage,omitzero"`
	StepBase     int       `json:"step_base,omitempty"`
	Extended     Extension `json:"extended,omitzero"`
	BudgetWarned []string  `json:"budget_warned,omitempty"`
}

// MemoryRef points at a compacted stretch of history: a memory_segment
// artifact the runtime owns, found with search_memory and read back with
// read_memory. Path is only set on refs from before segments were
// artifacts, Summary only on refs from before the index replaced it.
//
// The rest is the segment's index entry: its message numbers, when it was
// archived, the tools its messages called and the paths they passed to
// read_file, write_file and edit_file. These are recorded facts, not a
// summary; refs written before the index get them on their next prompt.
type MemoryRef struct {
	ArtifactID   string      `json:"artifact_id,omitempty"`
	Path         string      `json:"path,omitempty"`
	Summary      string      `json:"summary,omitempty"`
	MessageCount int         `json:"message_count"`
	FromSeq      int         `json:"from_seq,omitempty"`
	ToSeq        int         `json:"to_seq,omitempty"`
	Archived     string      `json:"archived,omitempty"`
	Tools        []ToolCount `json:"tools,omitempty"`
	Paths        []string    `json:"paths,omitempty"`
	MorePaths    int         `json:"more_paths,omitempty"`
	// Index is what the agent wrote, at handoff, about where things are in
	// the segment; the runtime checked only its message numbers.
	Index []IndexLine `json:"agent_index,omitempty"`
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
	// TurnExit is the exit an ExitEndsTurn run took as the end of a turn.
	TurnExit *kernel.ExitStatus `json:"turn_exit,omitempty"`
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
	// Recall switches the recall aids; the zero value turns them all on.
	Recall RecallPolicy
	// BackendRetry retries failed model calls within a step.
	BackendRetry BackendRetryPolicy
	// OnCapabilityDenied, when set, receives each capability.denied kernel
	// event this runtime records.
	OnCapabilityDenied func(kernel.Event)
}

// CompactionPolicy decides when older history is compacted and how much
// stays live. Compaction starts when history exceeds MaxMessages or
// MaxChars (zero disables that trigger), then archives the oldest messages
// until at most KeepMessages (zero: no limit) and TargetChars remain. The
// gap between MaxChars and TargetChars is the hysteresis: the next
// compaction is that much new history away.
type CompactionPolicy struct {
	MaxMessages  int
	MaxChars     int
	KeepMessages int
	// TargetChars defaults to MaxChars / 2.
	TargetChars int
	// Handoff gives the agent one step, after a notice, to update its
	// working memory before history is archived.
	Handoff bool
}

// EstimatedCharsPerToken is a conservative chars-per-token ratio for the
// code- and JSON-heavy prompts agents see.
const EstimatedCharsPerToken = 3

// DefaultCompactionPolicy is for an unknown context window. It is bounded
// mostly by message count and is not held to Validate: large tool outputs
// make it compact often, which an unknown window cannot avoid.
func DefaultCompactionPolicy() CompactionPolicy {
	return CompactionPolicy{
		MaxMessages:  sessionMemoryMaxMessages,
		MaxChars:     sessionMemoryMaxChars,
		KeepMessages: sessionMemoryKeepMessages,
	}
}

// ContextCompactionPolicy compacts once history reaches ratio of a context
// window of windowTokens tokens, down to half of that, by size alone.
func ContextCompactionPolicy(windowTokens int, ratio float64) CompactionPolicy {
	return CompactionPolicy{
		MaxChars: int(float64(windowTokens)*ratio) * EstimatedCharsPerToken,
		Handoff:  true,
	}
}

// ShouldCompact reports whether messages exceed either policy trigger.
func (p CompactionPolicy) ShouldCompact(messages []Message) bool {
	if p.MaxMessages > 0 && len(messages) > p.MaxMessages {
		return true
	}
	return p.MaxChars > 0 && historyChars(messages) > p.MaxChars
}

func (p CompactionPolicy) targetChars() int {
	if p.TargetChars > 0 {
		return p.TargetChars
	}
	return p.MaxChars / 2
}

// Validate refuses a policy whose hysteresis a single message can defeat:
// if one tool output can be as large as the gap between MaxChars and
// TargetChars, history can cross MaxChars again a step after compacting.
// maxOutputChars is the largest output a single tool call returns.
func (p CompactionPolicy) Validate(maxOutputChars int) error {
	if p.MaxChars <= 0 {
		return nil
	}
	if target := p.targetChars(); target >= p.MaxChars {
		return fmt.Errorf("compaction target of %d chars must be below its trigger of %d", target, p.MaxChars)
	}
	if gap := p.MaxChars - p.targetChars(); maxOutputChars >= gap {
		return fmt.Errorf("compaction at %d chars down to %d leaves %d chars of headroom, but one tool output can be %d chars, so every step could compact; use a larger context window",
			p.MaxChars, p.targetChars(), gap, maxOutputChars)
	}
	return nil
}

func historyChars(messages []Message) int {
	total := 0
	for _, message := range messages {
		total += messageChars(message)
	}
	return total
}

func messageChars(message Message) int {
	return len(message.Role) + len(message.Content)
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
	// Status is DelegateWaiting when the child paused in a wait instead of
	// exiting; Question is what it asked. Empty means it exited.
	Status   string `json:"status,omitempty"`
	Question string `json:"question,omitempty"`
}

// DelegateWaiting marks a delegated child parked in a wait: blocked with
// its checkpoint, resumed when its delegator delegates to it again.
const DelegateWaiting = "waiting"

type DelegateHandler func(ctx context.Context, parent kernel.Process, call DelegateCall, session Session) (DelegateResult, error)

func (r *AgentRuntime) Run(ctx context.Context, pid string, initial []Message) (RunResult, error) {
	return r.runSession(ctx, pid, Session{Messages: append([]Message(nil), initial...)}, RunOptions{}, "")
}

func (r *AgentRuntime) RunWithOptions(ctx context.Context, pid string, initial []Message, options RunOptions) (RunResult, error) {
	return r.runSession(ctx, pid, Session{Messages: append([]Message(nil), initial...)}, options, "")
}

type RunOptions struct {
	// NewTurn starts a fresh step allowance, as a new user message in an
	// interactive session does; otherwise steps continue from the
	// checkpoint, so resuming or recovering cannot reset the step limit.
	NewTurn bool
	// Extend grants extra steps, tokens or seconds, for an operator
	// recovering a process that ran out. It is recorded in the session.
	Extend                 Extension
	StopOnAssistantMessage bool
	// ExitEndsTurn makes an agent's exit end its turn instead of the
	// process: it is parked ready, as after a message, so the user's next
	// message continues the same conversation with its archive and working
	// memory. The interactive root of a terminal session runs this way.
	ExitEndsTurn bool
	// HandoffOnly runs only a handoff: the first step gives the handoff
	// notice for the history the compaction policy would archive (whether
	// or not it is due), and the run parks ready as soon as that history
	// is compacted, before any other action runs. It is for asking an
	// agent to write its working memory and index for a checkpoint it
	// resumes from; give it a process without tool grants, so only the
	// memory tools can run.
	HandoffOnly         bool
	ContinueOnToolError bool
	OnToolCall          func(ToolCall)
	OnToolResult        func(tools.Result)
	OnToolError         func(ToolCall, error)
	OnAssistantMessage  func(string)
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
	if r.Bus != nil {
		if _, err := r.Bus.Transfer(pid, recovered.PID); err != nil {
			status := kernel.ExitStatus{Code: 1, Reason: "mail transfer: " + err.Error(), Retryable: true, Checkpoint: checkpointID}
			_, _ = r.Kernel.Exit(recovered.PID, status)
			return RunResult{PID: recovered.PID, ExitStatus: status, CheckpointID: checkpointID}, err
		}
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
	result, runErr = r.closeRun(pid, track, result, runErr)
	if r.Bus != nil {
		// An exited process reads nothing more; its pending mail stays for a
		// Recover to take over.
		if final, err := r.Kernel.Get(pid); err == nil && final.State == kernel.StateExited {
			_ = r.Bus.Unsubscribe(pid)
		}
	}
	return result, runErr
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

	// The budget: the process's own (plus any operator extension), chained
	// to its delegator's through ctx, continuing from the usage its
	// checkpoint recorded.
	if options.NewTurn {
		session.StepBase = session.Usage.Steps
	}
	session.Extended = session.Extended.add(options.Extend)
	limit := process.CapabilitySet.Budget
	if limit.Tokens > 0 {
		limit.Tokens += session.Extended.Tokens
	}
	if limit.Seconds > 0 {
		limit.Seconds += session.Extended.Seconds
	}
	owner := pid
	if agentID := process.Metadata["agent_id"]; agentID != "" {
		owner = fmt.Sprintf("%s (%s)", agentID, pid)
	}
	account := NewAccount(owner, limit, session.Usage, AccountFrom(ctx))
	account.Start()
	ctx = WithAccount(ctx, account)
	if strict := account.timeLeft(true); strict.ok {
		// Only a strict time budget stops the run; the in-flight model call
		// and any running command are cancelled with it.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, time.Duration(max(strict.Left, 0)*float64(time.Second)), timeBudgetError(strict))
		defer cancel()
	}
	syncUsage := func() {
		session.Usage.Tokens, session.Usage.Seconds = account.Used()
	}
	synced := func() Session {
		syncUsage()
		return session
	}

	var checkpointID string
	// unacked are received messages no checkpoint records yet; the next
	// checkpoint acknowledges them.
	var unacked []string
	defer func() {
		account.Stop()
		syncUsage()
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
	maxSteps += session.Extended.Steps
	// budgetExit ends the process when a budget runs out, or the model
	// endpoint stays down: retryable, since an operator may grant more or
	// the endpoint come back, with a checkpoint that keeps the usage.
	budgetExit := func(reason string) (RunResult, error) {
		record, err := r.saveCheckpoint(pid, synced(), latest(), "final", &unacked)
		if err != nil {
			return RunResult{}, err
		}
		checkpointID = record.ID
		status := kernel.ExitStatus{Code: 1, Reason: reason, Retryable: true, Checkpoint: checkpointID}
		if _, err := r.Kernel.Exit(pid, status); err != nil {
			return RunResult{}, err
		}
		return RunResult{PID: pid, ExitStatus: status, CheckpointID: checkpointID, Session: session}, nil
	}
	// stop handles a cancelled ctx: a strict time budget running out ends
	// the process; anything else (the user's Esc) parks it.
	stop := func() (RunResult, error) {
		if cause := context.Cause(ctx); errors.Is(cause, ErrTimeBudgetExceeded) {
			return budgetExit(cause.Error())
		}
		return r.cancelRun(ctx, pid, synced(), latest(), &unacked)
	}
	// handoffDone parks a HandoffOnly run once its handoff compacted.
	handoffDone := func() (RunResult, error) {
		record, err := r.saveCheckpoint(pid, synced(), latest(), "handoff", &unacked)
		if err != nil {
			return RunResult{}, err
		}
		checkpointID = record.ID
		if _, err := r.Kernel.Transition(pid, kernel.StateReady); err != nil {
			return RunResult{}, err
		}
		return RunResult{PID: pid, CheckpointID: checkpointID, Session: session, Yielded: true}, nil
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
	var headerLogged map[string]string
	// Steps count across resumes and recoveries (Usage.Steps is saved), from
	// the start of the current turn.
	for step := 0; session.Usage.Steps-session.StepBase < maxSteps; step++ {
		if ctx.Err() != nil {
			return stop()
		}
		session.Usage.Steps++
		for _, note := range budgetNotes(account, &session) {
			session.Messages = append(session.Messages, Message{Role: "tool", Content: note})
		}
		if r.Bus != nil {
			// The first step also takes messages handed out before but never
			// acknowledged: a crash or an interruption left them unrecorded.
			inboxMessages, err := r.Bus.Receive(pid, step == 0)
			if err != nil {
				return RunResult{}, err
			}
			var received []string
			session, received, err = r.appendInboxArtifacts(pid, session, inboxMessages)
			if err != nil {
				return RunResult{}, err
			}
			unacked = append(unacked, received...)
		}
		archived := len(session.MemoryRefs)
		if options.HandoffOnly && step == 0 && session.HandoffSeq == 0 {
			session, err = r.beginHandoff(process, numberMessages(session))
		} else {
			session, err = r.compactSessionMemory(process, session, latest(), false)
		}
		if err != nil {
			return RunResult{}, err
		}
		if options.HandoffOnly && len(session.MemoryRefs) > archived {
			return handoffDone()
		}
		session, err = r.indexMemoryRefs(pid, session)
		if err != nil {
			return RunResult{}, err
		}
		var snapshot *WorkingMemory
		if session.MemorySnapshot != "" {
			memory, err := r.readWorkingMemory(session.MemorySnapshot)
			if err != nil {
				return RunResult{}, err
			}
			snapshot = &memory
		}

		// Check the token budget before the call, and cap the reply to what
		// is left of it; its prompt may still overshoot, by at most one call.
		info := CallInfo{PID: pid, AgentID: process.Metadata["agent_id"]}
		if tokens := account.tokensLeft(); tokens.ok {
			if tokens.Left <= 0 {
				return budgetExit(fmt.Sprintf("token budget exceeded: %d of %d tokens used (budget of %s)", tokens.Limit-int(tokens.Left), tokens.Limit, tokens.Owner))
			}
			info.MaxTokens = int(tokens.Left)
		}
		request := GenerateRequest{
			Process:       process,
			History:       append([]Message(nil), session.Messages...),
			MemoryRefs:    append([]MemoryRef(nil), session.MemoryRefs...),
			WorkingMemory: snapshot,
		}
		if sizer, ok := r.Backend.(HeaderSizer); ok && !r.Recall.NoAgentIndex {
			// The header only changes at compaction, so it is logged when
			// it does, and at a run's first call.
			data := map[string]string{}
			for name, size := range sizer.HeaderSizes(request) {
				data[name] = strconv.Itoa(size)
			}
			if !maps.Equal(data, headerLogged) {
				if _, err := r.Kernel.RecordEvent(pid, "memory.header", data); err != nil {
					return RunResult{}, err
				}
				headerLogged = data
			}
		}
		// A failed call is retried within the step, after a growing wait:
		// an endpoint outage is not the agent's doing and must not spend
		// its steps.
		retry := r.BackendRetry.withDefaults()
		var result GenerateResult
		attempts := 0
		for {
			attempts++
			meter := &TokenMeter{}
			info.Meter = meter
			result, err = r.Backend.Generate(WithCallInfo(ctx, info), request)
			account.AddTokens(meter.Total())
			syncUsage()
			if ctx.Err() != nil {
				// Drop whatever the interrupted call produced, action or error.
				return stop()
			}
			if err == nil || retry.Attempts < 0 || attempts >= retry.Attempts {
				break
			}
			delay := retry.delay(attempts)
			if _, recordErr := r.Kernel.RecordEvent(pid, "backend.retry", map[string]string{
				"attempt": strconv.Itoa(attempts),
				"error":   err.Error(),
				"delay":   delay.String(),
			}); recordErr != nil {
				return RunResult{}, recordErr
			}
			sleepContext(ctx, delay)
			if ctx.Err() != nil {
				return stop()
			}
		}
		if err != nil && retry.Attempts >= 0 {
			// Retries ran out: the endpoint is down, which another step
			// cannot fix. Exit retryable with a checkpoint, as a budget does.
			return budgetExit(fmt.Sprintf("model endpoint failed %d times in a row: %v", attempts, err))
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

		if session.HandoffSteps > 0 && result.Action.Type != "exit" && !writesMemory(result.Action) {
			// A later handoff step that does something else is compacted
			// before its action runs, so its output lands on the
			// compacted history rather than overrunning the trigger.
			if session, err = r.compactSessionMemory(process, session, latest(), true); err != nil {
				return RunResult{}, err
			}
			if options.HandoffOnly {
				return handoffDone()
			}
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
			artifactRecord, err := r.saveCheckpoint(pid, synced(), latest(), "yield", &unacked)
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
					artifactRecord, err := r.saveCheckpoint(pid, synced(), latest(), "loop_retry_suppressed", &unacked)
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
						Content: fmt.Sprintf("tool_cancelled name=%s reason=%s", result.Action.ToolCall.Name, cancelReason(ctx)),
					})
					return stop()
				}
				if options.ContinueOnToolError {
					if session.HandoffSeq != 0 && writesMemory(result.Action) {
						session.HandoffRefused = true
					}
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
			resultMessage := Message{
				Role:    "tool",
				Content: toolResultMessage(toolResult),
			}
			switch {
			case toolResult.Tool == "read_memory" && toolResult.Output["artifact_id"] == "":
				resultMessage.ArchiveAs = rangeReadReference(toolResult.Output["from"], toolResult.Output["to"])
			case toolResult.Tool == "read_memory":
				end := toolResult.Output["next_offset"]
				if end == "" {
					end = toolResult.Output["total_bytes"]
				}
				resultMessage.ArchiveAs = readMemoryReference(toolResult.Output["artifact_id"], toolResult.Output["offset"], end, toolResult.Output["total_bytes"])
			case toolResult.Tool == "search_memory":
				resultMessage.ArchiveAs = searchReference(toolResult.Output["query"], toolResult.Output["matched"])
			case toolResult.Tool == "update_memory":
				session.WorkingMemory = toolResult.Output["artifact_id"]
				if session.HandoffSeq != 0 {
					session.HandoffUpdates++
					session.HandoffWrote = true
				}
			case toolResult.Tool == "index_memory":
				lines, rejected := parseIndex(toolResult.Output["index"], 1, session.HandoffSeq)
				if len(rejected) > 0 {
					return RunResult{}, fmt.Errorf("index_memory returned an index it would reject: %s", rejected[0])
				}
				session.HandoffIndex = lines
				session.HandoffWrote = true
				session.HandoffRefused = toolResult.Output["rejected"] != ""
			}
			session.Messages = append(session.Messages, resultMessage)
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
					Content: fmt.Sprintf("delegate_cancelled agent=%s reason=%s", result.Action.Delegate.AgentID, cancelReason(ctx)),
				})
				return stop()
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
			if delegateResult.Status == DelegateWaiting {
				session.Messages = append(session.Messages, Message{
					Role: "tool",
					Content: fmt.Sprintf(
						"delegate_result agent=%s pid=%s status=waiting question=%q checkpoint=%s note=%q",
						delegateResult.AgentID, delegateResult.PID, delegateResult.Question, delegateResult.CheckpointID,
						"The agent paused to ask you this and has not exited. Answer by delegating to "+delegateResult.AgentID+
							" again with your answer as the goal: that resumes the same process. If you exit without answering, it is closed as abandoned.",
					),
				})
				break
			}
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
			if errors.Is(err, ipc.ErrUnknownRecipient) || errors.Is(err, ipc.ErrRecipientExited) {
				// The agent named a process that cannot read it: its mistake to
				// correct, not a failure of the bus.
				if continued, failed, failErr := r.refuseAction(pid, lastCheckpoint(), &session, "send", err, options); !continued {
					return failed, failErr
				}
				break
			}
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
			artifactRecord, err := r.saveCheckpoint(pid, synced(), latest(), "blocked", &unacked)
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
			if options.ExitEndsTurn {
				ended := *result.Action.ExitStatus
				ended.Origin = kernel.ExitOriginAgent
				session.Messages = append(session.Messages, Message{Role: "notice", Content: fmt.Sprintf(exitEndsTurnNotice, ended.Reason)})
				artifactRecord, err := r.saveCheckpoint(pid, synced(), latest(), "yield", &unacked)
				if err != nil {
					return RunResult{}, err
				}
				checkpointID = artifactRecord.ID
				if _, err := r.Kernel.Transition(pid, kernel.StateReady); err != nil {
					return RunResult{}, err
				}
				return RunResult{PID: pid, CheckpointID: checkpointID, Session: session, Yielded: true, TurnExit: &ended}, nil
			}
			artifactRecord, err := r.saveCheckpoint(pid, synced(), latest(), "final", &unacked)
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

		artifactRecord, err := r.saveCheckpoint(pid, synced(), latest(), "intermediate", &unacked)
		if err != nil {
			return RunResult{}, err
		}
		checkpointID = artifactRecord.ID
	}

	// Checkpoint the last steps too: ones that ended in a backend error
	// continued without one, and may be every step the agent took.
	artifactRecord, err := r.saveCheckpoint(pid, synced(), latest(), "final", &unacked)
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
//
// It archives the oldest messages until the policy's targets hold, cutting
// only at the start of a turn, so a tool call stays with its result, and
// never past the latest turn. If the latest turn alone is over the target,
// it is kept anyway and the memory.compacted event says oversized. now
// compacts a pending handoff at once, without giving it another step.
func (r *AgentRuntime) compactSessionMemory(process kernel.Process, session Session, parents []string, now bool) (Session, error) {
	session = numberMessages(session)
	policy := r.compactionPolicy()
	handoff := session.HandoffSeq != 0
	if !handoff && !policy.ShouldCompact(session.Messages) {
		return session, nil
	}
	cut, oversized := compactionCut(session.Messages, policy)
	if cut == 0 {
		session = endHandoff(session)
		return session, nil
	}
	if policy.Handoff && !handoff {
		// Compact at the next model call, whatever this step does; an exit
		// ends the run with its history whole.
		return r.beginHandoff(process, session)
	}
	end := "other_action"
	if handoff && !now {
		var wait, renotice bool
		wait, renotice, end = r.handoffContinues(&session)
		if renotice {
			session.Messages = append(session.Messages, Message{Role: "notice", Content: r.renotice(session)})
			session = numberMessages(session)
			if err := r.noteRecall(process.PID, NoticeRenotice, map[string]string{"through_seq": strconv.Itoa(session.HandoffSeq)}); err != nil {
				return Session{}, err
			}
		}
		if wait {
			return session, nil
		}
	}
	if handoff && !r.Recall.NoAgentIndex {
		// Archive exactly what the notice named, which the agent indexed,
		// not what the history has grown to since.
		if i := slices.IndexFunc(session.Messages, func(m Message) bool { return m.Seq == session.HandoffSeq }); i >= 0 {
			cut = i + 1
			oversized = policy.MaxChars > 0 && historyChars(session.Messages[cut:]) > policy.targetChars()
		}
	}
	prior, priorCut, refused, err := r.priorSegment(process.PID, session.Messages, parents)
	if err != nil {
		return Session{}, err
	}
	if prior.ID != "" {
		cut = priorCut
		oversized = policy.MaxChars > 0 && historyChars(session.Messages[cut:]) > policy.targetChars()
	}

	older := archivedMessages(session.Messages[:cut])
	recent := append([]Message(nil), session.Messages[cut:]...)
	segment := memorySegment{
		ProcessID:    process.PID,
		Sequence:     session.MemorySequence + 1,
		MessageCount: len(older),
		FromSeq:      older[0].Seq,
		ToSeq:        older[len(older)-1].Seq,
		Messages:     older,
		Index:        indexWithin(session.HandoffIndex, older[0].Seq, older[len(older)-1].Seq),
	}
	record := prior
	if prior.ID != "" {
		// An adopted segment keeps the index it was written with.
		var adopted memorySegment
		if err := r.Artifacts.ReadJSON(prior.ID, &adopted); err != nil {
			return Session{}, err
		}
		segment.Index = adopted.Index
	}
	if record.ID == "" {
		record, err = r.Artifacts.CreateJSON("memory_segment", process.PID, segment, parents, map[string]string{
			"sequence":        strconv.Itoa(segment.Sequence),
			"from_seq":        strconv.Itoa(segment.FromSeq),
			"to_seq":          strconv.Itoa(segment.ToSeq),
			"messages_sha256": messagesDigest(older),
		})
		if err != nil {
			return Session{}, err
		}
	}
	data := map[string]string{
		"segment":       record.ID,
		"from_seq":      strconv.Itoa(segment.FromSeq),
		"to_seq":        strconv.Itoa(segment.ToSeq),
		"archived":      strconv.Itoa(len(older)),
		"live_messages": strconv.Itoa(len(recent)),
		"live_chars":    strconv.Itoa(historyChars(recent)),
		"oversized":     strconv.FormatBool(oversized),
		"adopted":       strconv.FormatBool(prior.ID != ""),
	}
	if refused != "" {
		data["not_adopted"] = refused
	}
	if handoff {
		data["handoff_through"] = strconv.Itoa(session.HandoffSeq)
		data["handoff_updates"] = strconv.Itoa(session.HandoffUpdates)
		if !r.Recall.NoAgentIndex || !r.Recall.NoHandoffRenotice {
			data["handoff_steps"] = strconv.Itoa(session.HandoffSteps)
			data["handoff_renoticed"] = strconv.FormatBool(session.HandoffRenoticed)
		}
		if !r.Recall.NoAgentIndex {
			data["agent_index_lines"] = strconv.Itoa(len(segment.Index))
			data["handoff_end"] = end
			data["handoff_retried"] = strconv.FormatBool(session.HandoffRetried)
		}
	}
	if _, err := r.Kernel.RecordEvent(process.PID, "memory.compacted", data); err != nil {
		return Session{}, err
	}
	ref := indexedRef(MemoryRef{
		ArtifactID:   record.ID,
		MessageCount: segment.MessageCount,
	}, older, record.CreatedAt)
	ref.Index = segment.Index
	session.MemoryRefs = append(session.MemoryRefs, ref)
	session.MemorySequence++
	session.Messages = recent
	session.MemorySnapshot = session.WorkingMemory
	session = endHandoff(session)
	return session, nil
}

// ErrNothingToArchive is a HandoffOnly run's refusal of a history too
// short to compact.
var ErrNothingToArchive = errors.New("nothing to archive: the history has no earlier turn to compact")

// beginHandoff gives the handoff notice for what compaction would archive
// now: from the first live message through the last one before the cut.
func (r *AgentRuntime) beginHandoff(process kernel.Process, session Session) (Session, error) {
	cut, _ := compactionCut(session.Messages, r.compactionPolicy())
	if cut == 0 {
		return Session{}, ErrNothingToArchive
	}
	through := session.Messages[cut-1].Seq
	notice := fmt.Sprintf(handoffNotice, through)
	if !r.Recall.NoAgentIndex {
		from := session.Messages[0].Seq
		notice = fmt.Sprintf(handoffIndexFirstNotice, through, from, through, from, through, SegmentIndexCap(cut))
	}
	session.Messages = append(session.Messages, Message{Role: "notice", Content: notice})
	session = numberMessages(session)
	session = endHandoff(session)
	session.HandoffSeq = through
	if _, err := r.Kernel.RecordEvent(process.PID, "memory.handoff", map[string]string{"through_seq": strconv.Itoa(through)}); err != nil {
		return Session{}, err
	}
	if !r.Recall.NoAgentIndex {
		if err := r.noteRecall(process.PID, NoticeIndex, map[string]string{"through_seq": strconv.Itoa(through)}); err != nil {
			return Session{}, err
		}
	}
	return session, nil
}

// maxHandoffSteps bounds the steps between a handoff notice and its
// compaction, whatever the agent does.
const maxHandoffSteps = 4

// maxHandoffExtraChars bounds the live history a handoff step after the
// first may be shown: only while it is within this much of the trigger.
// Without the bound, the first handoff step's output, which compaction
// used to archive before any model saw it, would reach the model. A later
// step that would not write memory is compacted before its action runs,
// so only a memory write's record and result can add to it.
const maxHandoffExtraChars = 16 * 1024

// handoffContinues decides, at the step after a handoff step, whether to
// compact now or give the agent another step: while it keeps writing
// memory (update_memory, index_memory) when it is asked for an index, or
// once more, with the notice repeated, when it ignored the notice and has
// no working memory or no index. A write that was refused, or had index
// lines rejected, earns one step to retry whatever the other bounds: that
// step sees the history the refused step saw plus the refused call and its
// bounded report. Every path compacts within maxHandoffSteps+1. end says
// why a handoff that does not wait ends.
func (r *AgentRuntime) handoffContinues(session *Session) (wait, renotice bool, end string) {
	wrote, refused := session.HandoffWrote, session.HandoffRefused
	session.HandoffWrote, session.HandoffRefused = false, false
	session.HandoffSteps++
	if refused && !session.HandoffRetried && !r.Recall.NoAgentIndex {
		session.HandoffRetried = true
		return true, false, ""
	}
	policy := r.compactionPolicy()
	if session.HandoffSteps >= maxHandoffSteps {
		return false, false, "step_limit"
	}
	if policy.MaxChars > 0 && historyChars(session.Messages) > policy.MaxChars+maxHandoffExtraChars {
		return false, false, "overrun"
	}
	if wrote && !r.Recall.NoAgentIndex {
		return true, false, ""
	}
	missing := session.WorkingMemory == "" || (!r.Recall.NoAgentIndex && len(session.HandoffIndex) == 0)
	if !wrote && missing && !session.HandoffRenoticed && !r.Recall.NoHandoffRenotice {
		session.HandoffRenoticed = true
		return true, true, ""
	}
	if wrote {
		return false, false, "wrote"
	}
	return false, false, "no_write"
}

// renotice repeats a handoff notice the agent did not act on, naming
// index_memory when the index is what is missing.
func (r *AgentRuntime) renotice(session Session) string {
	if r.Recall.NoAgentIndex || len(session.HandoffIndex) > 0 {
		return fmt.Sprintf(handoffRenotice, session.HandoffSeq)
	}
	from := session.Messages[0].Seq
	notice := fmt.Sprintf(handoffIndexRenotice, session.HandoffSeq, from, session.HandoffSeq, SegmentIndexCap(handoffMessages(session)))
	if session.WorkingMemory == "" {
		notice += handoffMemoryRenotice
	}
	return notice
}

// endHandoff clears a session's handoff state.
func endHandoff(session Session) Session {
	session.HandoffSeq, session.HandoffUpdates = 0, 0
	session.HandoffSteps, session.HandoffWrote, session.HandoffRenoticed = 0, false, false
	session.HandoffRefused, session.HandoffRetried = false, false
	session.HandoffIndex = nil
	return session
}

// writesMemory reports whether action writes working memory or the
// archive's index: what a handoff step after the first may do before
// compaction.
func writesMemory(action GenerateAction) bool {
	return action.Type == "tool" && action.ToolCall != nil && (action.ToolCall.Name == "update_memory" || action.ToolCall.Name == "index_memory")
}

// indexWithin keeps the index lines that lie within from-to.
func indexWithin(lines []IndexLine, from, to int) []IndexLine {
	var kept []IndexLine
	for _, line := range lines {
		if line.From >= from && line.To <= to {
			kept = append(kept, line)
		}
	}
	return kept
}

// exitEndsTurnNotice tells an agent whose exit ended only its turn that the
// conversation goes on.
const exitEndsTurnNotice = "runtime notice: You exited (%s). In this interactive session that ends your turn, not the session: the user's next message continues this conversation."

// handoffNotice precedes a compaction. It is labelled as the runtime's, so
// it reads neither as a tool's output nor as the user.
const handoffNotice = "runtime notice: History through message %d will move to the archive before your next call. " +
	"Your working memory is what you will still see. Update it now with anything from messages up to that one you will need: " +
	"results, decisions, and what you rejected and why, with the message numbers they rest on as evidence."

// handoffIndexFirstNotice replaces handoffNotice when the agent indexes
// what it archives: the index first, with its range and cap, then working
// memory.
const handoffIndexFirstNotice = "runtime notice: History through message %d will move to the archive before your next call. " +
	"First index messages %d-%d with index_memory: one line per thing you might need to find again " +
	"(measurements with their numbers, decisions and why, rejected approaches, errors and their fixes), as \"N: what is there\" or \"N-M: what is there\", " +
	"citing only messages %d-%d, in at most %d bytes. The index stays in your context as a map of the archive, so you know what to read back. " +
	"Then update your working memory with anything from those messages you will need: results, decisions, and what you rejected and why, " +
	"with the message numbers they rest on as evidence."

// handoffRenotice repeats a handoff notice the agent's last step ignored;
// compaction follows the next step whatever it does.
const handoffRenotice = "runtime notice: Reminder: history through message %d moves to the archive after your next action, and you have not saved what you need from it. " +
	"Update your working memory now with anything from those messages you will need, with the message numbers as evidence."

// handoffIndexRenotice repeats a handoff notice whose index the agent has
// not written; handoffMemoryRenotice follows it when working memory is
// missing too.
const handoffIndexRenotice = "runtime notice: Reminder: history through message %d moves to the archive after your next action, and you have not indexed it. " +
	"Call index_memory now for messages %d-%d: one \"N: what is there\" line per thing you might need to find again, citing only those messages, in at most %d bytes."

const handoffMemoryRenotice = " Then update your working memory with anything from those messages you will need, with the message numbers as evidence."

// toolResultMessage records a tool result in the history. Tools cap their
// own output; this holds every result to tools.MaxToolResultBytes, the
// bound compaction is validated against, whatever a tool returns.
func toolResultMessage(result tools.Result) string {
	content := fmt.Sprintf("%s:%v", result.Tool, result.Output)
	if len(content) <= tools.MaxToolResultBytes {
		return content
	}
	cut := tools.MaxToolResultBytes - 200
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + fmt.Sprintf("\n[tool result truncated: showing %d of %d bytes]", cut, len(content))
}

// priorSegment finds a segment this process, or one it was recovered from,
// already cut from the same checkpoint starting at the same message, that
// no later checkpoint followed: a run that crashed after compacting but
// before checkpointing the result. The replay adopts it, and cuts where it
// did, so no message is stored twice, but only if its messages in that
// range are the ones the segment holds: turns appended after the
// checkpoint, such as inbox messages, can differ on replay and get the
// same numbers. A segment that differs is not adopted, and refused names
// it. A segment a checkpoint followed
// belongs to a run that went on, so a second resume from the same
// checkpoint is a fork and cuts its own. A compaction with no parent
// checkpoint (the first step of a fresh run) cannot be replayed.
func (r *AgentRuntime) priorSegment(pid string, messages []Message, parents []string) (prior artifact.Artifact, cut int, refused string, err error) {
	if len(parents) == 0 || len(messages) == 0 || messages[0].Seq == 0 {
		return artifact.Artifact{}, 0, "", nil
	}
	records, err := r.Artifacts.List()
	if err != nil {
		return artifact.Artifact{}, 0, "", err
	}
	from := strconv.Itoa(messages[0].Seq)
	followed := func(segment artifact.Artifact) bool {
		for _, record := range records {
			if record.Type == "checkpoint" && slices.Equal(record.ParentIDs, parents) && !record.CreatedAt.Before(segment.CreatedAt) {
				return true
			}
		}
		return false
	}
	for _, record := range records {
		if record.Type != "memory_segment" || record.Metadata["from_seq"] != from || !slices.Equal(record.ParentIDs, parents) {
			continue
		}
		if r.ownedBy(pid, record) != nil || followed(record) {
			continue
		}
		to, err := strconv.Atoi(record.Metadata["to_seq"])
		if err != nil {
			continue
		}
		for i, message := range messages {
			// The cut must leave the latest turn live, as compactionCut does.
			if message.Seq != to || i+1 >= len(messages) {
				continue
			}
			if record.Metadata["messages_sha256"] != messagesDigest(archivedMessages(messages[:i+1])) {
				refused = record.ID + ": its messages differ from the replay's"
				break
			}
			return record, i + 1, "", nil
		}
	}
	return artifact.Artifact{}, 0, refused, nil
}

// messagesDigest is the SHA-256 of messages as a segment stores them.
func messagesDigest(messages []Message) string {
	encoded, _ := json.Marshal(messages)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// compactionCut is how many of the oldest messages to archive: the fewest,
// at a turn boundary, that leave the rest within the policy's targets, or
// everything before the latest turn when nothing smaller does (oversized
// reports that the latest turn alone is over the character target).
func compactionCut(messages []Message, policy CompactionPolicy) (cut int, oversized bool) {
	target := policy.targetChars()
	live := historyChars(messages)
	last := 0
	for i := range messages {
		if turnStart(messages, i) {
			last = i
		}
	}
	for i := range messages {
		if i > 0 && turnStart(messages, i) {
			fits := policy.MaxChars <= 0 || live <= target
			if policy.KeepMessages > 0 && len(messages)-i > policy.KeepMessages {
				fits = false
			}
			if fits || i == last {
				return i, policy.MaxChars > 0 && live > target
			}
		}
		live -= messageChars(messages[i])
	}
	return 0, false
}

// turnStart reports whether a cut before messages[i] keeps every turn
// whole: tool results stay with the action that asked for them, and an
// action record with the message that followed it.
func turnStart(messages []Message, i int) bool {
	switch messages[i].Role {
	case "tool":
		return false
	case "assistant":
		return i == 0 || messages[i-1].Role != "assistant"
	}
	return true
}

// archivedMessages is what a segment stores for messages: their content,
// or the reference they carry instead (ArchiveAs). A read_memory result
// from before references existed is reduced to the segment it read.
func archivedMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, message := range messages {
		switch {
		case message.ArchiveAs != "":
			message.Content = message.ArchiveAs
			message.ArchiveAs = ""
		case message.Role == "tool" && strings.HasPrefix(message.Content, "read_memory:"):
			if id := legacyReadMemoryTarget.FindStringSubmatch(message.Content); id != nil {
				message.Content = readMemoryReference(id[1], "", "", "")
			}
		}
		out = append(out, message)
	}
	return out
}

var legacyReadMemoryTarget = regexp.MustCompile(`^read_memory:map\[artifact_id:(\S+) `)

// readMemoryReference stands in for a read_memory result in a segment:
// what was read and how to read it again, without its text. It is one hop:
// reading it back returns this reference, never the text it names.
func readMemoryReference(id, offset, end, total string) string {
	where := id
	if offset != "" && end != "" && total != "" {
		where = fmt.Sprintf("%s, bytes %s-%s of %s", id, offset, end, total)
	}
	again := "read_memory artifact_id=" + id
	if offset != "" && offset != "0" {
		again += " offset=" + offset
	}
	return fmt.Sprintf("read_memory result (%s): not copied into this segment; %s reads it again", where, again)
}

// numberMessages gives every unnumbered message the next sequence number.
// A session from before numbering starts after its archived messages, so
// numbers still count from the start of the process's history.
func numberMessages(session Session) Session {
	next := session.NextSeq
	numbered := false
	for _, message := range session.Messages {
		if message.Seq > 0 {
			numbered = true
			next = max(next, message.Seq)
		}
	}
	if next == 0 && !numbered {
		for _, ref := range session.MemoryRefs {
			next += ref.MessageCount
		}
	}
	// Number a copy: the caller's slice may be shared with a saved session.
	messages := session.Messages
	copied := false
	for i := range messages {
		if messages[i].Seq != 0 {
			continue
		}
		if !copied {
			messages = append([]Message(nil), messages...)
			copied = true
		}
		next++
		messages[i].Seq = next
	}
	session.Messages = messages
	session.NextSeq = next
	return session
}

// memorySegment is the payload of a memory_segment artifact. FromSeq and
// ToSeq are the sequence numbers of its first and last messages (zero in
// segments written before numbering). Summary is only set in segments
// written before the index replaced it.
type memorySegment struct {
	ProcessID    string    `json:"process_id"`
	Sequence     int       `json:"sequence"`
	MessageCount int       `json:"message_count"`
	FromSeq      int       `json:"from_seq,omitempty"`
	ToSeq        int       `json:"to_seq,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	Messages     []Message `json:"messages"`
	// Index is the agent's index of these messages, written at handoff.
	Index []IndexLine `json:"agent_index,omitempty"`
}

// cancelReason says why ctx stopped a tool or delegate: the user, or a
// strict time budget.
func cancelReason(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), ErrTimeBudgetExceeded) {
		return "time_budget_exceeded"
	}
	return "interrupted_by_user"
}

// saveCheckpoint writes session as a checkpoint of pid, then acknowledges
// the messages it now records. An acknowledgement that fails only means
// they are delivered again, and the session drops the repeats.
func (r *AgentRuntime) saveCheckpoint(pid string, session Session, parents []string, kind string, unacked *[]string) (artifact.Artifact, error) {
	session = numberMessages(session)
	record, err := r.Artifacts.CreateJSON("checkpoint", pid, session, parents, map[string]string{"kind": kind})
	if err != nil {
		return artifact.Artifact{}, err
	}
	if r.Bus != nil && len(*unacked) > 0 {
		if r.Bus.Ack(pid, *unacked) == nil {
			*unacked = nil
		}
	}
	return record, nil
}

// cancelRun checkpoints session and parks the process as ready so a later
// Resume picks up where the cancelled run stopped.
func (r *AgentRuntime) cancelRun(ctx context.Context, pid string, session Session, parents []string, unacked *[]string) (RunResult, error) {
	artifactRecord, err := r.saveCheckpoint(pid, session, parents, "cancelled", unacked)
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
		r.logDenial(process.PID, call.Name, call.Name, path, tools.DenialReason(err))
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
	// read_memory and search_memory only return the process's own compacted
	// history, which was already in its context, and update_memory only
	// writes the process's own working memory, and index_memory its own
	// archive's index, so every agent may use them.
	if capabilityName == "read_memory" || capabilityName == "search_memory" || capabilityName == "update_memory" || capabilityName == "index_memory" || (capability.Evaluator{}).ToolAllowed(capabilityName, process.CapabilitySet) {
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
	case "spawn_agent", "message_agent", "await_agent", "checkpoint", "list_artifacts", "publish_artifact", "acquire_lease", "release_lease", "read_memory", "search_memory", "update_memory", "index_memory":
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
	case "search_memory":
		return r.searchMemory(process.PID, session, call.Args)
	case "update_memory":
		return r.updateMemory(process.PID, session, call.Args)
	case "index_memory":
		return r.indexMemory(session, call.Args)
	case "read_memory":
		if strings.TrimSpace(call.Args["from"]) != "" {
			return r.readMemoryRange(process.PID, session, call.Args)
		}
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
		// A page at a time: the whole segment would put the compacted history
		// straight back into the context. Read results that older segments
		// copied read back as references, as in newer segments.
		text := segmentText(archivedMessages(segment.Messages))
		offset := 0
		if raw := strings.TrimSpace(call.Args["offset"]); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				return tools.Result{}, errors.New("read_memory offset must be a non-negative byte offset")
			}
			offset = min(parsed, len(text))
			for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
				offset--
			}
		}
		end := min(offset+tools.MaxToolOutputBytes, len(text))
		for end > offset && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		output := map[string]string{
			"artifact_id":   id,
			"message_count": strconv.Itoa(segment.MessageCount),
			"offset":        strconv.Itoa(offset),
			"total_bytes":   strconv.Itoa(len(text)),
			"messages":      text[offset:end],
		}
		if end < len(text) {
			output["next_offset"] = strconv.Itoa(end)
		}
		// The index the header shows this segment with, so its "+K more
		// lines" pointer reads back every line: the segment's ref carries
		// it, as written at the cut; an older ref may not.
		index := segment.Index
		for _, ref := range session.MemoryRefs {
			if ref.ArtifactID == id && len(ref.Index) > 0 {
				index = ref.Index
			}
		}
		if len(index) > 0 {
			output["agent_index"] = RenderIndex(index)
		}
		return tools.Result{
			Tool:    call.Name,
			Summary: "read memory segment",
			Output:  output,
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

// appendInboxArtifacts records the messages new to the session and adds
// them to its history; repeats of messages it already holds are skipped.
// It returns every received ID, repeats included, for acknowledgement.
//
// Only the user speaks as the user. A peer's message names its sender and
// reaches the model as information, ranked below the task and the user by
// the system prompt, so a peer cannot redefine another agent's work.
func (r *AgentRuntime) appendInboxArtifacts(pid string, session Session, messages []ipc.Message) (Session, []string, error) {
	var received []string
	for _, message := range messages {
		received = append(received, message.ID)
		if slices.Contains(session.MailboxSeen, message.ID) {
			continue
		}
		artifactRecord, err := r.Artifacts.CreateJSON("mailbox_message", pid, message, nil, map[string]string{
			"direction": "receive",
			"from":      message.From,
			"topic":     message.Topic,
		})
		if err != nil {
			return Session{}, received, err
		}
		session.MailboxArtifactIDs = append(session.MailboxArtifactIDs, artifactRecord.ID)
		session.MailboxSeen = append(session.MailboxSeen, message.ID)
		if message.From == "user" {
			session.Messages = append(session.Messages, Message{Role: "user", Content: message.Body})
			continue
		}
		session.Messages = append(session.Messages, Message{Role: "mailbox", Content: r.peerMessage(message)})
	}
	return session, received, nil
}

// PeerMessagePrefix starts every message from another agent in the history.
const PeerMessagePrefix = "message from "

// peerMessage is how a message from another agent reads in the history:
// sender first, so it is never mistaken for the user.
func (r *AgentRuntime) peerMessage(message ipc.Message) string {
	sender := message.From
	if r.Kernel != nil {
		if process, err := r.Kernel.Get(message.From); err == nil && process.Metadata["agent_id"] != "" {
			sender = fmt.Sprintf("%s (%s)", process.Metadata["agent_id"], message.From)
		}
	}
	via := ""
	if message.Topic != "" {
		via = " on topic " + message.Topic
	}
	// A body line that looks like a sender label is quoted, so it cannot pass
	// for a separate message from someone else.
	lines := strings.Split(message.Body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), PeerMessagePrefix) {
			lines[i] = "> " + line
		}
	}
	return fmt.Sprintf("%s%s%s [%s]: %s", PeerMessagePrefix, sender, via, message.ID, strings.Join(lines, "\n"))
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
