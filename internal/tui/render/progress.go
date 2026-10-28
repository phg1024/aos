package render

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"aos/internal/agentflow"
)

// WorkflowProgress tracks the live delegation stack of a workflow from engine
// trace events and renders it as a status line and transcript entries.
type WorkflowProgress struct {
	ui       Palette
	workflow string
	maxSteps map[string]int
	now      func() time.Time

	mu    sync.Mutex
	stack []progressFrame
	// completed counts finished delegated (non-entry) agent runs.
	completed int
}

type progressFrame struct {
	agentID  string
	pid      string
	steps    int
	started  time.Time
	activity string
}

func NewWorkflowProgress(ui Palette, spec agentflow.WorkflowSpec) *WorkflowProgress {
	maxSteps := make(map[string]int, len(spec.Agents))
	for _, agent := range spec.Agents {
		maxSteps[agent.ID] = agent.MaxSteps
	}
	return &WorkflowProgress{ui: ui, workflow: spec.Name, maxSteps: maxSteps, now: time.Now}
}

// Observe applies one trace event to the delegation stack.
func (p *WorkflowProgress) Observe(event agentflow.TraceEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch event.Kind {
	case "start":
		if p.indexLocked(event.PID) >= 0 {
			return
		}
		started := event.Time
		if started.IsZero() {
			started = p.now()
		}
		p.stack = append(p.stack, progressFrame{agentID: event.AgentID, pid: event.PID, started: started, activity: "starting"})
	case "exit", "runtime_error", "cancelled":
		if idx := p.indexLocked(event.PID); idx >= 0 {
			p.stack = p.stack[:idx]
			// Only delegated runs count; the entry agent ends every turn.
			if idx > 0 {
				p.completed++
			}
		}
	case "tool_call":
		p.stepLocked(event.PID, "tool "+firstField(event.Detail))
	case "delegate":
		p.stepLocked(event.PID, "waiting on "+strings.TrimPrefix(firstField(event.Detail), "to="))
	case "message":
		p.stepLocked(event.PID, "replied")
	case "tool_result", "tool_error", "delegate_result":
		p.setActivityLocked(event.PID, "thinking")
	}
}

// Active reports whether any agent is still running.
func (p *WorkflowProgress) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stack) > 0
}

// Depth is the number of running agents above the one with pid.
func (p *WorkflowProgress) Depth(pid string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx := p.indexLocked(pid); idx >= 0 {
		return idx
	}
	return len(p.stack)
}

// StatusLine renders the stack as "coordinator 3/400 › writer 49/200 · 14m02s · tool run".
func (p *WorkflowProgress) StatusLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var builder strings.Builder
	builder.WriteString(" workflow " + p.workflow)
	if len(p.stack) == 0 {
		fmt.Fprintf(&builder, " | idle | finished=%d ", p.completed)
		return builder.String()
	}
	builder.WriteString(" ▸ ")
	for idx, frame := range p.stack {
		if idx > 0 {
			builder.WriteString(" › ")
		}
		builder.WriteString(frame.agentID + " " + p.stepLabelLocked(frame))
	}
	top := p.stack[len(p.stack)-1]
	fmt.Fprintf(&builder, " | %s | %s | finished=%d ", formatElapsed(p.now().Sub(top.started)), top.activity, p.completed)
	return builder.String()
}

// Apply observes event and returns its transcript line. A start is observed
// first so the new agent is indented at its own depth; everything else is
// rendered before an exit pops the agent.
func (p *WorkflowProgress) Apply(event agentflow.TraceEvent) string {
	if event.Kind == "start" {
		p.Observe(event)
		return p.Render(event)
	}
	line := p.Render(event)
	p.Observe(event)
	return line
}

// Render returns the transcript line for an event from a delegated agent,
// or "" for events that only update the status line.
func (p *WorkflowProgress) Render(event agentflow.TraceEvent) string {
	p.mu.Lock()
	depth := p.indexLocked(event.PID)
	if depth < 0 {
		depth = len(p.stack)
	}
	p.mu.Unlock()

	indent := strings.Repeat("  ", depth)
	agent := event.AgentID
	switch event.Kind {
	case "start":
		return p.ui.Header(indent+"┌ "+agent+" "+event.PID) + p.ui.Subtle(" goal: "+clipLine(event.Detail, 100))
	case "tool_call":
		return indent + "│ " + p.ui.ToolCall(agent+" › "+event.Detail)
	case "tool_result":
		return indent + "│ " + p.ui.ToolResult("  ↳ "+clipLine(event.Detail, 110))
	case "tool_error":
		return indent + "│ " + p.ui.ToolError("  ✗ "+clipLine(event.Detail, 110))
	case "message":
		return indent + "│ " + p.ui.AgentBlock(agent+"> ", clipLine(event.Detail, 160))
	case "exit":
		// Engine exit details start with "code=N"; anything but 0 is a failure.
		if !strings.HasPrefix(event.Detail, "code=0 ") {
			return indent + p.ui.Error("└ "+agent+" failed") + p.ui.Subtle(" "+clipLine(event.Detail, 120))
		}
		return p.ui.Header(indent+"└ "+agent+" exited") + p.ui.Subtle(" "+clipLine(event.Detail, 120))
	case "runtime_error":
		return indent + p.ui.Error("└ "+agent+" failed: "+event.Detail)
	case "cancelled":
		return indent + p.ui.Warn("└ "+agent+" cancelled")
	}
	return ""
}

func (p *WorkflowProgress) stepLabelLocked(frame progressFrame) string {
	if limit := p.maxSteps[frame.agentID]; limit > 0 {
		return fmt.Sprintf("%d/%d", frame.steps, limit)
	}
	return fmt.Sprintf("%d", frame.steps)
}

func (p *WorkflowProgress) stepLocked(pid, activity string) {
	if idx := p.indexLocked(pid); idx >= 0 {
		p.stack[idx].steps++
		p.stack[idx].activity = activity
	}
}

func (p *WorkflowProgress) setActivityLocked(pid, activity string) {
	if idx := p.indexLocked(pid); idx >= 0 {
		p.stack[idx].activity = activity
	}
}

func (p *WorkflowProgress) indexLocked(pid string) int {
	for idx := len(p.stack) - 1; idx >= 0; idx-- {
		if p.stack[idx].pid == pid {
			return idx
		}
	}
	return -1
}

func firstField(value string) string {
	if fields := strings.Fields(value); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm%02ds", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// clipLine flattens value to one line of at most limit runes.
func clipLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
