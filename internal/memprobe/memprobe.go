// Package memprobe measures what an agent can recover from its own memory.
// A probe resumes an agent from one of its checkpoints with a single
// question whose answer lies somewhere in its history, live or compacted,
// and scores the answer against a pattern.
//
// Probes run against a copy of a run's kernel and artifact index, so the
// original run is never changed, and the probe process gets no tools but
// the memory ones, so it cannot touch the workspace or redo the work.
// Several samples per probe give the variance of a model at temperature.
package memprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/skills"
	"aos/internal/tools"
)

// Probe is one question asked of the agent that wrote Checkpoint.
type Probe struct {
	ID string `json:"id"`
	// Kind says where the answer lives: "archive" (only in compacted
	// memory), "summary" (also in the header's segment summaries), "live"
	// (in the live history, so no memory read is needed) or "superseded"
	// (the archive holds an older answer that was later overturned).
	Kind       string `json:"kind"`
	Checkpoint string `json:"checkpoint"`
	Question   string `json:"question"`
	// Topic, if set, must match any answer that counts: it keeps a reply
	// about something else from scoring on a stray word. Expect matches a
	// correct answer; Reject, if set, matches an answer that states the
	// superseded fact as current. All three are Go regexps.
	Topic  string `json:"topic,omitempty"`
	Expect string `json:"expect"`
	Reject string `json:"reject,omitempty"`
	// WorkingMemory, if set, is given to the agent as its working memory
	// at the checkpoint, in place of any it had, unless
	// Config.NoWorkingMemory. Without it the agent keeps the checkpoint's
	// own working memory, if any. It is written by
	// whoever wrote the probe, not by the agent.
	WorkingMemory []ProbeMemoryEntry `json:"working_memory,omitempty"`
}

// ProbeMemoryEntry is one working-memory entry a probe gives the agent.
type ProbeMemoryEntry struct {
	Section  string `json:"section"`
	Key      string `json:"key"`
	Content  string `json:"content"`
	Evidence string `json:"evidence,omitempty"`
}

// Sample is one probe answered once.
type Sample struct {
	Probe       string `json:"probe"`
	Kind        string `json:"kind"`
	Sample      int    `json:"sample"`
	PID         string `json:"pid"`
	Answer      string `json:"answer"`
	Correct     bool   `json:"correct"`
	Superseded  bool   `json:"superseded"`
	OffTopic    bool   `json:"off_topic,omitempty"`
	Steps       int    `json:"steps"`
	MemoryReads int    `json:"memory_reads"`
	Searches    int    `json:"searches,omitempty"`
	Updates     int    `json:"memory_updates,omitempty"`
	// Condition is what the agent was given: the probe's working memory
	// ("memory"), the checkpoint's own ("own-memory") or none
	// ("no-memory"), and the rule to check it before deciding or not.
	Condition string `json:"condition,omitempty"`
	// WorkingMemory is the working_memory version the agent resumed with,
	// empty if none.
	WorkingMemory string `json:"working_memory,omitempty"`
	BytesRead     int    `json:"bytes_read"`
	ToolErrors    int    `json:"tool_errors"`
	// BackendErrors counts model calls that failed. Steps counts every
	// call, so with BackendRetry set it includes the retried ones.
	BackendErrors int `json:"backend_errors,omitempty"`
	// AgentIndexSegments and AgentIndexBytes are how many agent-written
	// segment indexes, and how many bytes of them, the first prompt's
	// header showed; zero unless a1 is on.
	AgentIndexSegments int `json:"agent_index_segments,omitempty"`
	AgentIndexBytes    int `json:"agent_index_bytes,omitempty"`
	// AgentIndexShown and AgentIndexHidden are, by memory_segment ID, how
	// many lines of each agent index that header showed and hid; the
	// header shows an index's first lines.
	AgentIndexShown  map[string]int `json:"agent_index_shown,omitempty"`
	AgentIndexHidden map[string]int `json:"agent_index_hidden,omitempty"`
	// AgentIndexBudget is the header's total for agent indexes it was
	// shown under; zero unless a1 is on.
	AgentIndexBudget int `json:"agent_index_budget,omitempty"`
	// AgentIndexPartialNote and AgentIndexShortenedNote say whether that
	// header said the agent index is partial, and how to read a shortened
	// index; false unless a1 and a1p are on.
	AgentIndexPartialNote   bool `json:"agent_index_partial_note,omitempty"`
	AgentIndexShortenedNote bool `json:"agent_index_shortened_note,omitempty"`
	// Tokens is what the answer's model calls used, as the backend
	// reported it; zero if it reports none.
	Tokens int `json:"tokens,omitempty"`
	// Reads lists each memory read and search with its arguments, in
	// order, as "read_memory artifact_id=art-1 page=2".
	Reads []string `json:"reads,omitempty"`
	Error string   `json:"error,omitempty"`
}

// Config is where probes run and with what model.
type Config struct {
	// StateDir is the run's state directory. It is read, never written.
	StateDir string
	// WorkDir receives the copy probes run in; a temporary directory if
	// empty.
	WorkDir   string
	Workflow  agentflow.WorkflowSpec
	Skills    skills.Registry
	Completer agentflow.JSONCompleter
	// Compaction is the policy the probed run used.
	Compaction runtime.CompactionPolicy
	Samples    int
	// MaxSteps bounds one answer (default 12).
	MaxSteps int
	// NoWorkingMemory leaves out every working memory, the probes' and
	// the checkpoint's own, and NoCheckRule the prompt's rule to check it
	// before deciding: the conditions phase 3 is measured under.
	NoWorkingMemory bool
	NoCheckRule     bool
	// Recall names the recall aids turned on: "w1" (overwrite warning),
	// "w2" (pointer wording), "w4" (handoff renotice), "a1" (agent
	// index) and "a1p" (the header's note that the agent index is
	// partial; it shows only with a1). Every aid not named is off, so a Config that names none
	// runs with the prompts from before the aids existed. The runtime's
	// zero RecallPolicy turns every aid on, so it is never passed as is.
	Recall []string
	// BackendRetry, if set, retries a failed model call within its step;
	// nil turns retrying off, so a failure takes a step as before.
	BackendRetry *runtime.BackendRetryPolicy
	// AgentIndexes, by memory_segment ID, are agent-written indexes shown
	// with those segments wherever a probe's checkpoint holds them, as if
	// the agent had written them at the segment's handoff. The header
	// shows them only with a1 on. They are set on the checkpoint's
	// segment references; the segments themselves are unchanged, so
	// read_memory does not repeat them.
	AgentIndexes map[string][]runtime.IndexLine
	// HeaderIndexBytes is the header's total for agent indexes
	// (runtime.RecallPolicy.HeaderIndexBytes); zero is the runtime's
	// default.
	HeaderIndexBytes int
}

// RecallAids are the names Config.Recall takes, in the order a condition
// lists them.
var RecallAids = []string{"w1", "w2", "w4", "a1", "a1p"}

// RecallPolicy is the runtime policy that turns on exactly the named aids.
func RecallPolicy(aids []string) (runtime.RecallPolicy, error) {
	policy := runtime.RecallOff()
	for _, aid := range aids {
		switch aid {
		case "w1":
			policy.NoOverwriteWarning = false
		case "w2":
			policy.NoPointerPrompt = false
		case "w4":
			policy.NoHandoffRenotice = false
		case "a1":
			policy.NoAgentIndex = false
		case "a1p":
			policy.NoPartialIndexNote = false
		default:
			return runtime.RecallPolicy{}, fmt.Errorf("unknown recall aid %q (want %s)", aid, strings.Join(RecallAids, ", "))
		}
	}
	return policy, nil
}

// retryPolicy is the runtime's retry policy for cfg: off unless set.
func (c Config) retryPolicy() runtime.BackendRetryPolicy {
	if c.BackendRetry == nil {
		return runtime.BackendRetryPolicy{Attempts: -1}
	}
	return *c.BackendRetry
}

// recallCondition lists the aids turned on, as "recall=w1+a1", or "" for
// none, so conditions from before the aids read as they did.
func (c Config) recallCondition() string {
	var on []string
	for _, aid := range RecallAids {
		if slices.Contains(c.Recall, aid) {
			on = append(on, aid)
		}
	}
	if len(on) == 0 {
		return ""
	}
	return ",recall=" + strings.Join(on, "+")
}

// condition names the conditions a sample ran under, given the working
// memory version it resumes with.
func (c Config) condition(probe Probe, workingMemory string) string {
	memory := "no-memory"
	switch {
	case workingMemory == "":
	case len(probe.WorkingMemory) > 0:
		memory = "memory"
	default:
		memory = "own-memory"
	}
	if c.NoCheckRule {
		return memory + ",no-check-rule" + c.recallCondition()
	}
	return memory + ",check-rule" + c.recallCondition()
}

// questionPrompt frames a probe as an operator's question that should be
// answered, not acted on.
const questionPrompt = "Question from the operator, answered from what you already know: %s\n" +
	"Use your notes, your history and, if needed, your compacted memory. Do not start new work. " +
	"Reply with a message action that states the answer and how you know it."

// LoadProbes reads a JSON array of probes and checks their patterns.
func LoadProbes(path string) ([]Probe, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probes []Probe
	if err := json.Unmarshal(bytes, &probes); err != nil {
		return nil, err
	}
	for _, probe := range probes {
		if probe.ID == "" || probe.Checkpoint == "" || probe.Question == "" || probe.Expect == "" {
			return nil, fmt.Errorf("probe %q needs id, checkpoint, question and expect", probe.ID)
		}
		if _, err := regexp.Compile(probe.Expect); err != nil {
			return nil, fmt.Errorf("probe %s expect: %w", probe.ID, err)
		}
		if _, err := regexp.Compile(probe.Reject); err != nil {
			return nil, fmt.Errorf("probe %s reject: %w", probe.ID, err)
		}
		if _, err := regexp.Compile(probe.Topic); err != nil {
			return nil, fmt.Errorf("probe %s topic: %w", probe.ID, err)
		}
		for _, entry := range probe.WorkingMemory {
			if !slices.Contains(runtime.MemorySections, entry.Section) || entry.Key == "" || entry.Content == "" {
				return nil, fmt.Errorf("probe %s: working memory entries need a section of %v, a key and content", probe.ID, runtime.MemorySections)
			}
		}
	}
	return probes, nil
}

// Run answers every probe cfg.Samples times. A sample that fails is
// recorded with its error; only a failure to set up stops the run.
func Run(ctx context.Context, cfg Config, probes []Probe) ([]Sample, error) {
	if cfg.Samples <= 0 {
		cfg.Samples = 1
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 12
	}
	if _, err := RecallPolicy(cfg.Recall); err != nil {
		return nil, err
	}
	k, store, err := openCopy(cfg)
	if err != nil {
		return nil, err
	}

	var samples []Sample
	for _, probe := range probes {
		owner, agent, err := probeAgent(k, store, cfg.Workflow, probe)
		if err != nil {
			return nil, fmt.Errorf("probe %s: %w", probe.ID, err)
		}
		loaded, err := cfg.Skills.LoadMany(agent.Skills)
		if err != nil {
			return nil, err
		}
		checkpoint := probe.Checkpoint
		switch {
		case cfg.NoWorkingMemory:
			checkpoint, err = stripWorkingMemory(store, owner, probe)
		case len(probe.WorkingMemory) > 0:
			checkpoint, err = giveWorkingMemory(store, owner, probe)
		}
		if err != nil {
			return nil, fmt.Errorf("probe %s: %w", probe.ID, err)
		}
		if len(cfg.AgentIndexes) > 0 {
			if checkpoint, err = giveAgentIndexes(store, owner, probe, checkpoint, cfg.AgentIndexes); err != nil {
				return nil, fmt.Errorf("probe %s: %w", probe.ID, err)
			}
		}
		var session runtime.Session
		if err := store.ReadJSON(checkpoint, &session); err != nil {
			return nil, fmt.Errorf("probe %s: %w", probe.ID, err)
		}
		for n := 1; n <= cfg.Samples; n++ {
			if ctx.Err() != nil {
				return samples, ctx.Err()
			}
			sample := ask(ctx, cfg, k, store, owner, agent, loaded, probe, checkpoint, session.WorkingMemory)
			sample.Sample = n
			samples = append(samples, sample)
		}
	}
	return samples, nil
}

// openCopy copies the run's state into cfg.WorkDir, or a temporary
// directory, and opens the copy's kernel and artifact index.
func openCopy(cfg Config) (*kernel.Kernel, *artifact.Store, error) {
	work := cfg.WorkDir
	if work == "" {
		dir, err := os.MkdirTemp("", "aos-memprobe-*")
		if err != nil {
			return nil, nil, err
		}
		work = dir
	}
	if err := copyState(cfg.StateDir, work); err != nil {
		return nil, nil, err
	}
	k, err := kernel.New(filepath.Join(work, "kernel"))
	if err != nil {
		return nil, nil, err
	}
	store, err := artifact.New(filepath.Join(work, "artifacts"))
	if err != nil {
		return nil, nil, err
	}
	return k, store, nil
}

// probeAgent finds the process that wrote the probe's checkpoint and its
// agent spec, stripped of every capability: the system prompt then lists
// only search_memory and read_memory, which are always allowed.
func probeAgent(k *kernel.Kernel, store *artifact.Store, spec agentflow.WorkflowSpec, probe Probe) (kernel.Process, agentflow.AgentSpec, error) {
	record, err := store.Metadata(probe.Checkpoint)
	if err != nil {
		return kernel.Process{}, agentflow.AgentSpec{}, err
	}
	if record.Type != "checkpoint" {
		return kernel.Process{}, agentflow.AgentSpec{}, fmt.Errorf("%s is a %s, not a checkpoint", probe.Checkpoint, record.Type)
	}
	// Payload paths in the index are as the run recorded them, usually
	// relative to the directory it ran in.
	if _, err := os.Stat(record.Path); err != nil {
		return kernel.Process{}, agentflow.AgentSpec{}, fmt.Errorf("checkpoint payload %s is not readable from here; run from the directory the run was started in: %w", record.Path, err)
	}
	owner, err := k.Get(record.ProducerPID)
	if err != nil {
		return kernel.Process{}, agentflow.AgentSpec{}, err
	}
	for _, agent := range spec.Agents {
		if agent.ID == owner.Metadata["agent_id"] {
			agent.Capabilities = capability.Set{}
			agent.Delegable = capability.Set{}
			agent.AllowedAgents = nil
			agent.Mounts = nil
			return owner, agent, nil
		}
	}
	return kernel.Process{}, agentflow.AgentSpec{}, fmt.Errorf("agent %q of %s is not in workflow %s", owner.Metadata["agent_id"], owner.PID, spec.Name)
}

// giveWorkingMemory writes the probe's working memory as a version the
// checkpoint's owner holds, and a checkpoint like the probe's that shows
// it, in the copy probes run in. It returns that checkpoint.
func giveWorkingMemory(store *artifact.Store, owner kernel.Process, probe Probe) (string, error) {
	memory := runtime.WorkingMemory{ProcessID: owner.PID, Version: 1, Update: runtime.MemoryUpdate{Section: "probe"}}
	for _, name := range runtime.MemorySections {
		section := runtime.MemorySection{Name: name}
		for _, entry := range probe.WorkingMemory {
			if entry.Section == name {
				section.Entries = append(section.Entries, runtime.MemoryEntry{Key: entry.Key, Content: entry.Content, Evidence: entry.Evidence})
			}
		}
		memory.Sections = append(memory.Sections, section)
	}
	version, err := store.CreateJSON("working_memory", owner.PID, memory, nil, map[string]string{"version": "1", "memprobe": probe.ID})
	if err != nil {
		return "", err
	}
	var session runtime.Session
	if err := store.ReadJSON(probe.Checkpoint, &session); err != nil {
		return "", err
	}
	session.WorkingMemory, session.MemorySnapshot = version.ID, version.ID
	checkpoint, err := store.CreateJSON("checkpoint", owner.PID, session, []string{probe.Checkpoint, version.ID}, map[string]string{"kind": "intermediate", "memprobe": probe.ID})
	if err != nil {
		return "", err
	}
	return checkpoint.ID, nil
}

// giveAgentIndexes writes a checkpoint like the given one whose segment
// references carry the indexes for their segments, in the copy probes run
// in. Each line must lie in its segment and each index must fit the
// runtime's cap. It returns the given checkpoint when no index applies.
func giveAgentIndexes(store *artifact.Store, owner kernel.Process, probe Probe, checkpoint string, indexes map[string][]runtime.IndexLine) (string, error) {
	var session runtime.Session
	if err := store.ReadJSON(checkpoint, &session); err != nil {
		return "", err
	}
	given := 0
	for i, ref := range session.MemoryRefs {
		lines, ok := indexes[ref.ArtifactID]
		if !ok {
			continue
		}
		for _, line := range lines {
			if line.From < ref.FromSeq || line.To > ref.ToSeq || line.From > line.To {
				return "", fmt.Errorf("index line %q lies outside segment %s (%d-%d)", line, ref.ArtifactID, ref.FromSeq, ref.ToSeq)
			}
		}
		if size := len(runtime.RenderIndex(lines)); size > runtime.MaxSegmentIndexBytes {
			return "", fmt.Errorf("index of %s is %d bytes, over the runtime's %d", ref.ArtifactID, size, runtime.MaxSegmentIndexBytes)
		}
		session.MemoryRefs[i].Index = lines
		given++
	}
	if given == 0 {
		return checkpoint, nil
	}
	record, err := store.CreateJSON("checkpoint", owner.PID, session, []string{checkpoint}, map[string]string{"kind": "intermediate", "memprobe": probe.ID, "agent_indexes": strconv.Itoa(given)})
	if err != nil {
		return "", err
	}
	return record.ID, nil
}

// stripWorkingMemory writes a checkpoint like the probe's without its
// working memory, so neither the header nor search_memory shows it; the
// history and archive, update_memory calls included, are unchanged. It
// returns the probe's checkpoint when there is nothing to strip.
func stripWorkingMemory(store *artifact.Store, owner kernel.Process, probe Probe) (string, error) {
	var session runtime.Session
	if err := store.ReadJSON(probe.Checkpoint, &session); err != nil {
		return "", err
	}
	if session.WorkingMemory == "" && session.MemorySnapshot == "" {
		return probe.Checkpoint, nil
	}
	session.WorkingMemory, session.MemorySnapshot = "", ""
	checkpoint, err := store.CreateJSON("checkpoint", owner.PID, session, []string{probe.Checkpoint}, map[string]string{"kind": "intermediate", "memprobe": probe.ID, "working_memory": "stripped"})
	if err != nil {
		return "", err
	}
	return checkpoint.ID, nil
}

// countingBackend counts model calls, one per step unless retried, and
// keeps the header sizes of the first prompt.
type countingBackend struct {
	runtime.Backend
	calls  int
	errors int
	header map[string]int
}

func (b *countingBackend) Generate(ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	b.calls++
	if b.header == nil {
		b.header = b.HeaderSizes(request)
	}
	result, err := b.Backend.Generate(ctx, request)
	if err != nil {
		b.errors++
	}
	return result, err
}

// headerLines collects the header sizes under prefix by the rest of their
// key, or nil if there are none.
func headerLines(sizes map[string]int, prefix string) map[string]int {
	var lines map[string]int
	for key, n := range sizes {
		if id, ok := strings.CutPrefix(key, prefix); ok {
			if lines == nil {
				lines = map[string]int{}
			}
			lines[id] = n
		}
	}
	return lines
}

// HeaderSizes passes the wrapped backend's header sizes on, so the
// runtime logs them as it would without the wrapper.
func (b *countingBackend) HeaderSizes(request runtime.GenerateRequest) map[string]int {
	if sizer, ok := b.Backend.(runtime.HeaderSizer); ok {
		return sizer.HeaderSizes(request)
	}
	return map[string]int{}
}

// probeRuntime is the backend and runtime one probe process runs with:
// the aids cfg turns on, set on both, and its retry policy.
func probeRuntime(cfg Config, k *kernel.Kernel, store *artifact.Store, agent agentflow.AgentSpec, loaded []skills.Document) (*countingBackend, *runtime.AgentRuntime) {
	recall, _ := RecallPolicy(cfg.Recall) // checked when the run starts
	recall.HeaderIndexBytes = cfg.HeaderIndexBytes
	backend := &countingBackend{Backend: agentflow.PromptBackend{Completer: cfg.Completer, Workflow: cfg.Workflow, Agent: agent, Skills: loaded, NoCheckRule: cfg.NoCheckRule, Recall: recall}}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: cfg.MaxSteps, Compaction: cfg.Compaction,
		Recall: recall, BackendRetry: cfg.retryPolicy()}
	return backend, agentRuntime
}

// spawnProbe spawns a process resumed from owner, marked as a probe
// without naming the probe: an ID such as "leaf2-readopted" in the
// header would hint at the answer.
func spawnProbe(k *kernel.Kernel, owner kernel.Process) (kernel.Process, error) {
	metadata := map[string]string{"resumed_from": owner.PID, "memprobe": "true"}
	for key, value := range owner.Metadata {
		if _, set := metadata[key]; !set {
			metadata[key] = value
		}
	}
	return k.Spawn(kernel.ProcessSpec{Role: owner.Role, Metadata: metadata})
}

// ask runs one sample: a fresh process resumed from the checkpoint as if
// recovered from its owner, so its own compacted memory is readable.
func ask(ctx context.Context, cfg Config, k *kernel.Kernel, store *artifact.Store, owner kernel.Process, agent agentflow.AgentSpec, loaded []skills.Document, probe Probe, checkpoint, workingMemory string) Sample {
	sample := Sample{Probe: probe.ID, Kind: probe.Kind, Condition: cfg.condition(probe, workingMemory), WorkingMemory: workingMemory}
	process, err := spawnProbe(k, owner)
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	sample.PID = process.PID
	backend, agentRuntime := probeRuntime(cfg, k, store, agent, loaded)
	options := runtime.RunOptions{
		NewTurn:                true,
		StopOnAssistantMessage: true,
		ContinueOnToolError:    true,
		OnAssistantMessage:     func(text string) { sample.Answer = text },
		OnToolCall: func(call runtime.ToolCall) {
			switch call.Name {
			case "read_memory":
				sample.MemoryReads++
			case "search_memory":
				sample.Searches++
			case "update_memory":
				sample.Updates++
			}
			if call.Name == "read_memory" || call.Name == "search_memory" {
				sample.Reads = append(sample.Reads, describeCall(call))
			}
		},
		OnToolResult: func(result tools.Result) {
			switch result.Tool {
			case "read_memory":
				sample.BytesRead += len(result.Output["messages"])
			case "search_memory":
				sample.BytesRead += len(result.Output["hits"])
			}
		},
		OnToolError: func(runtime.ToolCall, error) { sample.ToolErrors++ },
	}
	var start runtime.Session
	if err := store.ReadJSON(checkpoint, &start); err != nil {
		sample.Error = err.Error()
		return sample
	}
	question := []runtime.Message{{Role: "user", Content: fmt.Sprintf(questionPrompt, probe.Question)}}
	result, err := agentRuntime.Resume(ctx, process.PID, checkpoint, question, options)
	sample.Steps, sample.BackendErrors = backend.calls, backend.errors
	// Usage carries over from the checkpoint; a failed run returns none.
	sample.Tokens = max(0, result.Session.Usage.Tokens-start.Usage.Tokens)
	sample.AgentIndexSegments, sample.AgentIndexBytes = backend.header["agent_index_segments"], backend.header["agent_index_bytes"]
	sample.AgentIndexShown, sample.AgentIndexHidden = headerLines(backend.header, "agent_index_shown."), headerLines(backend.header, "agent_index_hidden.")
	sample.AgentIndexBudget = backend.header["agent_index_budget"]
	sample.AgentIndexPartialNote, sample.AgentIndexShortenedNote = backend.header["agent_index_partial_note"] == 1, backend.header["agent_index_shortened_note"] == 1
	if err != nil && !errors.Is(err, runtime.ErrAgentBlocked) {
		sample.Error = err.Error()
	}
	if sample.Answer == "" && result.ExitStatus.Reason != "" {
		sample.Answer = result.ExitStatus.Reason
	}
	score(&sample, probe)
	return sample
}

// Handoff is one handoff run on a checkpoint: the agent that wrote it is
// asked, as at a compaction, to update its working memory and, with a1
// on, to index what is archived.
type Handoff struct {
	// Checkpoint is the input; it is only read.
	Checkpoint string `json:"checkpoint"`
	Sample     int    `json:"sample"`
	PID        string `json:"pid"`
	Condition  string `json:"condition"`
	// Result is the checkpoint the handoff parked at, and Segment the
	// memory_segment it archived, in the copy the run worked in.
	Result  string              `json:"result_checkpoint,omitempty"`
	Segment string              `json:"segment,omitempty"`
	FromSeq int                 `json:"from_seq,omitempty"`
	ToSeq   int                 `json:"to_seq,omitempty"`
	Index   []runtime.IndexLine `json:"agent_index,omitempty"`
	// WorkingMemory is the working_memory version the handoff left.
	WorkingMemory string   `json:"working_memory,omitempty"`
	Steps         int      `json:"steps"`
	Tokens        int      `json:"tokens,omitempty"`
	BackendErrors int      `json:"backend_errors,omitempty"`
	Tools         []string `json:"tools,omitempty"`
	ToolErrors    int      `json:"tool_errors"`
	Error         string   `json:"error,omitempty"`
}

// RunHandoffs runs a handoff on each checkpoint cfg.Samples times, each in
// a fresh process resumed from the checkpoint's owner with only the memory
// tools. The results live in the copy under cfg.WorkDir, which a later Run
// can take as its StateDir; the run's own state is never written.
// Probe-given and stripped working memory do not apply: the agent hands
// off from the checkpoint as it is.
func RunHandoffs(ctx context.Context, cfg Config, checkpoints []string) ([]Handoff, error) {
	if cfg.Samples <= 0 {
		cfg.Samples = 1
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 12
	}
	if cfg.NoWorkingMemory {
		return nil, errors.New("a handoff runs on the checkpoint's own working memory; NoWorkingMemory does not apply")
	}
	if _, err := RecallPolicy(cfg.Recall); err != nil {
		return nil, err
	}
	k, store, err := openCopy(cfg)
	if err != nil {
		return nil, err
	}
	var handoffs []Handoff
	for _, checkpoint := range checkpoints {
		owner, agent, err := probeAgent(k, store, cfg.Workflow, Probe{Checkpoint: checkpoint})
		if err != nil {
			return nil, fmt.Errorf("checkpoint %s: %w", checkpoint, err)
		}
		loaded, err := cfg.Skills.LoadMany(agent.Skills)
		if err != nil {
			return nil, err
		}
		for n := 1; n <= cfg.Samples; n++ {
			if ctx.Err() != nil {
				return handoffs, ctx.Err()
			}
			handoff := handOff(ctx, cfg, k, store, owner, agent, loaded, checkpoint)
			handoff.Sample = n
			handoffs = append(handoffs, handoff)
		}
	}
	return handoffs, nil
}

// handOff runs one handoff and reads back what it archived.
func handOff(ctx context.Context, cfg Config, k *kernel.Kernel, store *artifact.Store, owner kernel.Process, agent agentflow.AgentSpec, loaded []skills.Document, checkpoint string) Handoff {
	handoff := Handoff{Checkpoint: checkpoint, Condition: "handoff" + cfg.recallCondition()}
	var before runtime.Session
	if err := store.ReadJSON(checkpoint, &before); err != nil {
		handoff.Error = err.Error()
		return handoff
	}
	process, err := spawnProbe(k, owner)
	if err != nil {
		handoff.Error = err.Error()
		return handoff
	}
	handoff.PID = process.PID
	backend, agentRuntime := probeRuntime(cfg, k, store, agent, loaded)
	// A fresh step allowance: the checkpoint's own steps would otherwise
	// count against MaxSteps.
	options := runtime.RunOptions{
		NewTurn:             true,
		HandoffOnly:         true,
		ContinueOnToolError: true,
		OnToolCall:          func(call runtime.ToolCall) { handoff.Tools = append(handoff.Tools, call.Name) },
		OnToolError:         func(runtime.ToolCall, error) { handoff.ToolErrors++ },
	}
	result, err := agentRuntime.Resume(ctx, process.PID, checkpoint, nil, options)
	handoff.Steps, handoff.BackendErrors = backend.calls, backend.errors
	handoff.Tokens = max(0, result.Session.Usage.Tokens-before.Usage.Tokens)
	if err != nil {
		handoff.Error = err.Error()
		return handoff
	}
	handoff.Result = result.CheckpointID
	if len(result.Session.MemoryRefs) <= len(before.MemoryRefs) {
		handoff.Error = "the handoff parked without archiving a segment"
		return handoff
	}
	ref := result.Session.MemoryRefs[len(result.Session.MemoryRefs)-1]
	handoff.Segment, handoff.FromSeq, handoff.ToSeq = ref.ArtifactID, ref.FromSeq, ref.ToSeq
	handoff.WorkingMemory = result.Session.WorkingMemory
	if handoff.Index, err = runtime.SegmentIndex(store, ref.ArtifactID); err != nil {
		handoff.Error = err.Error()
	}
	return handoff
}

// describeCall is a tool call as one line: its name and its arguments in
// key order.
func describeCall(call runtime.ToolCall) string {
	keys := slices.Sorted(maps.Keys(call.Args))
	parts := []string{call.Name}
	for _, key := range keys {
		parts = append(parts, key+"="+call.Args[key])
	}
	return strings.Join(parts, " ")
}

// Rescore scores recorded samples again against probes, without model
// calls, so a tightened pattern applies to answers already collected.
// Samples of probes not in the list keep their scores.
func Rescore(samples []Sample, probes []Probe) []Sample {
	byID := map[string]Probe{}
	for _, probe := range probes {
		byID[probe.ID] = probe
	}
	out := make([]Sample, 0, len(samples))
	for _, sample := range samples {
		if probe, ok := byID[sample.Probe]; ok {
			sample.Correct, sample.Superseded, sample.OffTopic = false, false, false
			score(&sample, probe)
		}
		out = append(out, sample)
	}
	return out
}

// ReadSamples reads samples written one JSON object per line.
func ReadSamples(path string) ([]Sample, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var samples []Sample
	decoder := json.NewDecoder(file)
	for decoder.More() {
		var sample Sample
		if err := decoder.Decode(&sample); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, nil
}

func score(sample *Sample, probe Probe) {
	if sample.Answer == "" {
		return
	}
	if probe.Topic != "" && !regexp.MustCompile(probe.Topic).MatchString(sample.Answer) {
		sample.OffTopic = true
		return
	}
	expect := regexp.MustCompile(probe.Expect).MatchString(sample.Answer)
	rejected := probe.Reject != "" && regexp.MustCompile(probe.Reject).MatchString(sample.Answer)
	sample.Correct = expect && !rejected
	sample.Superseded = rejected
}

// copyState copies what a probe changes, the kernel and the artifact
// index, into work. Payloads stay where they are: the copied index points
// at them, and they are write-once. Each file is replaced atomically by
// its writer, so copying from a live run gives each file whole.
func copyState(stateDir, work string) error {
	for _, rel := range []string{"artifacts/index.json", "kernel/processes.json", "kernel/events.json", "kernel/kernel_state.json"} {
		if err := copyFile(filepath.Join(stateDir, rel), filepath.Join(work, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(work, "artifacts/index.json")); err != nil {
		return fmt.Errorf("%s has no artifact index: %w", stateDir, err)
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Summary is one probe across its samples.
type Summary struct {
	Probe      string  `json:"probe"`
	Kind       string  `json:"kind"`
	Samples    int     `json:"samples"`
	Correct    int     `json:"correct"`
	Superseded int     `json:"superseded"`
	OffTopic   int     `json:"off_topic"`
	Errors     int     `json:"errors"`
	MeanSteps  float64 `json:"mean_steps"`
	MinSteps   int     `json:"min_steps"`
	MaxSteps   int     `json:"max_steps"`
	MeanReads  float64 `json:"mean_reads"`
	MeanSearch float64 `json:"mean_searches"`
	MeanBytes  float64 `json:"mean_bytes"`
}

// Summarize groups samples by probe, in the order probes first appear.
func Summarize(samples []Sample) []Summary {
	byProbe := map[string]*Summary{}
	var order []string
	for _, sample := range samples {
		summary, ok := byProbe[sample.Probe]
		if !ok {
			summary = &Summary{Probe: sample.Probe, Kind: sample.Kind, MinSteps: sample.Steps, MaxSteps: sample.Steps}
			byProbe[sample.Probe] = summary
			order = append(order, sample.Probe)
		}
		summary.Samples++
		if sample.Correct {
			summary.Correct++
		}
		if sample.Superseded {
			summary.Superseded++
		}
		if sample.OffTopic {
			summary.OffTopic++
		}
		if sample.Error != "" {
			summary.Errors++
		}
		summary.MeanSteps += float64(sample.Steps)
		summary.MeanReads += float64(sample.MemoryReads)
		summary.MeanSearch += float64(sample.Searches)
		summary.MeanBytes += float64(sample.BytesRead)
		summary.MinSteps = min(summary.MinSteps, sample.Steps)
		summary.MaxSteps = max(summary.MaxSteps, sample.Steps)
	}
	out := make([]Summary, 0, len(order))
	for _, id := range order {
		summary := byProbe[id]
		n := float64(summary.Samples)
		summary.MeanSteps /= n
		summary.MeanReads /= n
		summary.MeanSearch /= n
		summary.MeanBytes /= n
		out = append(out, *summary)
	}
	return out
}

// WriteTable prints summaries as an aligned text table.
func WriteTable(w io.Writer, summaries []Summary) {
	fmt.Fprintf(w, "%-24s %-10s %8s %10s %9s %6s %11s %8s %6s %10s\n", "probe", "kind", "correct", "superseded", "off-topic", "errors", "steps", "searches", "reads", "bytes")
	for _, s := range summaries {
		fmt.Fprintf(w, "%-24s %-10s %4d/%-3d %10d %9d %6d %5.1f %2d-%-2d %8.1f %6.1f %10.0f\n",
			s.Probe, s.Kind, s.Correct, s.Samples, s.Superseded, s.OffTopic, s.Errors, s.MeanSteps, s.MinSteps, s.MaxSteps, s.MeanSearch, s.MeanReads, s.MeanBytes)
	}
	fmt.Fprintln(w, strings.Repeat("-", 113))
}
