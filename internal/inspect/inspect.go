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
	"aos/internal/inference"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runlog"
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
	LatestCheckpoint string             `json:"latest_checkpoint,omitempty"`
	// UnconfinedRun marks a process whose run commands skip the sandbox.
	UnconfinedRun bool `json:"unconfined_run,omitempty"`
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
	PID              string `json:"pid,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	Calls            int    `json:"calls"`
	Repairs          int    `json:"repairs"`
	Errors           int    `json:"errors"`
	PromptTokens     int    `json:"prompt_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	LatencyMS        int64  `json:"latency_ms"`
}

func (u *Usage) add(call inference.CallRecord) {
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
		if verdictID != "" {
			var verdict agentflow.Verdict
			if err := store.ReadJSON(verdictID, &verdict); err != nil {
				return report, fmt.Errorf("read verification %s: %w", verdictID, err)
			}
			report.Verification, report.VerdictID = &verdict, verdictID
		}
	}
	report.Processes = processTree(processes, checkpoints)
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
			Exit: process.ExitStatus, LatestCheckpoint: checkpoints[process.PID],
			UnconfinedRun: process.CapabilitySet.UnconfinedRun,
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
		if process.LatestCheckpoint != "" {
			line += "  checkpoint " + process.LatestCheckpoint
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
