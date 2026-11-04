// Package inspect reconstructs what a run did from its state directory
// alone: the kernel's processes and exit records, the artifact index, the
// durable trace and the model-call ledger. It makes no model calls, so it
// works on a run that was killed or crashed.
package inspect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/inference"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runlog"
	"aos/internal/runtime"
	"aos/internal/statedir"
)

// Report is everything inspect recovers about a run.
type Report struct {
	StateDir  string    `json:"state_dir"`
	Processes []Process `json:"processes"`
	NotExited []string  `json:"not_exited"`
	// RunActive says a run holds the state directory now. If none does, the
	// not-exited processes are orphans that aos reap can close.
	RunActive bool          `json:"run_active"`
	Leases    []lease.Lease `json:"leases"`
	// CutOff is the world tool calls that started (a tool.started intent)
	// and did not finish: still open, cancelled, or interrupted.
	CutOff []CutOffCall `json:"cut_off_calls,omitempty"`
	// Mail is the messages not yet acknowledged, by recipient.
	Mail         []Mailbox               `json:"mail,omitempty"`
	Verification *agentflow.Verdict      `json:"verification,omitempty"`
	VerdictID    string                  `json:"verification_id,omitempty"`
	Usage        []Usage                 `json:"usage"`
	TotalUsage   Usage                   `json:"total_usage"`
	TraceCount   int                     `json:"trace_count"`
	TraceTail    []agentflow.TraceRecord `json:"trace_tail"`
}

// Process is one kernel process in tree order.
type Process struct {
	PID              string             `json:"pid"`
	ParentPID        string             `json:"parent_pid,omitempty"`
	Depth            int                `json:"depth"`
	AgentID          string             `json:"agent_id,omitempty"`
	Role             string             `json:"role"`
	State            string             `json:"state"`
	Exit             *kernel.ExitStatus `json:"exit,omitempty"`
	SupersededBy     string             `json:"superseded_by,omitempty"`
	Closed           bool               `json:"closed,omitempty"`
	LatestCheckpoint string             `json:"latest_checkpoint,omitempty"`
	// UnconfinedRun marks a process whose run commands skip the sandbox.
	UnconfinedRun bool `json:"unconfined_run,omitempty"`
	// Budget and Usage are its limits and what it consumed by its latest
	// checkpoint; an exit caused by a budget names it in the reason.
	Budget capability.Budget `json:"budget,omitzero"`
	Usage  runtime.Usage     `json:"usage,omitzero"`
}

// CutOffCall is a world tool call that did not finish. Outcome is
// runtime.ToolCancelledType (stopped while the run was alive),
// runtime.ToolInterruptedType (the process died; its recoverer recorded
// this), or empty: no outcome yet, so the call was in flight when the
// process died, or still is.
type CutOffCall struct {
	runtime.ToolIntent
	Outcome   string `json:"outcome,omitempty"`
	OutcomeID string `json:"outcome_id,omitempty"`
	// RecordedBy is the process that recorded an interruption.
	RecordedBy string `json:"recorded_by,omitempty"`
}

// Mailbox counts one recipient's unacknowledged messages: Pending never
// handed out, Delivered handed out but not yet recorded by a checkpoint.
type Mailbox struct {
	PID       string `json:"pid"`
	Pending   int    `json:"pending"`
	Delivered int    `json:"delivered"`
}

// Usage totals model calls, per process or for the whole run.
type Usage struct {
	PID     string `json:"pid,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Calls   int    `json:"calls"`
	// InFlight counts calls started but never finished: in flight when the
	// process died.
	InFlight         int   `json:"in_flight,omitempty"`
	Repairs          int   `json:"repairs"`
	Errors           int   `json:"errors"`
	PromptTokens     int   `json:"prompt_tokens"`
	CachedTokens     int   `json:"cached_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	LatencyMS        int64 `json:"latency_ms"`
}

func (u *Usage) add(call inference.CallRecord) {
	if !call.Finished() {
		return
	}
	u.Calls++
	if call.Repair {
		u.Repairs++
	}
	if call.Error != "" {
		u.Errors++
	}
	u.PromptTokens += call.PromptTokens
	u.CachedTokens += call.CachedTokens
	u.CompletionTokens += call.CompletionTokens
	u.LatencyMS += call.LatencyMS
}

// Load reads stateDir and keeps the last tail trace records.
func Load(stateDir string, tail int) (Report, error) {
	report := Report{StateDir: stateDir}
	kernelDir := filepath.Join(stateDir, "kernel")
	if _, err := os.Stat(filepath.Join(kernelDir, "processes.json")); err != nil {
		return report, fmt.Errorf("%s is not an aos state directory: %w", stateDir, err)
	}
	k, err := kernel.New(kernelDir)
	if err != nil {
		return report, err
	}
	processes, err := k.Processes()
	if err != nil {
		return report, err
	}

	checkpoints, err := statedir.LatestCheckpoints(stateDir)
	if err != nil {
		return report, err
	}
	if store, err := openArtifacts(filepath.Join(stateDir, "artifacts")); err != nil {
		return report, err
	} else if store != nil {
		artifacts, err := store.List()
		if err != nil {
			return report, err
		}
		var verdictID string
		for _, record := range artifacts {
			if record.Type == "verification" && statedir.Newer(record.ID, verdictID) {
				verdictID = record.ID
			}
		}
		events, err := k.Events()
		if err != nil {
			return report, err
		}
		report.CutOff = cutOffCalls(events, artifacts)
		if verdictID != "" {
			var verdict agentflow.Verdict
			if err := store.ReadJSON(verdictID, &verdict); err != nil {
				return report, fmt.Errorf("read verification %s: %w", verdictID, err)
			}
			report.Verification, report.VerdictID = &verdict, verdictID
		}
	}
	report.Processes = processTree(processes, checkpoints)
	if store, err := openArtifacts(filepath.Join(stateDir, "artifacts")); err == nil && store != nil {
		if usage, err := agentflow.ProcessUsage(store); err == nil {
			for i := range report.Processes {
				report.Processes[i].Usage = usage[report.Processes[i].PID]
			}
		}
	}
	for _, process := range report.Processes {
		if process.State != string(kernel.StateExited) {
			report.NotExited = append(report.NotExited, process.PID)
		}
	}
	if report.RunActive, err = statedir.InUse(stateDir); err != nil {
		return report, err
	}
	if _, err := os.Stat(filepath.Join(stateDir, "bus", "mailboxes.json")); err == nil {
		bus, err := ipc.New(filepath.Join(stateDir, "bus"), nil)
		if err != nil {
			return report, err
		}
		pending, err := bus.Pending()
		if err != nil {
			return report, err
		}
		for pid, messages := range pending {
			box := Mailbox{PID: pid}
			for _, message := range messages {
				if message.Deliveries > 0 {
					box.Delivered++
				} else {
					box.Pending++
				}
			}
			report.Mail = append(report.Mail, box)
		}
		sort.Slice(report.Mail, func(i, j int) bool { return report.Mail[i].PID < report.Mail[j].PID })
	}
	if _, err := os.Stat(filepath.Join(stateDir, "leases", "leases.json")); err == nil {
		leases, err := lease.New(filepath.Join(stateDir, "leases"), k)
		if err != nil {
			return report, err
		}
		if report.Leases, err = leases.List(); err != nil {
			return report, err
		}
	}

	calls, err := runlog.Read[inference.CallRecord](runlog.Path(stateDir, runlog.CallsFile))
	if err != nil {
		return report, err
	}
	perPID := map[string]*Usage{}
	var order []string
	for _, call := range calls {
		usage, ok := perPID[call.PID]
		if !ok {
			usage = &Usage{PID: call.PID, AgentID: call.AgentID}
			perPID[call.PID] = usage
			order = append(order, call.PID)
		}
		usage.add(call)
		report.TotalUsage.add(call)
	}
	for _, open := range inference.InFlight(calls) {
		perPID[open.PID].InFlight++
		report.TotalUsage.InFlight++
	}
	for _, pid := range order {
		report.Usage = append(report.Usage, *perPID[pid])
	}

	trace, err := runlog.Read[agentflow.TraceRecord](runlog.Path(stateDir, runlog.TraceFile))
	if err != nil {
		return report, err
	}
	report.TraceCount = len(trace)
	if tail >= 0 && len(trace) > tail {
		trace = trace[len(trace)-tail:]
	}
	report.TraceTail = trace
	return report, nil
}

// cutOffCalls pairs each intent with its outcome and keeps those that did
// not finish.
func cutOffCalls(events []kernel.Event, artifacts []artifact.Artifact) []CutOffCall {
	outcomes := map[string]artifact.Artifact{}
	for _, record := range artifacts {
		if id := record.Metadata["intent"]; id != "" {
			outcomes[id] = record
		}
	}
	var out []CutOffCall
	for _, event := range events {
		if event.Type != runtime.ToolStartedEvent {
			continue
		}
		call := CutOffCall{ToolIntent: runtime.ToolIntent{EventID: event.ID, PID: event.ProcessID, Tool: event.Data["tool"], Target: event.Data["target"], Time: event.Timestamp}}
		outcome, ok := outcomes[event.ID]
		switch {
		case !ok:
		case outcome.Type == runtime.ToolCancelledType || outcome.Type == runtime.ToolInterruptedType:
			call.Outcome, call.OutcomeID = outcome.Type, outcome.ID
			if outcome.Type == runtime.ToolInterruptedType {
				call.RecordedBy = outcome.ProducerPID
			}
		default:
			continue
		}
		out = append(out, call)
	}
	return out
}

func openArtifacts(dir string) (*artifact.Store, error) {
	if _, err := os.Stat(filepath.Join(dir, "index.json")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return artifact.New(dir)
}

// processTree orders processes parent first, children by PID.
func processTree(processes []kernel.Process, checkpoints map[string]string) []Process {
	known := map[string]bool{}
	children := map[string][]kernel.Process{}
	for _, process := range processes {
		known[process.PID] = true
	}
	var roots []kernel.Process
	for _, process := range processes {
		if process.ParentPID == "" || !known[process.ParentPID] {
			roots = append(roots, process)
		} else {
			children[process.ParentPID] = append(children[process.ParentPID], process)
		}
	}
	byPID := func(list []kernel.Process) {
		sort.Slice(list, func(i, j int) bool { return list[i].PID < list[j].PID })
	}
	byPID(roots)
	var out []Process
	var walk func(process kernel.Process, depth int)
	walk = func(process kernel.Process, depth int) {
		out = append(out, Process{
			PID: process.PID, ParentPID: process.ParentPID, Depth: depth,
			AgentID: process.Metadata["agent_id"], Role: process.Role, State: string(process.State),
			Exit: process.ExitStatus, SupersededBy: process.SupersededBy, Closed: process.Closed, LatestCheckpoint: checkpoints[process.PID],
			UnconfinedRun: process.CapabilitySet.UnconfinedRun,
			Budget:        process.CapabilitySet.Budget,
		})
		list := children[process.PID]
		byPID(list)
		for _, child := range list {
			walk(child, depth+1)
		}
	}
	for _, root := range roots {
		walk(root, 0)
	}
	return out
}

// WriteJSON writes the report as indented JSON.
func (r Report) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}

// WriteText writes the report for a terminal.
func (r Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "state dir: %s\n\n", r.StateDir)
	fmt.Fprintf(w, "processes (%d, %d not exited):\n", len(r.Processes), len(r.NotExited))
	for _, process := range r.Processes {
		name := process.AgentID
		if name == "" {
			name = process.Role
		}
		line := fmt.Sprintf("%s%s %s  %s", strings.Repeat("  ", process.Depth+1), process.PID, name, process.State)
		if process.UnconfinedRun {
			line += "  [UNCONFINED run]"
		}
		if exit := process.Exit; exit != nil {
			line += fmt.Sprintf("  code=%d", exit.Code)
			if exit.Origin != "" {
				line += " origin=" + exit.Origin
			}
			if exit.Retryable {
				line += " retryable"
			}
			line += fmt.Sprintf(" %q", clip(exit.Reason, 90))
		}
		if process.SupersededBy != "" {
			line += "  superseded by " + process.SupersededBy
		}
		if process.Closed {
			line += "  closed"
		}
		if process.LatestCheckpoint != "" {
			line += "  checkpoint " + process.LatestCheckpoint
		}
		if budget := agentflow.FormatBudget(process.Budget, process.Usage); budget != "" {
			line += "  [" + budget + "]"
		}
		fmt.Fprintln(w, line)
	}
	if len(r.NotExited) > 0 {
		fmt.Fprintf(w, "\nnot exited (no exit record): %s\n", strings.Join(r.NotExited, ", "))
		if r.RunActive {
			fmt.Fprintln(w, "a run holds this state directory; these may still be running")
		} else {
			fmt.Fprintf(w, "no run holds this state directory: %d orphaned (run aos reap to close them so they can be recovered)\n", len(r.NotExited))
		}
	}
	if len(r.CutOff) > 0 {
		fmt.Fprintf(w, "\ntool calls started and not finished (%d); each may have taken effect:\n", len(r.CutOff))
		for _, call := range r.CutOff {
			outcome := "no outcome: in flight when the process died, or still running"
			switch call.Outcome {
			case runtime.ToolCancelledType:
				outcome = "cancelled (" + call.OutcomeID + ")"
			case runtime.ToolInterruptedType:
				outcome = fmt.Sprintf("interrupted; %s was told (%s)", call.RecordedBy, call.OutcomeID)
			}
			fmt.Fprintf(w, "  %s  %s  %s %q  %s\n", call.EventID, call.PID, call.Tool, clip(call.Target, 80), outcome)
		}
	}
	if len(r.Leases) > 0 {
		fmt.Fprintf(w, "\nleases held (%d):\n", len(r.Leases))
		for _, held := range r.Leases {
			fmt.Fprintf(w, "  %s  %s  token=%d  until %s\n", held.Resource, held.OwnerPID, held.Token, held.ExpiresAt.Local().Format("15:04:05"))
		}
	}
	if len(r.Mail) > 0 {
		fmt.Fprintln(w, "\nmail not yet acknowledged:")
		for _, box := range r.Mail {
			fmt.Fprintf(w, "  %s  %d pending, %d delivered but not checkpointed\n", box.PID, box.Pending, box.Delivered)
		}
	}
	if v := r.Verification; v != nil {
		fmt.Fprintf(w, "\nverification (%s, %s): %s  %s\n", v.Policy, r.VerdictID, v.Status, clip(v.Reason, 120))
	}
	fmt.Fprintf(w, "\nusage (%d model calls):\n", r.TotalUsage.Calls)
	if r.TotalUsage.InFlight > 0 {
		fmt.Fprintf(w, "  %d call(s) started and never finished: in flight when the run died\n", r.TotalUsage.InFlight)
	}
	for _, usage := range append(append([]Usage(nil), r.Usage...), withLabel(r.TotalUsage, "total")) {
		fmt.Fprintf(w, "  %-14s %-11s calls=%-4d prompt=%-8d cached=%-8d completion=%-7d latency=%-8s repairs=%d errors=%d\n",
			usage.AgentID, usage.PID, usage.Calls, usage.PromptTokens, usage.CachedTokens, usage.CompletionTokens,
			(time.Duration(usage.LatencyMS) * time.Millisecond).Round(100*time.Millisecond), usage.Repairs, usage.Errors)
	}
	fmt.Fprintf(w, "\ntrace (last %d of %d):\n", len(r.TraceTail), r.TraceCount)
	for _, event := range r.TraceTail {
		line := fmt.Sprintf("  #%-5d %s %s %s %s %s", event.Seq, event.Time.Local().Format("15:04:05"), event.AgentID, event.PID, event.Kind, clip(event.Detail, 100))
		if event.KernelEvent != "" {
			line += " [" + event.KernelEvent + "]"
		}
		fmt.Fprintln(w, line)
	}
}

func withLabel(usage Usage, label string) Usage {
	usage.AgentID = label
	return usage
}

func clip(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}
