package agentflow

import (
	"context"
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
	// VerdictUnverified means nobody judged the work on the evidence the workflow
	// declared: the verifier did not read it, it was never published, or the
	// verifier ended without a judgement. It says nothing about the work.
	VerdictUnverified = "unverified"
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
	// Evidence is each declared record as it stood when the verdict was reached.
	Evidence []EvidenceCheck `json:"evidence,omitempty"`
	// WorkAtVerify is the work's hash when VerifierPID exited, and WorkAtGate
	// its hash when the root exited; they must match for a pass to stand.
	// Both are absent from a record made before the verdict rules.
	WorkAtVerify *WorkHash `json:"work_at_verify,omitempty"`
	WorkAtGate   *WorkHash `json:"work_at_gate,omitempty"`
	// Checks are the gate's own checks, run once the verifier passed.
	Checks []CheckResult `json:"checks,omitempty"`
}

// judge derives the verdict for the workflow under root. Runs are ordered
// by the kernel's event sequence, which is total, rather than timestamps.
// The entry agent is left out of the freshness check: its run spans the
// whole workflow, so it always ends last.
//
// Under the verdict rules a pass must also hold three more ways, checked in
// this order: the work's hash when the root exits is its hash when the
// verifier exited (bindWork); the declared evidence was read (withEvidence);
// and every gate check exits as declared.
func (e *Engine) judge(ctx context.Context, spec WorkflowSpec, req RunRequest, policy workflow.VerificationPolicy, rootPID, entryPID string) (Verdict, error) {
	var hashes map[string]WorkHash
	if !e.NoVerdictRules {
		var err error
		if hashes, err = e.recordedWorkHashes(); err != nil {
			return Verdict{}, err
		}
	}
	verdict, err := e.judgeExits(spec, policy, rootPID, entryPID, hashes)
	if err != nil {
		return Verdict{}, err
	}
	if !e.NoVerdictRules {
		verdict = e.bindWork(spec, req, verdict, hashes)
	}
	if verdict, err = e.withEvidence(spec, verdict); err != nil {
		return Verdict{}, err
	}
	if !e.NoVerdictRules && verdict.Status == VerdictPass && len(spec.Verification.Checks) > 0 {
		verdict.Checks = e.runChecks(ctx, spec, req, rootPID)
		if failed, ok := failedCheck(verdict.Checks); ok {
			verdict.Status = VerdictFail
			if failed.Err != "" {
				verdict.Reason = fmt.Sprintf("gate check %q did not run: %s; verifier %s's pass does not stand without it", failed.Name, failed.Err, verdict.VerifierPID)
			} else {
				verdict.Reason = fmt.Sprintf("gate check %q exited %d, want %d; verifier %s's pass does not stand", failed.Name, failed.ExitCode, failed.Want, verdict.VerifierPID)
			}
		}
	}
	return verdict, nil
}

// bindWork holds a pass to the work the verifier saw: the hash recorded when
// the verifier exited against the work's hash now. Any change since, by any
// process or none, makes the pass stale; work that could not be hashed
// leaves it unverified.
func (e *Engine) bindWork(spec WorkflowSpec, req RunRequest, verdict Verdict, hashes map[string]WorkHash) Verdict {
	if verdict.VerifierPID == "" {
		return verdict
	}
	at, recorded := hashes[verdict.VerifierPID]
	if recorded {
		verdict.WorkAtVerify = &at
	}
	if verdict.Status != VerdictPass {
		return verdict
	}
	scopes, excluded := e.workScopes(spec, req)
	now := hashWork(scopes, excluded)
	verdict.WorkAtGate = &now
	switch {
	case !recorded:
		verdict.Status = VerdictUnverified
		verdict.Reason = fmt.Sprintf("verifier %s exited 0, and no hash of the work was recorded at its exit. This says nothing about the work: verification is what is missing", verdict.VerifierPID)
	case at.Err != "" || now.Err != "":
		verdict.Status = VerdictUnverified
		verdict.Reason = fmt.Sprintf("verifier %s exited 0, and the work could not be bound to its verdict: %s. This says nothing about the work: verification is what is missing", verdict.VerifierPID, strings.TrimSpace(at.Err+" "+now.Err))
	case at.SHA256 != now.SHA256:
		verdict.Status = VerdictStale
		verdict.Reason = fmt.Sprintf("verdict stale; the work changed after verifier %s passed it (hash %.12s at its exit, %.12s now)", verdict.VerifierPID, at.SHA256, now.SHA256)
	}
	return verdict
}

// judgeExits is the verdict from the kernel's record of exits alone.
//
// hashes, under the verdict rules, are the work's hashes at verifier exits:
// a verifier's failure stands only while the work is as it failed it.
func (e *Engine) judgeExits(spec WorkflowSpec, policy workflow.VerificationPolicy, rootPID, entryPID string, hashes map[string]WorkHash) (Verdict, error) {
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
		if failedOn, ok := hashes[run.pid]; ok && failedOn.Err == "" {
			if now, ok := hashes[latest.pid]; ok && now.Err == "" && now.SHA256 != failedOn.SHA256 {
				// The work changed since this verifier judged it.
				continue
			}
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
//
// Under the verdict rules (legacy false) two things change. A best_effort
// workflow whose verifier judged the work wrong fails too, where before it
// exited 0; best_effort still does not wait for a verdict that is missing,
// stale or unverified. And a failure that stands is not retryable: the
// verifier's own claim of retryable is an agent's word, and running the
// verifier again cannot clear the failure without a change to the work.
func gateExit(root kernel.ExitStatus, policy workflow.VerificationPolicy, verdict Verdict, legacy bool) kernel.ExitStatus {
	if !legacy {
		return gateExitV1(root, policy, verdict)
	}
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
	case VerdictUnverified:
		// Nobody judged the work, so running the verifier again is the remedy.
		gated.Retryable = true
	}
	return gated
}

func gateExitV1(root kernel.ExitStatus, policy workflow.VerificationPolicy, verdict Verdict) kernel.ExitStatus {
	if root.Code != 0 || verdict.Status == VerdictPass || policy == workflow.VerificationNone {
		return root
	}
	if policy == workflow.VerificationBestEffort && verdict.Status != VerdictFail {
		return root
	}
	gated := kernel.ExitStatus{Code: 1, Reason: "verification " + string(policy) + ": " + verdict.Reason}
	switch verdict.Status {
	case VerdictFail:
		if verdict.VerifierExit != nil && verdict.VerifierExit.Code != 0 {
			gated.Code = verdict.VerifierExit.Code
		}
	case VerdictStale, VerdictUnverified:
		// Verifying the work again is the remedy.
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
