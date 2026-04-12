package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/scenario"
)

type demoData struct {
	Scenario    string
	Root        string
	Report      scenario.Report
	Processes   []kernel.Process
	Artifacts   []artifact.Artifact
	Events      []kernel.Event
	GeneratedAt time.Time
	OutputHTML  string
}

func main() {
	var scenarioName string
	var outputPath string

	flag.StringVar(&scenarioName, "scenario", "docs", "demo scenario: docs, refactor, extraction")
	flag.StringVar(&outputPath, "out", "", "optional output HTML path")
	flag.Parse()

	root, err := os.MkdirTemp("", "aos-demo-*")
	if err != nil {
		fail(err)
	}

	runner, err := scenario.NewRunner(root)
	if err != nil {
		fail(err)
	}

	ctx := context.Background()
	var report scenario.Report
	switch scenarioName {
	case "docs":
		report, err = runner.RunDocsAssembly(ctx)
	case "refactor":
		report, err = runner.RunParallelRefactor(ctx)
	case "extraction":
		report, err = runner.RunDataExtraction(ctx)
	default:
		fail(errors.New("unknown scenario: " + scenarioName))
	}
	if err != nil {
		fail(err)
	}

	processes, err := runner.Kernel.Processes()
	if err != nil {
		fail(err)
	}
	artifacts, err := runner.Artifacts.List()
	if err != nil {
		fail(err)
	}
	events, err := runner.Kernel.Events()
	if err != nil {
		fail(err)
	}

	filteredProcesses := filterProcesses(processes, report)
	filteredArtifacts := sortArtifacts(artifacts)
	filteredEvents := sortEvents(filterEvents(events, report))

	if outputPath == "" {
		outputPath = filepath.Join("/Users/claw/Code/aos/reports", fmt.Sprintf("demo-%s.html", report.Name))
	}

	data := demoData{
		Scenario:    scenarioName,
		Root:        root,
		Report:      report,
		Processes:   filteredProcesses,
		Artifacts:   filteredArtifacts,
		Events:      filteredEvents,
		GeneratedAt: time.Now().UTC(),
		OutputHTML:  outputPath,
	}

	if err := os.WriteFile(outputPath, []byte(renderHTML(data)), 0o644); err != nil {
		fail(err)
	}

	summaryBytes, err := json.MarshalIndent(map[string]any{
		"scenario":    report.Name,
		"output_html": outputPath,
		"parent_pid":  report.ParentPID,
		"processes":   len(filteredProcesses),
		"artifacts":   len(filteredArtifacts),
		"events":      len(filteredEvents),
	}, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(summaryBytes))
}

func filterProcesses(processes []kernel.Process, report scenario.Report) []kernel.Process {
	processByID := make(map[string]kernel.Process, len(processes))
	for _, process := range processes {
		processByID[process.PID] = process
	}
	var out []kernel.Process
	for _, pid := range report.ProcessIDs {
		if process, ok := processByID[pid]; ok {
			out = append(out, process)
		}
	}
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

func filterEvents(events []kernel.Event, report scenario.Report) []kernel.Event {
	keep := make(map[string]bool, len(report.ProcessIDs))
	for _, pid := range report.ProcessIDs {
		keep[pid] = true
	}
	var out []kernel.Event
	for _, event := range events {
		if keep[event.ProcessID] {
			out = append(out, event)
		}
	}
	return out
}

func sortEvents(events []kernel.Event) []kernel.Event {
	out := append([]kernel.Event(nil), events...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

func renderHTML(data demoData) string {
	var processRows strings.Builder
	for _, process := range data.Processes {
		processRows.WriteString("<tr>")
		processRows.WriteString(td(process.PID))
		processRows.WriteString(td(process.Role))
		processRows.WriteString(td(string(process.State)))
		processRows.WriteString(td(process.ParentPID))
		if process.ExitStatus != nil {
			processRows.WriteString(td(fmt.Sprintf("%d / %s", process.ExitStatus.Code, process.ExitStatus.Reason)))
		} else {
			processRows.WriteString(td(""))
		}
		processRows.WriteString("</tr>\n")
	}

	var artifactRows strings.Builder
	for _, item := range data.Artifacts {
		artifactRows.WriteString("<tr>")
		artifactRows.WriteString(td(item.ID))
		artifactRows.WriteString(td(item.Type))
		artifactRows.WriteString(td(item.ProducerPID))
		artifactRows.WriteString(td(strings.Join(item.ParentIDs, ", ")))
		artifactRows.WriteString("</tr>\n")
	}

	var eventRows strings.Builder
	for _, event := range data.Events {
		eventRows.WriteString("<tr>")
		eventRows.WriteString(td(event.Timestamp.Format(time.RFC3339)))
		eventRows.WriteString(td(event.ProcessID))
		eventRows.WriteString(td(event.Type))
		eventRows.WriteString(td(formatMap(event.Data)))
		eventRows.WriteString("</tr>\n")
	}

	return "<!doctype html>\n<html lang=\"en\">\n<head>\n" +
		"  <meta charset=\"utf-8\">\n" +
		"  <title>AOS Mini Project Demo</title>\n" +
		"  <style>\n" +
		"    body { font-family: -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif; margin: 40px auto; max-width: 980px; line-height: 1.5; color: #1f2937; }\n" +
		"    code { background: #f3f4f6; padding: 2px 5px; border-radius: 4px; }\n" +
		"    table { border-collapse: collapse; width: 100%; margin: 20px 0; }\n" +
		"    th, td { border: 1px solid #d1d5db; padding: 10px; text-align: left; vertical-align: top; }\n" +
		"    th { background: #f9fafb; }\n" +
		"    .hero { background: linear-gradient(135deg, #f0fdf4, #eff6ff); padding: 24px; border-radius: 16px; }\n" +
		"  </style>\n" +
		"</head>\n<body>\n" +
		"  <div class=\"hero\">\n" +
		"    <h1>AOS Mini Project Demo</h1>\n" +
		"    <p>Scenario: <code>" + html.EscapeString(data.Report.Name) + "</code></p>\n" +
		"    <p>Generated: <code>" + data.GeneratedAt.Format(time.RFC3339) + "</code></p>\n" +
		"    <p>Parent PID: <code>" + html.EscapeString(data.Report.ParentPID) + "</code></p>\n" +
		"    <p>Processes: <code>" + fmt.Sprintf("%d", len(data.Processes)) + "</code>, Artifacts: <code>" + fmt.Sprintf("%d", len(data.Artifacts)) + "</code>, Events: <code>" + fmt.Sprintf("%d", len(data.Events)) + "</code></p>\n" +
		"  </div>\n" +
		"  <h2>What This Demonstrates</h2>\n" +
		"  <p>This mini project shows planner/worker/verifier collaboration, artifact handoff, and durable runtime traces. It is a concrete harness demo built on the AOS runtime rather than a unit test alone.</p>\n" +
		"  <h2>Processes</h2>\n" +
		"  <table>\n    <tr><th>PID</th><th>Role</th><th>State</th><th>Parent</th><th>Exit</th></tr>\n" + processRows.String() + "  </table>\n" +
		"  <h2>Artifacts</h2>\n" +
		"  <table>\n    <tr><th>ID</th><th>Type</th><th>Producer PID</th><th>Parents</th></tr>\n" + artifactRows.String() + "  </table>\n" +
		"  <h2>Event Trace</h2>\n" +
		"  <table>\n    <tr><th>Time</th><th>PID</th><th>Event</th><th>Data</th></tr>\n" + eventRows.String() + "  </table>\n" +
		"</body>\n</html>\n"
}

func td(value string) string {
	return "<td>" + html.EscapeString(value) + "</td>"
}

func formatMap(data map[string]string) string {
	if len(data) == 0 {
		return ""
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+data[key])
	}
	return strings.Join(parts, ", ")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
