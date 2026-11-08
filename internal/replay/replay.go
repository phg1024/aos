// Package replay re-executes a recorded single-agent workflow run against
// its record (replay-design.md, level b). The runtime, kernel, artifact
// store and prompt assembly run for real; what came from outside comes
// from the record: the model's replies (the call ledger's outcome lines),
// the world tools' results (the run's tool_result and tool_error
// artifacts), and the clock (each step's recorded reading, and the kernel's
// and store's recorded timestamps). The verdict is that every request the
// replay sends hashes to the recorded one and that the kernel and the
// artifact store end up the same; otherwise the first divergence is
// reported.
package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/inference"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runlog"
	"aos/internal/runtime"
	"aos/internal/skills"
	"aos/internal/tools"
)

// Options configure a replay.
type Options struct {
	// StateDir is the recorded run's state directory. It is only read.
	StateDir string
	// WorkDir holds the replay's own state; empty means a temporary
	// directory, removed afterwards unless Keep.
	WorkDir string
	Keep    bool
	// UnpinnedClock leaves the budget clock on the wall clock, which shows
	// what pinning prevents.
	UnpinnedClock bool
}

// Report is a replay's verdict.
type Report struct {
	StateDir  string `json:"state_dir"`
	ReplayDir string `json:"replay_dir,omitempty"`
	Workflow  string `json:"workflow"`
	// Calls counts the recorded calls (start lines), Matched those the
	// replay sent with an identical request.
	Calls   int `json:"calls"`
	Matched int `json:"matched"`
	// Tools counts the tool results taken from the record.
	Tools int `json:"tools"`
	// Events and Artifacts count what was compared.
	Events    int `json:"events"`
	Artifacts int `json:"artifacts"`
	// InFlight means the record ends with a call in flight (the run was
	// killed): the replay reproduces what came before it, and the kernel
	// and store are compared up to where the record ends.
	InFlight bool `json:"record_ends_in_flight,omitempty"`
	// LegacyRunStarted means the record's run.started predates recording
	// settings as they take effect, so it was compared by its settings'
	// meaning rather than byte for byte.
	LegacyRunStarted bool `json:"legacy_run_started,omitempty"`
	// LegacyToolIntents means the record ran world tools without
	// tool.started intents, as AOS did before them, so the replay records
	// none either.
	LegacyToolIntents bool `json:"legacy_tool_intents,omitempty"`
	// LegacyLeases means the record predates lease fencing, so the replay
	// neither fenced file writes nor showed lease tokens.
	LegacyLeases bool `json:"legacy_leases,omitempty"`
	// LegacyPeerFraming means the record predates framing, so the replay
	// rendered peer messages and prompts the way it did then.
	LegacyPeerFraming bool `json:"legacy_peer_framing,omitempty"`
	// LegacyChainDelivery means the record predates chain-owned delivery, so
	// the replay records no process.closed, ipc.subscribed or ipc.unsubscribed.
	LegacyChainDelivery bool `json:"legacy_chain_delivery,omitempty"`
	// LegacySenderOutcomes means the record predates dead letters, so the
	// replay tracks no shown mail and acknowledges every recorded message.
	LegacySenderOutcomes bool `json:"legacy_sender_outcomes,omitempty"`
	// LegacyBusBounds means the record predates the cap, the bound and handles,
	// so the replay takes any size into any mailbox.
	LegacyBusBounds bool `json:"legacy_bus_bounds,omitempty"`
	// LegacyPublishSource means the record predates publish_artifact's path,
	// and it replayed with typed bodies only.
	LegacyPublishSource bool `json:"legacy_publish_source,omitempty"`
	// LegacyDurationArgs means the record predates tools.DurationArg, and it replayed
	// with the older prompt and acquire_lease's older reading of ttl.
	LegacyDurationArgs bool `json:"legacy_duration_args,omitempty"`
	// LegacyDelegateHandles means the record predates handles at a delegation, and it
	// replayed with the older prompt.
	LegacyDelegateHandles bool `json:"legacy_delegate_handles,omitempty"`
	// LegacyDelegateAttach means the record predates delegate_attach, and it replayed
	// reading a reply's artifacts in one place only.
	LegacyDelegateAttach bool              `json:"legacy_delegate_attach,omitempty"`
	RecordedBuild        map[string]string `json:"recorded_build,omitempty"`
	Divergence           *Divergence       `json:"divergence,omitempty"`
}

// Identical reports whether the replay found no divergence.
func (r Report) Identical() bool { return r.Divergence == nil }

// Divergence is the first place the replay and the record differ.
type Divergence struct {
	// Kind is request, calls, tool, kernel or artifacts.
	Kind string `json:"kind"`
	// Call is the ledger seq of the recorded call's start line, Step its
	// step, and PID its process.
	Call   int    `json:"call,omitempty"`
	Step   int    `json:"step,omitempty"`
	PID    string `json:"pid,omitempty"`
	Detail string `json:"detail"`
	// Recorded and Replayed show the differing values, clipped.
	Recorded string `json:"recorded,omitempty"`
	Replayed string `json:"replayed,omitempty"`
}

// ErrNotReplayable means the record lacks what a replay needs.
var ErrNotReplayable = errors.New("run cannot be replayed")

// errDiverged and errRecordEnded stop the replayed run.
var (
	errDiverged    = errors.New("replay diverged from the record")
	errRecordEnded = errors.New("the record ends here: the call was in flight")
)

// record is a recorded run, read without changing it.
type record struct {
	dir       string
	calls     []inference.CallRecord
	events    []kernel.Event
	processes map[string]kernel.Process
	artifacts map[string]artifact.Artifact
}

func load(dir string) (record, error) {
	rec := record{dir: dir}
	var err error
	if rec.calls, err = runlog.Read[inference.CallRecord](runlog.Path(dir, runlog.CallsFile)); err != nil {
		return rec, err
	}
	if err := readJSON(filepath.Join(dir, "kernel", "events.json"), &rec.events); err != nil {
		return rec, err
	}
	if err := readJSON(filepath.Join(dir, "kernel", "processes.json"), &rec.processes); err != nil {
		return rec, err
	}
	if err := readJSON(filepath.Join(dir, "artifacts", "index.json"), &rec.artifacts); err != nil {
		return rec, err
	}
	return rec, rec.verify()
}

// verify checks every recorded payload against the sha256 the index holds
// for it, as the store does on each read, before anything is replayed: a
// payload changed on disk would otherwise show up as a divergence at the
// first call it feeds. Entries without a hash predate hashing and are
// trusted, as the store trusts them.
func (r record) verify() error {
	ids := make([]string, 0, len(r.artifacts))
	for id := range r.artifacts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		want := r.artifacts[id].SHA256
		if want == "" {
			continue
		}
		raw, err := r.payload(id)
		if err != nil {
			return fmt.Errorf("%w: artifact %s: %v", ErrNotReplayable, id, err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			return fmt.Errorf("%w: the record was changed: %w: %s (%s) has sha256 %s, the index records %s", ErrNotReplayable, artifact.ErrArtifactTampered, id, r.artifacts[id].Type, shortHash(got), shortHash(want))
		}
	}
	return nil
}

func readJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

// payload is a recorded artifact's payload. The index holds the path the
// run wrote, which may be elsewhere (inside a container), so it is read
// from this state directory.
func (r record) payload(id string) ([]byte, error) {
	return os.ReadFile(filepath.Join(r.dir, "artifacts", "payloads", id+".json"))
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// Run replays the run in opts.StateDir.
func Run(ctx context.Context, opts Options) (Report, error) {
	report := Report{StateDir: opts.StateDir}
	rec, err := load(opts.StateDir)
	if err != nil {
		return report, err
	}
	var started *kernel.Event
	for i := range rec.events {
		if rec.events[i].Type == agentflow.RunStartedEvent {
			started = &rec.events[i]
			break
		}
	}
	if started == nil {
		return report, fmt.Errorf("%w: no %s event (recorded by an older AOS?)", ErrNotReplayable, agentflow.RunStartedEvent)
	}
	if started.Data[agentflow.LeaseClockField] == "" {
		for _, event := range rec.events {
			if granted, ok := leaseGrant(event); ok {
				return report, fmt.Errorf("%w: it granted a lease (%s at %s) before lease expiries were taken from the kernel clock; their wall-clock expires_at cannot be reproduced", ErrNotReplayable, granted, event.ID)
			}
		}
	}
	settings, err := agentflow.ParseRunStarted(started.Data)
	if err != nil {
		return report, fmt.Errorf("%w: %v", ErrNotReplayable, err)
	}
	report.RecordedBuild = map[string]string{}
	for name, value := range settings.Record {
		if strings.HasPrefix(name, "build_") || name == "go_version" {
			report.RecordedBuild[name] = value
		}
	}
	raw, err := rec.payload(started.Data["run_spec"])
	if err != nil {
		return report, fmt.Errorf("%w: run spec: %v", ErrNotReplayable, err)
	}
	var runSpec agentflow.RunSpec
	if err := json.Unmarshal(raw, &runSpec); err != nil {
		return report, fmt.Errorf("%w: run spec: %v", ErrNotReplayable, err)
	}
	report.Workflow = runSpec.Spec.Name
	for _, event := range rec.events {
		if event.Type == agentflow.RestartEvent {
			return report, fmt.Errorf("%w: it restarted an agent (%s of %s as %s at %s), and a restarted run has more than one agent process; replay handles single-agent runs", ErrNotReplayable, event.Type, event.Data["predecessor"], event.ProcessID, event.ID)
		}
	}
	agents := 0
	for _, process := range rec.processes {
		if strings.HasPrefix(process.Role, "agent:") {
			agents++
		}
	}
	if agents != 1 {
		return report, fmt.Errorf("%w: it has %d agent processes; replay handles single-agent runs", ErrNotReplayable, agents)
	}
	var first *inference.CallRecord
	for i := range rec.calls {
		switch {
		case rec.calls[i].Phase == "":
			return report, fmt.Errorf("%w: its call ledger predates recorded replies", ErrNotReplayable)
		case rec.calls[i].Phase == inference.PhaseStart:
			report.Calls++
			if first == nil {
				first = &rec.calls[i]
			}
		}
	}

	workDir := opts.WorkDir
	if workDir == "" {
		if workDir, err = os.MkdirTemp("", "aos-replay-*"); err != nil {
			return report, err
		}
		if !opts.Keep {
			defer os.RemoveAll(workDir)
		}
	}
	if opts.Keep || opts.WorkDir != "" {
		report.ReplayDir = workDir
	}
	k, err := kernel.New(filepath.Join(workDir, "kernel"))
	if err != nil {
		return report, err
	}
	store, err := artifact.New(filepath.Join(workDir, "artifacts"))
	if err != nil {
		return report, err
	}
	leases, err := lease.New(filepath.Join(workDir, "leases"), k)
	if err != nil {
		return report, err
	}
	bus, err := ipc.New(filepath.Join(workDir, "bus"), k)
	if err != nil {
		return report, err
	}
	// The kernel and the store get the recorded timestamps, in order.
	k.SetClocks(sequenceClock(rec.events))
	store.SetClock(func(id string) time.Time {
		if recorded, ok := rec.artifacts[id]; ok {
			return recorded.CreatedAt
		}
		return time.Now().UTC()
	})

	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	player := newPlayer(rec, store, stop, &report)

	config := inference.VLLMConfig{Model: settings.Record["model"], Timeout: time.Hour, Replayer: player}
	if first != nil {
		// The first request carries the sampling as sent; a later cap from
		// a token budget is applied the same way on top of it.
		config.BaseURL, config.Model, config.Stream = first.Backend, first.Params.Model, first.Params.Stream
		config.Sampling = inference.Sampling{Temperature: first.Params.Temperature, TopP: first.Params.TopP, MaxTokens: first.Params.MaxTokens, ThinkingBudget: first.Params.ReasoningBudgetTokens}
	}
	backend := inference.NewVLLMBackend(config)
	if callLog, err := runlog.Open(runlog.Path(workDir, runlog.CallsFile)); err == nil {
		backend.SetCallLog(callLog)
	}
	toolRuntime := tools.NewRuntime()
	toolRuntime.Recorded = player.tool
	toolRuntime.RecordedSource = player.source

	engine := agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Bus:       bus,
		Leases:    leases,
		Tools:     toolRuntime,
		Factory: agentflow.StaticBackendFactory{
			Completer:         backend,
			Skills:            skills.NewRegistry(runSpec.Spec.SkillRoots),
			NoPeerFraming:     started.Data[agentflow.PeerFramingField] == "",
			NoBusBounds:       started.Data[agentflow.BusBoundsField] == "",
			NoPublishSource:   started.Data[agentflow.PublishSourceField] == "",
			NoDurationArgs:    started.Data[agentflow.DurationArgsField] == "",
			NoDelegateHandles: started.Data[agentflow.DelegateHandlesField] == "",
			NoDelegateAttach:  started.Data[agentflow.DelegateAttachField] == "",
		},
		Compaction:   settings.Compaction,
		BackendRetry: settings.BackendRetry,
		Budget:       settings.Budget,
		RunRecord:    settings.Record,
		Sleep:        func(context.Context, time.Duration) {},
	}
	engine.NoToolIntents = !recordsIntents(rec)
	// A run.started without lease_fencing predates fencing: replay its file
	// writes unfenced and its acquire_lease results without the token.
	engine.NoLeaseFencing = started.Data[agentflow.LeaseFencingField] == ""
	// A record without lease_clock granted no lease (or it was refused
	// above); replay it without the field.
	engine.NoLeaseClock = started.Data[agentflow.LeaseClockField] == ""
	report.LegacyLeases = engine.NoLeaseFencing
	// A run.started without peer_framing predates framing: its requests hold
	// the older rendering, so the replay must produce it too.
	engine.NoPeerFraming = started.Data[agentflow.PeerFramingField] == ""
	report.LegacyPeerFraming = engine.NoPeerFraming
	// A run.started without chain_delivery predates process.closed and the
	// subscription events, and the record's event sequence has none.
	engine.NoChainDelivery = started.Data[agentflow.ChainDeliveryField] == ""
	bus.NoSubscriptionEvents = engine.NoChainDelivery
	report.LegacyChainDelivery = engine.NoChainDelivery
	engine.NoSenderOutcomes = started.Data[agentflow.SenderOutcomesField] == ""
	report.LegacySenderOutcomes = engine.NoSenderOutcomes
	engine.NoBusBounds = started.Data[agentflow.BusBoundsField] == ""
	bus.NoBounds = engine.NoBusBounds
	report.LegacyBusBounds = engine.NoBusBounds
	// Replay refuses a run with more than one agent process, so no replayed run
	// re-delegates; the field is reproduced as recorded.
	engine.NoRedelegationClose = started.Data[agentflow.RedelegationCloseField] == ""
	engine.NoPublishSource = started.Data[agentflow.PublishSourceField] == ""
	report.LegacyPublishSource = engine.NoPublishSource
	engine.NoDurationArgs = started.Data[agentflow.DurationArgsField] == ""
	report.LegacyDurationArgs = engine.NoDurationArgs
	engine.NoDelegateHandles = started.Data[agentflow.DelegateHandlesField] == ""
	report.LegacyDelegateHandles = engine.NoDelegateHandles
	// Before delegate_attach a reply's artifacts were read in one place only.
	engine.NoDelegateAttach = started.Data[agentflow.DelegateAttachField] == ""
	report.LegacyDelegateAttach = engine.NoDelegateAttach
	if !opts.UnpinnedClock {
		// Budgets read a fixed origin, the run's start, except at each
		// step's reading, which is the origin plus what the step's call
		// recorded.
		origin := started.Timestamp
		engine.Clock = func() time.Time { return origin }
		engine.StepClock = player.stepClock(origin)
	}
	_, runErr := engine.Run(ctx, runSpec.Spec, runSpec.Request)
	report.LegacyToolIntents = engine.NoToolIntents && player.worldTools > 0
	if report.Divergence != nil {
		return report, nil
	}
	if runErr != nil && !report.InFlight {
		return report, fmt.Errorf("replayed run: %w", runErr)
	}
	if !report.InFlight {
		if missing := player.unplayed(); missing != nil {
			report.Divergence = &Divergence{Kind: "calls", Call: missing.Seq, Step: missing.Step, PID: missing.PID,
				Detail: fmt.Sprintf("the replay made %d of the %d recorded calls; the first it did not make is shown", report.Matched, report.Calls)}
			return report, nil
		}
	}
	compareKernel(rec, k, &report)
	if report.Divergence == nil {
		compareArtifacts(rec, store, &report)
	}
	return report, nil
}

// leaseGrant reports whether event grants a lease with an expiry, and
// names it.
func leaseGrant(event kernel.Event) (string, bool) {
	switch event.Type {
	case "lease.acquired", "lease.renewed", "lease.taken_over", "lease.transferred":
		return event.Type + " " + event.Data["resource"], true
	}
	return "", false
}

// recordsIntents reports whether the record has any tool.started intent.
// One that ran world tools without them predates intents. A record with
// no world tool calls has none either way.
func recordsIntents(rec record) bool {
	for _, event := range rec.events {
		if event.Type == runtime.ToolStartedEvent {
			return true
		}
	}
	return false
}

// sequenceClock returns the recorded events' timestamps one per call to
// next, in order, then a millisecond past the last for each call beyond
// them; peek returns what next would return without advancing.
func sequenceClock(events []kernel.Event) (next, peek func() time.Time) {
	var mu sync.Mutex
	n := 0
	at := func(i int) time.Time {
		if i < len(events) {
			return events[i].Timestamp
		}
		var last time.Time
		if len(events) > 0 {
			last = events[len(events)-1].Timestamp
		}
		return last.Add(time.Duration(i-len(events)+1) * time.Millisecond)
	}
	next = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return at(n - 1)
	}
	peek = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return at(n)
	}
	return next, peek
}

// player answers the replayed run's model calls and tool calls from the
// record.
type player struct {
	rec    record
	store  *artifact.Store
	stop   context.CancelCauseFunc
	report *Report

	mu       sync.Mutex
	queues   map[string][]int // each process's start lines, in order
	next     map[string]int
	outcomes map[int]int // start seq to its outcome's index
	// worldTools counts the world tool calls answered.
	worldTools int
}

func newPlayer(rec record, store *artifact.Store, stop context.CancelCauseFunc, report *Report) *player {
	p := &player{rec: rec, store: store, stop: stop, report: report, queues: map[string][]int{}, next: map[string]int{}, outcomes: map[int]int{}}
	for i, call := range rec.calls {
		switch call.Phase {
		case inference.PhaseStart:
			p.queues[call.PID] = append(p.queues[call.PID], i)
		case inference.PhaseOutcome:
			p.outcomes[call.Start] = i
		}
	}
	return p
}

// diverge records the first divergence and stops the run.
func (p *player) diverge(d Divergence) {
	if p.report.Divergence == nil {
		p.report.Divergence = &d
	}
	p.stop(errDiverged)
}

// Reply answers a model call with the recorded reply, once its request
// matches the recorded request byte for byte.
func (p *player) Reply(ctx context.Context, body []byte) (inference.RecordedReply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, _ := runtime.CallInfoFrom(ctx)
	queue := p.queues[info.PID]
	n := p.next[info.PID]
	if n >= len(queue) {
		p.diverge(Divergence{Kind: "calls", Step: info.Step, PID: info.PID, Detail: "the replay made a model call the record does not have"})
		return inference.RecordedReply{}, errDiverged
	}
	p.next[info.PID] = n + 1
	index := queue[n]
	start := p.rec.calls[index]
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != start.RequestSHA256 {
		p.diverge(requestDivergence(p.rec.calls, index, body))
		return inference.RecordedReply{}, errDiverged
	}
	p.report.Matched++
	outcomeIndex, ok := p.outcomes[start.Seq]
	if !ok {
		p.report.InFlight = true
		p.stop(errRecordEnded)
		return inference.RecordedReply{}, errRecordEnded
	}
	outcome := p.rec.calls[outcomeIndex]
	if outcome.Error != "" {
		if cause := p.stopCause(info.PID, n == len(queue)-1); cause != nil {
			// The original call was cut off by a strict budget or a
			// signal: stop the replayed run with the same cause.
			p.stop(cause)
		}
		return inference.RecordedReply{}, errors.New(outcome.Error)
	}
	return inference.RecordedReply{Content: outcome.Response, PromptTokens: outcome.PromptTokens, CompletionTokens: outcome.CompletionTokens, CachedTokens: outcome.CachedTokens}, nil
}

// stopCause rebuilds the cause that cut pid's run short, from its recorded
// exit, when last says this is where it was cut; nil otherwise.
func (p *player) stopCause(pid string, last bool) error {
	process, ok := p.rec.processes[pid]
	if !last || !ok || process.ExitStatus == nil {
		return nil
	}
	reason := process.ExitStatus.Reason
	for _, sentinel := range []error{runtime.ErrTimeBudgetExceeded, runtime.ErrTerminated} {
		if rest, found := strings.CutPrefix(reason, sentinel.Error()); found {
			return fmt.Errorf("%w%s", sentinel, rest)
		}
	}
	return nil
}

// tool answers a world tool call with the outcome recorded at the artifact
// the replayed outcome will become: the run is the same up to here, so the
// store's next ID is the one the original outcome got.
func (p *player) tool(ctx context.Context, req tools.Request) (tools.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if runtime.WorldTool(req.Tool) {
		p.worldTools++
	}
	id, err := p.store.NextID()
	if err != nil {
		return tools.Result{}, err
	}
	recorded, ok := p.rec.artifacts[id]
	matches := ok && recorded.ProducerPID == req.PID && recorded.Metadata["tool"] == req.Tool
	switch {
	case matches && recorded.Type == "tool_result":
		raw, err := p.rec.payload(id)
		if err != nil {
			return tools.Result{}, err
		}
		var result tools.Result
		if err := json.Unmarshal(raw, &result); err != nil {
			return tools.Result{}, err
		}
		p.report.Tools++
		return result, nil
	case matches && recorded.Type == "tool_error":
		raw, err := p.rec.payload(id)
		if err != nil {
			return tools.Result{}, err
		}
		var failure struct {
			Error   string             `json:"error"`
			Changes *tools.FileChanges `json:"changes"`
		}
		if err := json.Unmarshal(raw, &failure); err != nil {
			return tools.Result{}, err
		}
		p.report.Tools++
		var result tools.Result
		if failure.Changes != nil {
			result = tools.Result{Tool: req.Tool, Changes: failure.Changes}
		}
		return result, tools.RecordedError(failure.Error)
	}
	cause := p.stopCause(req.PID, p.next[req.PID] == len(p.queues[req.PID]))
	if cause != nil && matches && recorded.Type == runtime.ToolCancelledType {
		// The original tool was cut off and recorded so, with the files it
		// had changed.
		raw, err := p.rec.payload(id)
		if err != nil {
			return tools.Result{}, err
		}
		var cancelled struct {
			Changes *tools.FileChanges `json:"changes"`
		}
		if err := json.Unmarshal(raw, &cancelled); err != nil {
			return tools.Result{}, err
		}
		p.report.Tools++
		p.stop(cause)
		<-ctx.Done()
		return tools.Result{Tool: req.Tool, Changes: cancelled.Changes}, fmt.Errorf("%w: %w", tools.ErrToolCancelled, ctx.Err())
	}
	if cause != nil && ok && recorded.Type == "checkpoint" {
		// The original tool was cut off before cut-off calls were recorded:
		// its process's final checkpoint stands where the outcome would have.
		p.stop(cause)
		<-ctx.Done()
		return tools.Result{}, fmt.Errorf("%w: %w", tools.ErrToolCancelled, ctx.Err())
	}
	what := "nothing"
	if ok {
		what = fmt.Sprintf("a %s from %s (tool %q)", recorded.Type, recorded.ProducerPID, recorded.Metadata["tool"])
	}
	p.diverge(Divergence{Kind: "tool", PID: req.PID, Detail: fmt.Sprintf("the replay ran %s, but the record has %s at %s", req.Tool, what, id)})
	return tools.Result{}, errDiverged
}

// source answers a publish by path with the bytes the original stored, at the
// artifact the replayed publication will become, or with its recorded refusal. The
// file need not be there any more.
func (p *player) source(ctx context.Context, req tools.Request) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, err := p.store.NextID()
	if err != nil {
		return nil, err
	}
	recorded, ok := p.rec.artifacts[id]
	if ok && recorded.ProducerPID == req.PID {
		switch {
		case strings.HasPrefix(recorded.Type, artifact.PublishedPrefix) && recorded.Metadata["source_path"] == strings.TrimSpace(req.Args["path"]):
			raw, err := p.rec.payload(id)
			if err != nil {
				return nil, err
			}
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, err
			}
			return []byte(payload.Body), nil
		case recorded.Type == "tool_error" && recorded.Metadata["tool"] == req.Tool:
			raw, err := p.rec.payload(id)
			if err != nil {
				return nil, err
			}
			var failure struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(raw, &failure); err != nil {
				return nil, err
			}
			return nil, tools.RecordedError(failure.Error)
		}
	}
	p.diverge(Divergence{Kind: "tool", PID: req.PID, Detail: fmt.Sprintf("the replay published %s from a file, but the record has no such publication at %s", req.Args["path"], id)})
	return nil, errDiverged
}

// stepClock gives each step the reading its first model call recorded,
// added to origin; a step with no recorded call keeps the process's last.
func (p *player) stepClock(origin time.Time) func(pid string, step int) time.Time {
	elapsed := map[string]map[int]float64{}
	for _, call := range p.rec.calls {
		if call.Phase != inference.PhaseStart {
			continue
		}
		if elapsed[call.PID] == nil {
			elapsed[call.PID] = map[int]float64{}
		}
		if _, seen := elapsed[call.PID][call.Step]; !seen {
			elapsed[call.PID][call.Step] = call.ElapsedSeconds
		}
	}
	last := map[string]float64{}
	return func(pid string, step int) time.Time {
		if seconds, ok := elapsed[pid][step]; ok {
			last[pid] = seconds
		}
		return origin.Add(time.Duration(last[pid] * float64(time.Second)))
	}
}

// unplayed is the first recorded call the replay did not make, if any.
func (p *player) unplayed() *inference.CallRecord {
	var first *inference.CallRecord
	for pid, queue := range p.queues {
		if n := p.next[pid]; n < len(queue) {
			call := p.rec.calls[queue[n]]
			if first == nil || call.Seq < first.Seq {
				first = &call
			}
		}
	}
	return first
}

// sentRequest is the part of a request body a divergence shows.
type sentRequest struct {
	Messages []inference.ChatMessage `json:"messages"`
}

// requestDivergence says where the replayed request at the recorded start
// records[index] differs from it: the first differing message, or the
// parameters.
func requestDivergence(records []inference.CallRecord, index int, body []byte) Divergence {
	start := records[index]
	d := Divergence{Kind: "request", Call: start.Seq, Step: start.Step, PID: start.PID}
	recorded := inference.RequestMessages(records, index)
	var sent sentRequest
	_ = json.Unmarshal(body, &sent)
	for i := 0; i < max(len(recorded), len(sent.Messages)); i++ {
		switch {
		case i >= len(recorded):
			d.Detail = fmt.Sprintf("the replayed request has %d messages, the recorded %d", len(sent.Messages), len(recorded))
			d.Replayed = clip(sent.Messages[i].Content, 0)
			return d
		case i >= len(sent.Messages):
			d.Detail = fmt.Sprintf("the replayed request has %d messages, the recorded %d", len(sent.Messages), len(recorded))
			d.Recorded = clip(recorded[i].Content, 0)
			return d
		case recorded[i] != sent.Messages[i]:
			at := firstDifference(recorded[i].Content, sent.Messages[i].Content)
			d.Detail = fmt.Sprintf("message %d (%s) differs at character %d", i+1, recorded[i].Role, at)
			d.Recorded, d.Replayed = clip(recorded[i].Content, at), clip(sent.Messages[i].Content, at)
			return d
		}
	}
	d.Detail = "the messages are the same; the parameters differ"
	if rebuilt, err := inference.RebuildRequestBody(start.Params, recorded); err == nil {
		d.Recorded, d.Replayed = string(paramsOnly(rebuilt)), string(paramsOnly(body))
	}
	return d
}

func paramsOnly(body []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil
	}
	delete(fields, "messages")
	out, _ := json.Marshal(fields)
	return out
}

func firstDifference(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

// clip shows value around byte at.
func clip(value string, at int) string {
	const before, after = 80, 160
	from, to := max(0, at-before), min(len(value), at+after)
	out := value[from:to]
	if from > 0 {
		out = "…" + out
	}
	if to < len(value) {
		out += "…"
	}
	return out
}

// legacyRunStarted reports whether event is a run.started recorded before
// settings were recorded as they take effect (with *_source, budget_time
// and max_steps_*): its settings are the engine's raw ones, zero for a
// default. The replay sets the run up from them and records the current
// format, so the two are compared by meaning.
func legacyRunStarted(event kernel.Event) bool {
	_, current := event.Data["compaction_source"]
	return event.Type == agentflow.RunStartedEvent && !current
}

// sameLegacyRunStarted compares a legacy run.started with the replay's: the
// same budget, the same compaction and retry settings as they take effect,
// and every other field equal. The derived fields the legacy format lacks
// (budget_time, *_source, max_steps_*) are left out; max_steps_* comes
// from the run spec, which run_spec_sha256 already pins.
func sameLegacyRunStarted(recorded, replayed kernel.Event) bool {
	want, err := agentflow.ParseRunStarted(recorded.Data)
	if err != nil {
		return false
	}
	got, err := agentflow.ParseRunStarted(replayed.Data)
	if err != nil {
		return false
	}
	if want.Budget != got.Budget || want.Compaction.Effective() != got.Compaction.Effective() || want.BackendRetry.Effective() != got.BackendRetry.Effective() {
		return false
	}
	rest := func(event kernel.Event) kernel.Event {
		data := map[string]string{}
		for name, value := range event.Data {
			if !runSetting(name) {
				data[name] = value
			}
		}
		event.Data = data
		return event
	}
	return reflect.DeepEqual(rest(recorded), rest(replayed))
}

// runSetting reports whether a run.started field is one of the settings
// sameLegacyRunStarted compares by meaning, or derived from them.
func runSetting(name string) bool {
	for _, prefix := range []string{"budget_", "compact_", "retry_", "max_steps_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "compaction_source"
}

// compareKernel compares the replay's events and processes with the
// record's: all of them, or the record's prefix when it ends in flight.
func compareKernel(rec record, k *kernel.Kernel, report *Report) {
	events, err := k.Events()
	if err != nil {
		report.Divergence = &Divergence{Kind: "kernel", Detail: err.Error()}
		return
	}
	if !report.InFlight && len(events) != len(rec.events) {
		report.Divergence = &Divergence{Kind: "kernel", Detail: fmt.Sprintf("the replay recorded %d kernel events, the record %d", len(events), len(rec.events))}
	}
	for i := range min(len(events), len(rec.events)) {
		same := reflect.DeepEqual(events[i], rec.events[i])
		if !same && legacyRunStarted(rec.events[i]) {
			same = sameLegacyRunStarted(rec.events[i], events[i])
			report.LegacyRunStarted = true
		}
		if !same {
			recorded, _ := json.Marshal(rec.events[i])
			replayed, _ := json.Marshal(events[i])
			report.Divergence = &Divergence{Kind: "kernel", PID: rec.events[i].ProcessID, Detail: fmt.Sprintf("kernel event %d (%s) differs", i+1, rec.events[i].ID),
				Recorded: string(recorded), Replayed: string(replayed)}
			return
		}
	}
	if report.Divergence != nil {
		return
	}
	report.Events = len(rec.events)
	if report.InFlight {
		return
	}
	processes, err := k.Processes()
	if err != nil {
		report.Divergence = &Divergence{Kind: "kernel", Detail: err.Error()}
		return
	}
	for _, process := range processes {
		recorded, ok := rec.processes[process.PID]
		if !ok || !reflect.DeepEqual(process, recorded) {
			want, _ := json.Marshal(recorded)
			got, _ := json.Marshal(process)
			report.Divergence = &Divergence{Kind: "kernel", PID: process.PID, Detail: "process " + process.PID + " differs", Recorded: string(want), Replayed: string(got)}
			return
		}
	}
	if len(processes) != len(rec.processes) {
		report.Divergence = &Divergence{Kind: "kernel", Detail: fmt.Sprintf("the replay has %d processes, the record %d", len(processes), len(rec.processes))}
	}
}

// compareArtifacts compares the replay's artifacts with the record's, by
// ID: type, producer, parents, metadata, creation time and payload. The
// payload path differs by design, and so do the seconds a checkpoint's
// usage records, since they come from the wall clock at save time; both
// are left out.
func compareArtifacts(rec record, store *artifact.Store, report *Report) {
	replayed, err := store.List()
	if err != nil {
		report.Divergence = &Divergence{Kind: "artifacts", Detail: err.Error()}
		return
	}
	byID := map[string]artifact.Artifact{}
	for _, record := range replayed {
		byID[record.ID] = record
	}
	ids := make([]string, 0, len(rec.artifacts))
	for id := range rec.artifacts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		want := rec.artifacts[id]
		got, ok := byID[id]
		if !ok {
			if report.InFlight {
				continue
			}
			report.Divergence = &Divergence{Kind: "artifacts", PID: want.ProducerPID, Detail: fmt.Sprintf("the replay has no %s (%s)", id, want.Type)}
			return
		}
		if want.Type != got.Type || want.ProducerPID != got.ProducerPID || !reflect.DeepEqual(want.ParentIDs, got.ParentIDs) ||
			!reflect.DeepEqual(want.Metadata, got.Metadata) || !want.CreatedAt.Equal(got.CreatedAt) {
			report.Divergence = &Divergence{Kind: "artifacts", PID: want.ProducerPID, Detail: fmt.Sprintf("%s (%s) has a different record", id, want.Type),
				Recorded: fmt.Sprintf("%s %s %v %v %s", want.Type, want.ProducerPID, want.ParentIDs, want.Metadata, want.CreatedAt.Format(time.RFC3339Nano)),
				Replayed: fmt.Sprintf("%s %s %v %v %s", got.Type, got.ProducerPID, got.ParentIDs, got.Metadata, got.CreatedAt.Format(time.RFC3339Nano))}
			return
		}
		recordedPayload, err := rec.payload(id)
		if err != nil {
			report.Divergence = &Divergence{Kind: "artifacts", Detail: err.Error()}
			return
		}
		replayedPayload, err := os.ReadFile(got.Path)
		if err != nil {
			report.Divergence = &Divergence{Kind: "artifacts", Detail: err.Error()}
			return
		}
		if a, b := normalize(recordedPayload), normalize(replayedPayload); a != b {
			at := firstDifference(a, b)
			report.Divergence = &Divergence{Kind: "artifacts", PID: want.ProducerPID, Detail: fmt.Sprintf("%s (%s) has a different payload", id, want.Type), Recorded: clip(a, at), Replayed: clip(b, at)}
			return
		}
		report.Artifacts++
	}
	if !report.InFlight && len(replayed) != len(rec.artifacts) {
		report.Divergence = &Divergence{Kind: "artifacts", Detail: fmt.Sprintf("the replay has %d artifacts, the record %d", len(replayed), len(rec.artifacts))}
	}
}

// normalize is a payload as canonical JSON without the wall-clock seconds
// of any usage object.
func normalize(raw []byte) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	var strip func(any)
	strip = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, child := range v {
				if key == "usage" {
					if usage, ok := child.(map[string]any); ok {
						delete(usage, "seconds")
					}
				}
				strip(child)
			}
		case []any:
			for _, child := range v {
				strip(child)
			}
		}
	}
	strip(value)
	out, _ := json.Marshal(value)
	return string(out)
}
