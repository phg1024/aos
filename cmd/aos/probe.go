package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	positional := parseInterspersed(fs, args)
	if *rescore != "" && *probesPath != "" {
		if err := rescoreProbes(*rescore, *probesPath, *outPath, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(positional) != 1 || *workflowPath == "" || *probesPath == "" {
		fmt.Fprintln(os.Stderr, "usage: aos probe-memory --workflow W --probes P [--samples N] [--max-steps N] [--out F] <state-dir>\n       aos probe-memory --probes P --rescore samples.jsonl [--out F]")
		os.Exit(2)
	}
	llmConfig := inference.LoadBackendConfig(120 * time.Second)
	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{})
	err := runProbes(context.Background(), backend, llmConfig.CompactionPolicy(), positional[0], *workflowPath, *probesPath, *workDir, *outPath, *samples, *maxSteps, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runProbes(ctx context.Context, completer agentflow.JSONCompleter, compaction runtime.CompactionPolicy, stateDir, workflowPath, probesPath, workDir, outPath string, samples, maxSteps int, w io.Writer) error {
	specBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		return err
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return err
	}
	spec.SkillRoots = dedupeStrings(append(defaultSkillRoots(filepath.Dir(workflowPath)), resolveSkillRoots(spec.SkillRoots, filepath.Dir(workflowPath))...))
	probes, err := memprobe.LoadProbes(probesPath)
	if err != nil {
		return err
	}
	results, runErr := memprobe.Run(ctx, memprobe.Config{
		StateDir:   stateDir,
		WorkDir:    workDir,
		Workflow:   spec,
		Skills:     skills.NewRegistry(spec.SkillRoots),
		Completer:  completer,
		Compaction: compaction,
		Samples:    samples,
		MaxSteps:   maxSteps,
	}, probes)
	if outPath != "" && len(results) > 0 {
		if err := writeSamples(outPath, results); err != nil {
			return err
		}
	}
	memprobe.WriteTable(w, memprobe.Summarize(results))
	return runErr
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
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	for _, sample := range samples {
		if err := encoder.Encode(sample); err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}
