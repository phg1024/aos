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
	"os"
	"path/filepath"
	"regexp"
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
	// memory), "live" (in the live history, so no memory read is needed)
	// or "superseded" (the archive holds an older answer that was later
	// overturned).
	Kind       string `json:"kind"`
	Checkpoint string `json:"checkpoint"`
	Question   string `json:"question"`
	// Expect matches a correct answer; Reject, if set, matches an answer
	// that states the superseded fact as current. Both are Go regexps.
	Expect string `json:"expect"`
	Reject string `json:"reject,omitempty"`
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
	Steps       int    `json:"steps"`
	MemoryReads int    `json:"memory_reads"`
	BytesRead   int    `json:"bytes_read"`
	ToolErrors  int    `json:"tool_errors"`
	Error       string `json:"error,omitempty"`
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
	work := cfg.WorkDir
	if work == "" {
		dir, err := os.MkdirTemp("", "aos-memprobe-*")
		if err != nil {
			return nil, err
		}
		work = dir
	}
	if err := copyState(cfg.StateDir, work); err != nil {
		return nil, err
	}
	k, err := kernel.New(filepath.Join(work, "kernel"))
	if err != nil {
		return nil, err
	}
	store, err := artifact.New(filepath.Join(work, "artifacts"))
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
		for n := 1; n <= cfg.Samples; n++ {
			if ctx.Err() != nil {
				return samples, ctx.Err()
			}
			sample := ask(ctx, cfg, k, store, owner, agent, loaded, probe)
			sample.Sample = n
			samples = append(samples, sample)
		}
	}
	return samples, nil
}

// probeAgent finds the process that wrote the probe's checkpoint and its
// agent spec, stripped of every capability: the system prompt then lists
// only read_memory, which is always allowed.
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

// countingBackend counts model calls, one per step.
type countingBackend struct {
	runtime.Backend
	calls int
}

func (b *countingBackend) Generate(ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	b.calls++
	return b.Backend.Generate(ctx, request)
}

// ask runs one sample: a fresh process resumed from the checkpoint as if
// recovered from its owner, so its own compacted memory is readable.
func ask(ctx context.Context, cfg Config, k *kernel.Kernel, store *artifact.Store, owner kernel.Process, agent agentflow.AgentSpec, loaded []skills.Document, probe Probe) Sample {
	sample := Sample{Probe: probe.ID, Kind: probe.Kind}
	metadata := map[string]string{"resumed_from": owner.PID, "memprobe": probe.ID}
	for key, value := range owner.Metadata {
		if _, set := metadata[key]; !set {
			metadata[key] = value
		}
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: owner.Role, Metadata: metadata})
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	sample.PID = process.PID
	backend := &countingBackend{Backend: agentflow.PromptBackend{Completer: cfg.Completer, Workflow: cfg.Workflow, Agent: agent, Skills: loaded}}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: cfg.MaxSteps, Compaction: cfg.Compaction}
	options := runtime.RunOptions{
		NewTurn:                true,
		StopOnAssistantMessage: true,
		ContinueOnToolError:    true,
		OnAssistantMessage:     func(text string) { sample.Answer = text },
		OnToolCall: func(call runtime.ToolCall) {
			if call.Name == "read_memory" {
				sample.MemoryReads++
			}
		},
		OnToolResult: func(result tools.Result) {
			if result.Tool == "read_memory" {
				sample.BytesRead += len(result.Output["messages"])
			}
		},
		OnToolError: func(runtime.ToolCall, error) { sample.ToolErrors++ },
	}
	question := []runtime.Message{{Role: "user", Content: fmt.Sprintf(questionPrompt, probe.Question)}}
	result, err := agentRuntime.Resume(ctx, process.PID, probe.Checkpoint, question, options)
	sample.Steps = backend.calls
	if err != nil && !errors.Is(err, runtime.ErrAgentBlocked) {
		sample.Error = err.Error()
	}
	if sample.Answer == "" && result.ExitStatus.Reason != "" {
		sample.Answer = result.ExitStatus.Reason
	}
	score(&sample, probe)
	return sample
}

func score(sample *Sample, probe Probe) {
	if sample.Answer == "" {
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
	Errors     int     `json:"errors"`
	MeanSteps  float64 `json:"mean_steps"`
	MinSteps   int     `json:"min_steps"`
	MaxSteps   int     `json:"max_steps"`
	MeanReads  float64 `json:"mean_reads"`
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
		if sample.Error != "" {
			summary.Errors++
		}
		summary.MeanSteps += float64(sample.Steps)
		summary.MeanReads += float64(sample.MemoryReads)
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
		summary.MeanBytes /= n
		out = append(out, *summary)
	}
	return out
}

// WriteTable prints summaries as an aligned text table.
func WriteTable(w io.Writer, summaries []Summary) {
	fmt.Fprintf(w, "%-24s %-10s %8s %10s %6s %11s %10s %10s\n", "probe", "kind", "correct", "superseded", "errors", "steps", "reads", "bytes")
	for _, s := range summaries {
		fmt.Fprintf(w, "%-24s %-10s %4d/%-3d %10d %6d %5.1f %2d-%-2d %10.1f %10.0f\n",
			s.Probe, s.Kind, s.Correct, s.Samples, s.Superseded, s.Errors, s.MeanSteps, s.MinSteps, s.MaxSteps, s.MeanReads, s.MeanBytes)
	}
	fmt.Fprintln(w, strings.Repeat("-", 96))
}
