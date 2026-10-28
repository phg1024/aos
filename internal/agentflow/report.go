package agentflow

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/kernel"
)

type RunReport struct {
	Title       string
	Summary     RunSummary
	Processes   []kernel.Process
	Artifacts   []artifact.Artifact
	Events      []kernel.Event
	GeneratedAt time.Time
	Decisions   []string
	Issues      []string
	Tests       []string
}

func CollectRunReport(k *kernel.Kernel, store *artifact.Store, title string, summary RunSummary) (RunReport, error) {
	processes, err := k.Processes()
	if err != nil {
		return RunReport{}, err
	}
	artifacts, err := store.List()
	if err != nil {
		return RunReport{}, err
	}
	events, err := k.Events()
	if err != nil {
		return RunReport{}, err
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].CreatedAt.Before(processes[j].CreatedAt) })
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].CreatedAt.Before(artifacts[j].CreatedAt) })
	sort.Slice(events, func(i, j int) bool { return events[i].Timestamp.Before(events[j].Timestamp) })
	return RunReport{
		Title:       title,
		Summary:     summary,
		Processes:   processes,
		Artifacts:   artifacts,
		Events:      events,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

func RenderHTML(report RunReport) string {
	var processRows strings.Builder
	for _, process := range report.Processes {
		processRows.WriteString("<tr>")
		processRows.WriteString(td(process.PID))
		role := process.Role
		if process.CapabilitySet.UnconfinedRun {
			role += " (UNCONFINED run)"
		}
		processRows.WriteString(td(role))
		processRows.WriteString(td(process.ParentPID))
		processRows.WriteString(td(string(process.State)))
		if process.ExitStatus != nil {
			processRows.WriteString(td(fmt.Sprintf("%d / %s", process.ExitStatus.Code, process.ExitStatus.Reason)))
		} else {
			processRows.WriteString(td(""))
		}
		processRows.WriteString("</tr>\n")
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

	var eventRows strings.Builder
	for _, event := range report.Events {
		eventRows.WriteString("<tr>")
		eventRows.WriteString(td(event.Timestamp.Format(time.RFC3339)))
		eventRows.WriteString(td(event.ProcessID))
		eventRows.WriteString(td(event.Type))
		eventRows.WriteString(td(formatMap(event.Data)))
		eventRows.WriteString("</tr>\n")
	}

	return "<!doctype html>\n<html lang=\"en\">\n<head>\n" +
		"  <meta charset=\"utf-8\">\n" +
		"  <title>" + html.EscapeString(report.Title) + "</title>\n" +
		"  <style>\n" +
		"    body { font-family: -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif; margin: 40px auto; max-width: 1040px; line-height: 1.5; color: #1f2937; }\n" +
		"    code { background: #f3f4f6; padding: 2px 5px; border-radius: 4px; }\n" +
		"    table { border-collapse: collapse; width: 100%; margin: 20px 0; }\n" +
		"    th, td { border: 1px solid #d1d5db; padding: 10px; text-align: left; vertical-align: top; }\n" +
		"    th { background: #f9fafb; }\n" +
		"    .hero { background: linear-gradient(135deg, #fff7ed, #eff6ff); padding: 24px; border-radius: 16px; }\n" +
		"    ul { padding-left: 20px; }\n" +
		"  </style>\n" +
		"</head>\n<body>\n" +
		"  <div class=\"hero\">\n" +
		"    <h1>" + html.EscapeString(report.Title) + "</h1>\n" +
		"    <p>Workflow: <code>" + html.EscapeString(report.Summary.Workflow) + "</code></p>\n" +
		"    <p>Generated: <code>" + report.GeneratedAt.Format(time.RFC3339) + "</code></p>\n" +
		"    <p>Root PID: <code>" + html.EscapeString(report.Summary.RootPID) + "</code> | Entry PID: <code>" + html.EscapeString(report.Summary.EntryPID) + "</code></p>\n" +
		"    <p>Workflow summary artifact: <code>" + html.EscapeString(report.Summary.ResultID) + "</code></p>\n" +
		"  </div>\n" +
		renderListSection("Decisions", report.Decisions) +
		renderListSection("Issues And Resolutions", report.Issues) +
		renderListSection("Tests", report.Tests) +
		"  <h2>Processes</h2>\n" +
		"  <table>\n    <tr><th>PID</th><th>Role</th><th>Parent</th><th>State</th><th>Exit</th></tr>\n" + processRows.String() + "  </table>\n" +
		"  <h2>Artifacts</h2>\n" +
		"  <table>\n    <tr><th>ID</th><th>Type</th><th>Producer PID</th><th>Parents</th></tr>\n" + artifactRows.String() + "  </table>\n" +
		"  <h2>Event Trace</h2>\n" +
		"  <table>\n    <tr><th>Time</th><th>PID</th><th>Event</th><th>Data</th></tr>\n" + eventRows.String() + "  </table>\n" +
		"</body>\n</html>\n"
}

func renderListSection(title string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("  <h2>")
	builder.WriteString(html.EscapeString(title))
	builder.WriteString("</h2>\n  <ul>\n")
	for _, item := range items {
		builder.WriteString("    <li>")
		builder.WriteString(html.EscapeString(item))
		builder.WriteString("</li>\n")
	}
	builder.WriteString("  </ul>\n")
	return builder.String()
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
