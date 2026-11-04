package agentflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/supervisor"
)

// Kernel events of the restart policy.
const (
	// RestartEvent is recorded on a successor before its first step: its
	// predecessor, the attempt, the checkpoint and the backoff waited.
	RestartEvent = "supervisor.restart"
	// EscalatedEvent is recorded on the last process when the intensity
	// is exceeded: the failure goes to the delegator, final.
	EscalatedEvent = "supervisor.escalated"
	// NotRestartedEvent is recorded when a retryable exit is not
	// restarted for another reason: the budget is spent, there is no
	// checkpoint, or Recover refused.
	NotRestartedEvent = "supervisor.not_restarted"
)

// RestartIntensityReason starts the reason of an escalated exit.
const RestartIntensityReason = "restart intensity exceeded"

// supervise applies agent's restart policy to run, the result of running
// it. While the run ended in a retryable exit, it is restarted: recovered
// through the runtime's exclusive Recover from the checkpoint its exit
// names, after the policy's backoff, as a successor that continues the
// same budget. A run that waits, succeeded, failed for good, or was
// cancelled with ctx is returned as it is, and so is every run of an
// agent without a policy.
//
// Before a restart the intensity is checked against the restarts on the
// record: the supervisor.restart events of the run's process and the
// processes it was recovered from, so the count holds across a wait and a
// resume, and across engines on the same state directory. If the policy's
// max restarts already fall within its window, or the chain has had its
// total, the run is returned with a final, non-retryable exit naming the
// intensity, for the delegator to handle;
// the engine never restarts the delegator itself. A run whose budget is
// already spent (steps, tokens or a strict time budget, its own or up the
// chain) is not restarted either; its exit is returned unchanged.
func (e *Engine) supervise(ctx context.Context, spec WorkflowSpec, req RunRequest, agent AgentSpec, parentPID string, run AgentRun, err error) (AgentRun, error) {
	if agent.Restart == nil {
		return run, err
	}
	policy, policyErr := agent.Restart.Effective()
	if policyErr != nil {
		return run, policyErr
	}
	for {
		if err != nil || run.Status == runtime.DelegateWaiting || run.ExitStatus.Code == 0 || !run.ExitStatus.Retryable || ctx.Err() != nil {
			return run, err
		}
		restarts, recordErr := e.recordedRestarts(run.PID)
		if recordErr != nil {
			return run, recordErr
		}
		attempt := len(restarts) + 1
		now := e.Kernel.Now()
		if policy.Exceeded(restarts, now) {
			return e.escalate(agent, run, policy, restarts, now), nil
		}
		checkpoint := run.ExitStatus.Checkpoint
		if checkpoint == "" {
			e.notRestarted(agent, run, "no_checkpoint", "the exit names no checkpoint to restart from")
			return run, nil
		}
		spent, spentErr := (&runtime.AgentRuntime{Kernel: e.Kernel, Artifacts: e.Artifacts, Clock: e.Clock}).SpentBudget(ctx, run.PID, checkpoint, maxSteps(agent))
		if spentErr != nil {
			e.notRestarted(agent, run, "error", spentErr.Error())
			return run, nil
		}
		if spent != "" {
			e.notRestarted(agent, run, "budget_spent", spent)
			return run, nil
		}
		backoff := policy.Backoff(policy.Recent(restarts, now) + 1)
		if err := e.backoff(ctx, backoff); err != nil {
			// Stopped while waiting: the exit stands.
			return run, nil
		}
		predecessor := run
		previous, getErr := e.Kernel.Get(predecessor.PID)
		if getErr != nil {
			return predecessor, getErr
		}
		restarted := false
		run, err = e.drive(spec, req, agent, previous, parentPID, func(agentRuntime *runtime.AgentRuntime, options runtime.RunOptions) (runtime.RunResult, error) {
			track := options.OnRecovered
			options.OnRecovered = func(successor kernel.Process) {
				track(successor)
				// The window counts recorded restarts. A kernel that cannot
				// record this event cannot record the successor's run either.
				event, _ := e.Kernel.RecordEvent(successor.PID, RestartEvent, map[string]string{
					"agent_id":    agent.ID,
					"predecessor": predecessor.PID,
					"attempt":     strconv.Itoa(attempt),
					"checkpoint":  checkpoint,
					"backoff":     backoff.String(),
					"reason":      predecessor.ExitStatus.Reason,
				})
				restarted = true
				e.emitKernelTrace(agent.ID, successor.PID, "restart", fmt.Sprintf("attempt=%d of=%s checkpoint=%s backoff=%s reason=%s", attempt, predecessor.PID, checkpoint, backoff, predecessor.ExitStatus.Reason), event.ID)
			}
			return agentRuntime.Recover(ctx, predecessor.PID, "", nil, options)
		})
		if !restarted {
			// Recover refused before a successor existed, e.g. another
			// process recovered it first: the exit stands.
			if err != nil && errors.Is(err, runtime.ErrNotRecoverable) {
				e.notRestarted(agent, predecessor, "not_recoverable", err.Error())
				return predecessor, nil
			}
			return run, err
		}
	}
}

// recordedRestarts returns when pid's process chain was restarted: the
// supervisor.restart events recorded on pid and on each process it was
// recovered from, oldest first.
func (e *Engine) recordedRestarts(pid string) ([]time.Time, error) {
	chain := map[string]bool{}
	for current, hops := pid, 0; current != "" && hops < 64; hops++ {
		chain[current] = true
		process, err := e.Kernel.Get(current)
		if err != nil {
			return nil, err
		}
		current = process.Metadata["resumed_from"]
	}
	events, err := e.Kernel.Events()
	if err != nil {
		return nil, err
	}
	var restarts []time.Time
	for _, event := range events {
		if event.Type == RestartEvent && chain[event.ProcessID] {
			restarts = append(restarts, event.Timestamp)
		}
	}
	return restarts, nil
}

// escalate returns run with a final exit naming the restart intensity, and
// records why: the window's max, or the chain's total.
func (e *Engine) escalate(agent AgentSpec, run AgentRun, policy supervisor.Policy, restarts []time.Time, now time.Time) AgentRun {
	recent := policy.Recent(restarts, now)
	bound := fmt.Sprintf("%d restarts within %s (max %d)", recent, policy.Within, policy.Max)
	if recent < policy.Max {
		bound = fmt.Sprintf("%d restarts in all (total %d)", len(restarts), policy.Total)
	}
	reason := fmt.Sprintf("%s: %s; last exit: %s", RestartIntensityReason, bound, run.ExitStatus.Reason)
	event, _ := e.Kernel.RecordEvent(run.PID, EscalatedEvent, map[string]string{
		"agent_id":    agent.ID,
		"restarts":    strconv.Itoa(recent),
		"within":      policy.Within.String(),
		"max":         strconv.Itoa(policy.Max),
		"in_all":      strconv.Itoa(len(restarts)),
		"total":       strconv.Itoa(policy.Total),
		"last_reason": run.ExitStatus.Reason,
	})
	e.emitKernelTrace(agent.ID, run.PID, "escalated", reason, event.ID)
	e.closeChain(run.PID, reason)
	run.ExitStatus = kernel.ExitStatus{Code: run.ExitStatus.Code, Reason: reason, Retryable: false, Checkpoint: run.ExitStatus.Checkpoint}
	return run
}

// notRestarted records a retryable exit the policy did not restart.
func (e *Engine) notRestarted(agent AgentSpec, run AgentRun, cause, detail string) {
	event, _ := e.Kernel.RecordEvent(run.PID, NotRestartedEvent, map[string]string{
		"agent_id": agent.ID,
		"cause":    cause,
		"detail":   detail,
	})
	e.emitKernelTrace(agent.ID, run.PID, "not_restarted", cause+": "+detail, event.ID)
	// An error reading the budget says nothing about the exit, which an
	// operator may still recover.
	if cause != "error" {
		e.closeChain(run.PID, cause+": "+detail)
	}
}

// closeChain ends the chain of a retryable exit the policy will not restart:
// the engine, as its supervisor, says no successor is coming, for this
// process's chain alone, whoever delegated to it. Its subscriptions go, and
// its unread mail stays in the mailbox. A process another engine recovered
// first is refused, since its chain goes on.
func (e *Engine) closeChain(pid, reason string) {
	if e.NoChainDelivery {
		return
	}
	if _, err := e.Kernel.Close(pid, reason); err != nil {
		return
	}
	if e.Bus != nil {
		_ = e.Bus.Unsubscribe(pid, ipc.CauseChainEnded)
	}
}

// backoff waits d, or until ctx is done. Engine.Sleep, when set, does the
// waiting, as it does for backend retries.
func (e *Engine) backoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	if e.Sleep != nil {
		e.Sleep(ctx, d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
