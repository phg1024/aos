package agentflow

import (
	"fmt"
	"strings"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/runtime"
	"aos/internal/statedir"
)

// ProcessUsage reads each process's consumption from its latest checkpoint.
func ProcessUsage(store *artifact.Store) (map[string]runtime.Usage, error) {
	records, err := store.List()
	if err != nil {
		return nil, err
	}
	latest := map[string]string{}
	for _, record := range records {
		if record.Type == "checkpoint" && statedir.Newer(record.ID, latest[record.ProducerPID]) {
			latest[record.ProducerPID] = record.ID
		}
	}
	usage := map[string]runtime.Usage{}
	for pid, id := range latest {
		var session runtime.Session
		if err := store.ReadJSON(id, &session); err != nil {
			return nil, err
		}
		usage[pid] = session.Usage
	}
	return usage, nil
}

// FormatBudget describes consumption against a budget, e.g.
// "steps 7 · tokens 12034/50000 · time 95s/900s strict"; empty when there
// is nothing to show.
func FormatBudget(budget capability.Budget, usage runtime.Usage) string {
	var parts []string
	if usage.Steps > 0 {
		parts = append(parts, fmt.Sprintf("steps %d", usage.Steps))
	}
	if budget.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens %d/%d", usage.Tokens, budget.Tokens))
	} else if usage.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens %d", usage.Tokens))
	}
	if budget.Seconds > 0 {
		time := fmt.Sprintf("time %ds/%ds", int(usage.Seconds), budget.Seconds)
		if budget.StrictTime {
			time += " strict"
		}
		parts = append(parts, time)
	} else if usage.Seconds >= 1 {
		parts = append(parts, fmt.Sprintf("time %ds", int(usage.Seconds)))
	}
	return strings.Join(parts, " · ")
}
