package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/tools"
)

type projectSpec struct {
	Name      string
	Workflow  string
	Goal      string
	Output    string
	Inputs    map[string]string
	Decisions []string
	Issues    []string
	Tests     []string
}

func main() {
	reportRoot := getenv("AOS_REPORT_ROOT", "/Users/claw/Code/aos/reports/live-mini-projects")
	llmConfig := inference.LoadBackendConfig(180 * time.Second)

	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{})

	if err := os.MkdirAll(reportRoot, 0o755); err != nil {
		fail(err)
	}

	projects := []projectSpec{
		{
			Name:     "docs-merge",
			Workflow: "/Users/claw/Code/aos/workflows/live-docs-merge.json",
			Goal:     "Assemble the provided notes into one compact coherent HTML report. Preserve the most important ideas and remove repetition.",
			Output:   "combined-report.html",
			Inputs: map[string]string{
				"phase-a.html": "<h1>Phase A</h1><p>AOS should be role-agnostic, durable, and centered on artifacts instead of transcript sharing.</p>",
				"phase-b.html": "<h1>Phase B</h1><p>The runtime should support repeated tool use and delegation until the task is complete.</p>",
			},
			Decisions: []string{
				"Used a coordinator plus two specialist agents so the control plane stayed role-agnostic while the workflow remained collaborative.",
			},
			Issues: []string{
				"Large merged payloads can stress the delegate-summary channel, so the prompts keep the deliverable compact.",
			},
			Tests: []string{
				"Live local-vLLM run with read_file and write_file tool usage.",
			},
		},
		{
			Name:     "fact-sheet",
			Workflow: "/Users/claw/Code/aos/workflows/live-fact-sheet.json",
			Goal:     "Read the source notes and produce one markdown fact sheet with only the concrete project facts that matter.",
			Output:   "fact-sheet.md",
			Inputs: map[string]string{
				"notes-1.txt": "Project AOS uses a durable process table, event log, mailbox IPC, and artifact store.",
				"notes-2.txt": "The local prototype uses Go for the runtime and a local vLLM endpoint for live experimentation.",
			},
			Decisions: []string{
				"Kept this workflow in markdown to show that AOS can coordinate file-producing tasks without assuming HTML-only outputs.",
			},
			Issues: []string{
				"Structured extraction is still model-sensitive, so the deliverable format is intentionally simple and inspectable.",
			},
			Tests: []string{
				"Live local-vLLM run with artifact-producing delegation plus output file creation.",
			},
		},
		{
			Name:     "reviewed-merge",
			Workflow: "/Users/claw/Code/aos/workflows/live-reviewed-merge.json",
			Goal:     "Combine the source fragments into a final HTML note, then get a review note from another agent before finishing.",
			Output:   "reviewed-report.html",
			Inputs: map[string]string{
				"fragment-1.html": "<section><h2>Kernel</h2><p>The kernel owns process state, events, and durable records.</p></section>",
				"fragment-2.html": "<section><h2>Workflow</h2><p>Agent meanings should live in workflow specs, not inside the control plane.</p></section>",
			},
			Decisions: []string{
				"Included a review agent to show that review is a workflow choice rather than a core AOS obligation.",
			},
			Issues: []string{
				"If the review agent is weak, the workflow can still finish because the coordinator decides how to use the review note.",
			},
			Tests: []string{
				"Live local-vLLM run with reviewer participation as a normal delegated agent.",
			},
		},
	}

	var summaries []map[string]any
	for _, project := range projects {
		summary, err := runProject(context.Background(), backend, reportRoot, project)
		if err != nil {
			fail(err)
		}
		summaries = append(summaries, summary)
	}

	bytes, err := json.MarshalIndent(summaries, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(bytes))
}

func runProject(ctx context.Context, backend agentflow.JSONCompleter, reportRoot string, project projectSpec) (map[string]any, error) {
	projectRoot := filepath.Join(reportRoot, project.Name)
	inputRoot := filepath.Join(projectRoot, "inputs")
	stateRoot := filepath.Join(projectRoot, "state")
	outputPath := filepath.Join(projectRoot, project.Output)
	reportPath := filepath.Join(projectRoot, "completion-report.html")
	summaryPath := filepath.Join(projectRoot, "summary.json")

	for _, dir := range []string{projectRoot, inputRoot, stateRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	var inputPaths []string
	for name, content := range project.Inputs {
		path := filepath.Join(inputRoot, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return nil, err
		}
		inputPaths = append(inputPaths, path)
	}

	specBytes, err := os.ReadFile(project.Workflow)
	if err != nil {
		return nil, err
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return nil, err
	}

	k, err := kernel.New(filepath.Join(stateRoot, "kernel"))
	if err != nil {
		return nil, err
	}
	store, err := artifact.New(filepath.Join(stateRoot, "artifacts"))
	if err != nil {
		return nil, err
	}
	engine := agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Tools:     tools.NewRuntime(),
		Factory:   agentflow.StaticBackendFactory{Completer: backend},
	}

	summary, err := engine.Run(ctx, spec, agentflow.RunRequest{
		Goal:       project.Goal,
		InputPaths: inputPaths,
		OutputPath: outputPath,
	})
	if err != nil {
		return nil, err
	}

	report, err := agentflow.CollectRunReport(k, store, "AOS Mini Project: "+project.Name, summary)
	if err != nil {
		return nil, err
	}
	report.Decisions = project.Decisions
	report.Issues = project.Issues
	report.Tests = project.Tests
	if err := os.WriteFile(reportPath, []byte(agentflow.RenderHTML(report)), 0o644); err != nil {
		return nil, err
	}

	out := map[string]any{
		"name":        project.Name,
		"workflow":    spec.Name,
		"goal":        project.Goal,
		"state_dir":   stateRoot,
		"inputs_dir":  inputRoot,
		"output_path": outputPath,
		"report_path": reportPath,
		"result_id":   summary.ResultID,
		"entry_pid":   summary.EntryPID,
		"agent_count": countRuns(summary.AgentRuns),
	}
	bytes, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(summaryPath, bytes, 0o644); err != nil {
		return nil, err
	}
	out["summary_path"] = summaryPath
	return out, nil
}

func countRuns(runs []agentflow.AgentRun) int {
	total := 0
	for _, run := range runs {
		total++
		total += countRuns(run.Children)
	}
	return total
}

func getenv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	seconds, err := time.ParseDuration(value + "s")
	if err == nil && seconds > 0 {
		return seconds
	}
	return fallback
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
