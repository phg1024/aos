package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/workflow"
)

type liveDemoReport struct {
	ParentPID   string
	Processes   []kernel.Process
	Artifacts   []artifact.Artifact
	Events      []kernel.Event
	GeneratedAt time.Time
	Goal        string
	OutputHTML  string
}

type jsonCompleter interface {
	CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error
}

type liveWorker struct {
	backend jsonCompleter
}

type liveVerifier struct {
	backend jsonCompleter
}

type staticPlanner struct {
	plan workflow.Plan
}

func (p staticPlanner) Plan(_ context.Context, _ string) (workflow.Plan, error) {
	return p.plan, nil
}

func (w liveWorker) Execute(ctx context.Context, contract workflow.TaskContract, attempt int) (workflow.WorkerOutput, error) {
	var out struct {
		Summary string `json:"summary"`
	}
	system := "Return only compact JSON: {\"summary\":\"...\"}."
	user := contract.Objective + "\nReturn a concise 2-3 sentence summary."
	if err := w.backend.CompleteJSON(ctx, system, user, &out); err != nil {
		return workflow.WorkerOutput{}, err
	}
	return workflow.WorkerOutput{
		Summary: out.Summary,
		Metadata: map[string]string{
			"attempt": fmt.Sprintf("%d", attempt),
			"mode":    "live-llm",
		},
	}, nil
}

func (v liveVerifier) Verify(ctx context.Context, contract workflow.TaskContract, output workflow.WorkerOutput) (workflow.Verification, error) {
	var out struct {
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	system := "Return only compact JSON: {\"approved\":true|false,\"reason\":\"...\"}."
	user := "Objective:\n" + contract.Objective + "\n\nOutput:\n" + output.Summary + "\n\nApprove only if the output addresses the objective."
	if err := v.backend.CompleteJSON(ctx, system, user, &out); err != nil {
		return workflow.Verification{}, err
	}
	return workflow.Verification{
		Approved: out.Approved,
		Reason:   out.Reason,
		Metadata: map[string]string{"mode": "live-llm"},
	}, nil
}

func main() {
	outputPath := getenv("AOS_DEMO_OUT", "/Users/claw/Code/aos/reports/demo-live-docs-assembly.html")
	llmConfig := inference.LoadBackendConfig(120 * time.Second)

	root, err := os.MkdirTemp("", "aos-demo-live-*")
	if err != nil {
		fail(err)
	}

	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		fail(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		fail(err)
	}
	parent, err := k.Spawn(kernel.ProcessSpec{Role: "scenario-docs-live"})
	if err != nil {
		fail(err)
	}

	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{})

	goal := "assemble a compact AOS guide from two source snippets"
	engine := workflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Planner: staticPlanner{plan: workflow.Plan{
			Goal: goal,
			Contracts: []workflow.TaskContract{
				{
					ID: "core-model",
					Objective: "Summarize this source snippet in 2-4 sentences:\n" +
						"\"AOS is a minimal agent operating system prototype built around deterministic runtime semantics, artifact-based collaboration, and replaceable inference backends. Agents should share artifacts and protocols, not context windows.\"",
				},
			},
		}},
		Worker:            liveWorker{backend: backend},
		Verifier:          liveVerifier{backend: backend},
		Verification:      workflow.VerificationRequired,
		MaxWorkerAttempts: 2,
	}

	summary, err := engine.Run(context.Background(), parent.PID, goal)
	if err != nil {
		fail(err)
	}

	processes, err := k.Processes()
	if err != nil {
		fail(err)
	}
	artifacts, err := store.List()
	if err != nil {
		fail(err)
	}
	events, err := k.Events()
	if err != nil {
		fail(err)
	}

	report := liveDemoReport{
		ParentPID:   parent.PID,
		Processes:   sortProcesses(processes),
		Artifacts:   sortArtifacts(artifacts),
		Events:      sortEvents(events),
		GeneratedAt: time.Now().UTC(),
		Goal:        goal,
		OutputHTML:  outputPath,
	}
	if err := os.WriteFile(outputPath, []byte(renderHTML(report, summary)), 0o644); err != nil {
		fail(err)
	}

	out, err := json.MarshalIndent(map[string]any{
		"scenario":       "live-docs-assembly",
		"output_html":    outputPath,
		"parent_pid":     parent.PID,
		"processes":      len(report.Processes),
		"artifacts":      len(report.Artifacts),
		"events":         len(report.Events),
		"plan_artifact":  summary.PlanArtifactID,
		"approved_tasks": approvedCount(summary),
	}, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(out))
}

func approvedCount(summary workflow.Summary) int {
	count := 0
	for _, result := range summary.Results {
		if result.Approved {
			count++
		}
	}
	return count
}

func sortProcesses(processes []kernel.Process) []kernel.Process {
	out := append([]kernel.Process(nil), processes...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func sortArtifacts(artifacts []artifact.Artifact) []artifact.Artifact {
	out := append([]artifact.Artifact(nil), artifacts...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func sortEvents(events []kernel.Event) []kernel.Event {
	out := append([]kernel.Event(nil), events...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

func renderHTML(report liveDemoReport, summary workflow.Summary) string {
	var resultRows strings.Builder
	for _, result := range summary.Results {
		resultRows.WriteString("<tr>")
		resultRows.WriteString(td(result.ContractID))
		resultRows.WriteString(td(fmt.Sprintf("%d", result.Attempts)))
		resultRows.WriteString(td(fmt.Sprintf("%t", result.Approved)))
		resultRows.WriteString(td(result.FinalVerification.Reason))
		resultRows.WriteString(td(result.WorkerPID))
		resultRows.WriteString(td(result.VerifierPID))
		resultRows.WriteString("</tr>\n")
	}

	var artifactRows strings.Builder
	for _, item := range report.Artifacts {
		artifactRows.WriteString("<tr>")
		artifactRows.WriteString(td(item.ID))
		artifactRows.WriteString(td(item.Type))
		artifactRows.WriteString(td(item.ProducerPID))
		artifactRows.WriteString(td(strings.Join(item.ParentIDs, ", ")))
		artifactRows.WriteString("</tr>\n")
	}

	var processRows strings.Builder
	for _, process := range report.Processes {
		processRows.WriteString("<tr>")
		processRows.WriteString(td(process.PID))
		processRows.WriteString(td(process.Role))
		processRows.WriteString(td(string(process.State)))
		if process.ExitStatus != nil {
			processRows.WriteString(td(fmt.Sprintf("%d / %s", process.ExitStatus.Code, process.ExitStatus.Reason)))
		} else {
			processRows.WriteString(td(""))
		}
		processRows.WriteString("</tr>\n")
	}

	return "<!doctype html>\n<html lang=\"en\">\n<head>\n" +
		"  <meta charset=\"utf-8\">\n" +
		"  <title>AOS Live Mini Project Demo</title>\n" +
		"  <style>\n" +
		"    body { font-family: -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif; margin: 40px auto; max-width: 980px; line-height: 1.5; color: #1f2937; }\n" +
		"    code { background: #f3f4f6; padding: 2px 5px; border-radius: 4px; }\n" +
		"    table { border-collapse: collapse; width: 100%; margin: 20px 0; }\n" +
		"    th, td { border: 1px solid #d1d5db; padding: 10px; text-align: left; vertical-align: top; }\n" +
		"    th { background: #f9fafb; }\n" +
		"    .hero { background: linear-gradient(135deg, #fff7ed, #eff6ff); padding: 24px; border-radius: 16px; }\n" +
		"  </style>\n" +
		"</head>\n<body>\n" +
		"  <div class=\"hero\">\n" +
		"    <h1>AOS Live Mini Project Demo</h1>\n" +
		"    <p>Goal: <code>" + html.EscapeString(report.Goal) + "</code></p>\n" +
		"    <p>Generated: <code>" + report.GeneratedAt.Format(time.RFC3339) + "</code></p>\n" +
		"    <p>Parent PID: <code>" + html.EscapeString(report.ParentPID) + "</code></p>\n" +
		"    <p>Plan artifact: <code>" + html.EscapeString(summary.PlanArtifactID) + "</code></p>\n" +
		"  </div>\n" +
		"  <h2>What This Demonstrates</h2>\n" +
		"  <p>This demo uses your configured local LLM backend for worker and verifier roles. It is a live harness run, not a mocked scenario.</p>\n" +
		"  <h2>Task Results</h2>\n" +
		"  <table>\n    <tr><th>Contract</th><th>Attempts</th><th>Approved</th><th>Verifier Reason</th><th>Worker PID</th><th>Verifier PID</th></tr>\n" + resultRows.String() + "  </table>\n" +
		"  <h2>Processes</h2>\n" +
		"  <table>\n    <tr><th>PID</th><th>Role</th><th>State</th><th>Exit</th></tr>\n" + processRows.String() + "  </table>\n" +
		"  <h2>Artifacts</h2>\n" +
		"  <table>\n    <tr><th>ID</th><th>Type</th><th>Producer PID</th><th>Parents</th></tr>\n" + artifactRows.String() + "  </table>\n" +
		"</body>\n</html>\n"
}

func td(value string) string {
	return "<td>" + html.EscapeString(value) + "</td>"
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
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
