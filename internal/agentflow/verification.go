package agentflow

import (
	"fmt"
	"strconv"
	"strings"

	"aos/internal/kernel"
	"aos/internal/workflow"
)

// Verdict statuses. Only pass satisfies a required policy.
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictMissing = "missing"
	VerdictStale   = "stale"
)

// Verdict is a workflow's verification outcome as the engine read it from
// the kernel: the verifier's latest recorded exit, and whether a
// work-changing agent ran after it. Agents cannot write it; the engine
// records it as an artifact produced by the workflow root.
type Verdict struct {
	Policy       workflow.VerificationPolicy `json:"policy"`
	Verifier     string                      `json:"verifier"`
	Status       string                      `json:"status"`
	Reason       string                      `json:"reason"`
	VerifierPID  string                      `json:"verifier_pid,omitempty"`
	VerifierExit *kernel.ExitStatus          `json:"verifier_exit,omitempty"`
	StaleAgent   string                      `json:"stale_agent,omitempty"`
	StalePID     string                      `json:"stale_pid,omitempty"`
}

// judge derives the verdict for the workflow under root. Runs are ordered
// by the kernel's event sequence, which is total, rather than timestamps.
// The entry agent is left out of the freshness check: its run spans the
// whole workflow, so it always ends last.
func (e *Engine) judge(spec WorkflowSpec, policy workflow.VerificationPolicy, rootPID, entryPID string) (Verdict, error) {
	verdict := Verdict{Policy: policy, Verifier: spec.Verification.Verifier}
	runs, err := e.descendantExits(rootPID)
	if err != nil {
		return Verdict{}, err
	}

	var latest *exitedRun
	for idx := range runs {
		if runs[idx].agentID == verdict.Verifier && (latest == nil || runs[idx].seq > latest.seq) {
			latest = &runs[idx]
		}
	}
	if latest == nil {
		verdict.Status = VerdictMissing
		verdict.Reason = "no verifier run"
		return verdict, nil
	}
	// The last change to the work: a work-changing delegated run. The entry
	// agent is left out because its run spans the whole workflow.
	var lastChange *exitedRun
	for idx := range runs {
		run := runs[idx]
		if run.pid == entryPID || run.agentID == verdict.Verifier {
			continue
		}
		if agent, ok := spec.Agent(run.agentID); ok && agent.IsWorkChanging() && (lastChange == nil || run.seq > lastChange.seq) {
			lastChange = &runs[idx]
		}
	}
	if lastChange != nil && lastChange.seq > latest.seq {
		status, err := e.Kernel.Await(latest.pid)
		if err != nil {
			return Verdict{}, err
		}
		verdict.VerifierPID, verdict.VerifierExit = latest.pid, &status
		verdict.Status = VerdictStale
		verdict.StaleAgent, verdict.StalePID = lastChange.agentID, lastChange.pid
		verdict.Reason = fmt.Sprintf("verdict stale; %s (%s) ran after verifier %s", lastChange.agentID, lastChange.pid, latest.pid)
		return verdict, nil
	}

	// A verifier's own judgement (a failure from its own exit action, whatever
	// Retryable it claims) stands until the work changes, so re-running the
	// verifier cannot shop for a pass. A failure the runtime recorded (step
	// limit, crash) is operational and a later run may clear it.
	var judged *exitedRun
	var judgedStatus kernel.ExitStatus
	for idx := range runs {
		run := runs[idx]
		if run.agentID != verdict.Verifier || (lastChange != nil && run.seq < lastChange.seq) {
			continue
		}
		status, err := e.Kernel.Await(run.pid)
		if err != nil {
			return Verdict{}, err
		}
		if status.Code != 0 && status.Origin == kernel.ExitOriginAgent && (judged == nil || run.seq > judged.seq) {
			judged, judgedStatus = &runs[idx], status
		}
	}
	if judged != nil {
		verdict.VerifierPID, verdict.VerifierExit = judged.pid, &judgedStatus
		verdict.Status = VerdictFail
		verdict.Reason = fmt.Sprintf("verifier %s failed: %s", judged.pid, judgedStatus.Reason)
		if judged.pid != latest.pid {
			verdict.Reason += fmt.Sprintf("; later verifier %s does not clear it without a change to the work", latest.pid)
		}
		return verdict, nil
	}

	status, err := e.Kernel.Await(latest.pid)
	if err != nil {
		return Verdict{}, err
	}
	verdict.VerifierPID, verdict.VerifierExit = latest.pid, &status
	if status.Code == 0 {
		verdict.Status = VerdictPass
		verdict.Reason = fmt.Sprintf("verifier %s passed: %s", latest.pid, status.Reason)
	} else {
		verdict.Status = VerdictFail
		verdict.Reason = fmt.Sprintf("verifier %s failed: %s", latest.pid, status.Reason)
	}
	return verdict, nil
}

// gateExit applies a required policy to the root's exit: an entry agent
// that succeeded still fails the workflow without a fresh passing verdict.
func gateExit(root kernel.ExitStatus, policy workflow.VerificationPolicy, verdict Verdict) kernel.ExitStatus {
	if policy != workflow.VerificationRequired || root.Code != 0 || verdict.Status == VerdictPass {
		return root
	}
	gated := kernel.ExitStatus{Code: 1, Reason: "verification required: " + verdict.Reason}
	switch verdict.Status {
	case VerdictFail:
		gated.Code = verdict.VerifierExit.Code
		gated.Retryable = verdict.VerifierExit.Retryable
	case VerdictStale:
		// Running the verifier again may well pass.
		gated.Retryable = true
	}
	return gated
}

type exitedRun struct {
	pid     string
	agentID string
	seq     int
}

// descendantExits lists every exited process under root with the sequence
// number of its exit event.
func (e *Engine) descendantExits(rootPID string) ([]exitedRun, error) {
	processes, err := e.Kernel.Processes()
	if err != nil {
		return nil, err
	}
	events, err := e.Kernel.Events()
	if err != nil {
		return nil, err
	}
	exitSeq := map[string]int{}
	for _, event := range events {
		if event.Type != "process.exited" {
			continue
		}
		seq, err := strconv.Atoi(strings.TrimPrefix(event.ID, "evt-"))
		if err != nil {
			return nil, fmt.Errorf("unexpected event id %q: %w", event.ID, err)
		}
		exitSeq[event.ProcessID] = seq
	}
	children := map[string][]kernel.Process{}
	for _, process := range processes {
		children[process.ParentPID] = append(children[process.ParentPID], process)
	}
	var runs []exitedRun
	queue := []string{rootPID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			queue = append(queue, child.PID)
			if seq, exited := exitSeq[child.PID]; exited {
				runs = append(runs, exitedRun{pid: child.PID, agentID: child.Metadata["agent_id"], seq: seq})
			}
		}
	}
	return runs, nil
}
