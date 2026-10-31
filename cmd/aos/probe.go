package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aos/internal/agentflow"
	"aos/internal/inference"
	"aos/internal/memprobe"
	"aos/internal/runtime"
	"aos/internal/skills"
)

// probeMemory asks agents of a finished or running run questions whose
// answers lie in their own history, and scores what they recover.
func probeMemory(args []string) {
	fs := flag.NewFlagSet("probe-memory", flag.ExitOnError)
	workflowPath := fs.String("workflow", "", "workflow spec the run used")
	probesPath := fs.String("probes", "", "JSON array of probes (id, kind, checkpoint, question, expect, reject)")
	samples := fs.Int("samples", 3, "answers per probe")
	maxSteps := fs.Int("max-steps", 12, "steps one answer may take")
	outPath := fs.String("out", "", "write every sample as a JSON line here")
	workDir := fs.String("work-dir", "", "directory for the copy probes run in (default: a temporary directory)")
	rescore := fs.String("rescore", "", "score the samples in this JSON-lines file again with --probes, without model calls")
	noMemory := fs.Bool("no-working-memory", false, "leave out all working memory: what probes give the agent and what its checkpoint holds")
	noCheckRule := fs.Bool("no-check-rule", false, "leave out the prompt's rule to check working memory before deciding")
	recall := fs.String("recall", "", "comma-separated recall aids to turn on (w1, w2, w4, a1, a1p); every other aid is off")
	retry := fs.Int("backend-retry", -1, "attempts per failed model call within a step; -1 turns retrying off, 0 takes the runtime's defaults")
	agentIndexes := fs.String("agent-indexes", "", "JSON object of agent-written indexes by memory_segment ID, shown with those segments (with --recall a1)")
	headerIndexBytes := fs.Int("header-index-bytes", 0, "the header's total for agent indexes with --recall a1; 0 takes the runtime's default")
	handoff := fs.String("handoff", "", "comma-separated checkpoints to run a handoff on, instead of asking probes; results go to --out")
	positional := parseInterspersed(fs, args)
	if *rescore != "" && *probesPath != "" {
		if err := rescoreProbes(*rescore, *probesPath, *outPath, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// A handoff's segments and indexes stay in the copy it worked in, so
	// that copy must be one that is kept.
	if len(positional) != 1 || *workflowPath == "" || (*probesPath == "") == (*handoff == "") || (*handoff != "" && *workDir == "") {
		fmt.Fprintln(os.Stderr, "usage: aos probe-memory --workflow W --probes P [--samples N] [--max-steps N] [--recall AIDS] [--backend-retry N] [--out F] <state-dir>\n"+
			"       aos probe-memory --workflow W --handoff CHECKPOINTS --work-dir D [--samples N] [--recall AIDS] [--backend-retry N] [--out F] <state-dir>\n"+
			"       aos probe-memory --probes P --rescore samples.jsonl [--out F]")
		os.Exit(2)
	}
	llmConfig := inference.LoadBackendConfig(120 * time.Second)
	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{})
	conditions := memprobe.Config{NoWorkingMemory: *noMemory, NoCheckRule: *noCheckRule, Recall: splitList(*recall), HeaderIndexBytes: *headerIndexBytes}
	if *retry != -1 {
		conditions.BackendRetry = &runtime.BackendRetryPolicy{Attempts: *retry}
	}
	if *agentIndexes != "" {
		data, err := os.ReadFile(*agentIndexes)
		if err == nil {
			err = json.Unmarshal(data, &conditions.AgentIndexes)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	var err error
	if *handoff != "" {
		err = runHandoffs(context.Background(), backend, llmConfig.CompactionPolicy(), positional[0], *workflowPath, splitList(*handoff), *workDir, *outPath, *samples, *maxSteps, conditions, os.Stdout)
	} else {
		err = runProbes(context.Background(), backend, llmConfig.CompactionPolicy(), positional[0], *workflowPath, *probesPath, *workDir, *outPath, *samples, *maxSteps, conditions, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// probeConfig is the memprobe configuration for a run under conditions.
func probeConfig(completer agentflow.JSONCompleter, compaction runtime.CompactionPolicy, stateDir, workflowPath, workDir string, samples, maxSteps int, conditions memprobe.Config) (memprobe.Config, error) {
	specBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		return memprobe.Config{}, err
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return memprobe.Config{}, err
	}
	spec.SkillRoots = dedupeStrings(append(defaultSkillRoots(filepath.Dir(workflowPath)), resolveSkillRoots(spec.SkillRoots, filepath.Dir(workflowPath))...))
	return memprobe.Config{
		StateDir:   stateDir,
		WorkDir:    workDir,
		Workflow:   spec,
		Skills:     skills.NewRegistry(spec.SkillRoots),
		Completer:  completer,
		Compaction: compaction,
		Samples:    samples,
		MaxSteps:   maxSteps,

		NoWorkingMemory: conditions.NoWorkingMemory,
		NoCheckRule:     conditions.NoCheckRule,
		Recall:          conditions.Recall,
		BackendRetry:    conditions.BackendRetry,
		AgentIndexes:    conditions.AgentIndexes,

		HeaderIndexBytes: conditions.HeaderIndexBytes,
	}, nil
}

func runProbes(ctx context.Context, completer agentflow.JSONCompleter, compaction runtime.CompactionPolicy, stateDir, workflowPath, probesPath, workDir, outPath string, samples, maxSteps int, conditions memprobe.Config, w io.Writer) error {
	cfg, err := probeConfig(completer, compaction, stateDir, workflowPath, workDir, samples, maxSteps, conditions)
	if err != nil {
		return err
	}
	probes, err := memprobe.LoadProbes(probesPath)
	if err != nil {
		return err
	}
	results, runErr := memprobe.Run(ctx, cfg, probes)
	if outPath != "" && len(results) > 0 {
		if err := writeSamples(outPath, results); err != nil {
			return err
		}
	}
	memprobe.WriteTable(w, memprobe.Summarize(results))
	return runErr
}

// runHandoffs runs a handoff on each checkpoint and prints one line per
// handoff, writing every handoff to outPath as a JSON line if given. The
// copy in workDir keeps the segments and indexes they wrote.
func runHandoffs(ctx context.Context, completer agentflow.JSONCompleter, compaction runtime.CompactionPolicy, stateDir, workflowPath string, checkpoints []string, workDir, outPath string, samples, maxSteps int, conditions memprobe.Config, w io.Writer) error {
	cfg, err := probeConfig(completer, compaction, stateDir, workflowPath, workDir, samples, maxSteps, conditions)
	if err != nil {
		return err
	}
	handoffs, runErr := memprobe.RunHandoffs(ctx, cfg, checkpoints)
	if outPath != "" && len(handoffs) > 0 {
		if err := writeJSONLines(outPath, handoffs); err != nil {
			return err
		}
	}
	for _, h := range handoffs {
		fmt.Fprintf(w, "%s #%d %s: segment %s (%d-%d), %d index lines, %d steps", h.Checkpoint, h.Sample, h.Condition, h.Segment, h.FromSeq, h.ToSeq, len(h.Index), h.Steps)
		if h.Error != "" {
			fmt.Fprintf(w, ", error: %s", h.Error)
		}
		fmt.Fprintln(w)
	}
	return runErr
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// rescoreProbes scores recorded samples again with the probes' current
// patterns and prints the table, writing the rescored samples to outPath
// if given.
func rescoreProbes(samplesPath, probesPath, outPath string, w io.Writer) error {
	probes, err := memprobe.LoadProbes(probesPath)
	if err != nil {
		return err
	}
	samples, err := memprobe.ReadSamples(samplesPath)
	if err != nil {
		return err
	}
	samples = memprobe.Rescore(samples, probes)
	if outPath != "" {
		if err := writeSamples(outPath, samples); err != nil {
			return err
		}
	}
	memprobe.WriteTable(w, memprobe.Summarize(samples))
	return nil
}

func writeSamples(path string, samples []memprobe.Sample) error {
	return writeJSONLines(path, samples)
}

func writeJSONLines[T any](path string, items []T) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	for _, item := range items {
		if err := encoder.Encode(item); err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}
