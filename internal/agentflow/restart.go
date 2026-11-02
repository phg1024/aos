package agentflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

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
// Before a restart the intensity is checked: if the policy's max restarts
// already fall within its window, the run is returned with a final,
// non-retryable exit naming the intensity, for the delegator to handle;
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
	var restarts []time.Time
	for attempt := 1; ; attempt++ {
		if err != nil || run.Status == runtime.DelegateWaiting || run.ExitStatus.Code == 0 || !run.ExitStatus.Retryable || ctx.Err() != nil {
			return run, err
		}
		now := e.Kernel.Now()
		if policy.Exceeded(restarts, now) {
			return e.escalate(agent, run, policy, policy.Recent(restarts, now)), nil
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
		var restartedAt time.Time
		run, err = e.drive(spec, req, agent, previous, parentPID, func(agentRuntime *runtime.AgentRuntime, options runtime.RunOptions) (runtime.RunResult, error) {
			track := options.OnRecovered
			options.OnRecovered = func(successor kernel.Process) {
				track(successor)
				event, recordErr := e.Kernel.RecordEvent(successor.PID, RestartEvent, map[string]string{
					"agent_id":    agent.ID,
					"predecessor": predecessor.PID,
					"attempt":     strconv.Itoa(attempt),
					"checkpoint":  checkpoint,
					"backoff":     backoff.String(),
					"reason":      predecessor.ExitStatus.Reason,
				})
				restartedAt = e.Kernel.Now()
				if recordErr == nil {
					restartedAt = event.Timestamp
				}
				e.emitKernelTrace(agent.ID, successor.PID, "restart", fmt.Sprintf("attempt=%d of=%s checkpoint=%s backoff=%s reason=%s", attempt, predecessor.PID, checkpoint, backoff, predecessor.ExitStatus.Reason), event.ID)
			}
			return agentRuntime.Recover(ctx, predecessor.PID, "", nil, options)
		})
		if restartedAt.IsZero() {
			// Recover refused before a successor existed, e.g. another
			// process recovered it first: the exit stands.
			if err != nil && errors.Is(err, runtime.ErrNotRecoverable) {
				e.notRestarted(agent, predecessor, "not_recoverable", err.Error())
				return predecessor, nil
			}
			return run, err
		}
		restarts = append(restarts, restartedAt)
	}
}

// escalate returns run with a final exit naming the restart intensity, and
// records why.
func (e *Engine) escalate(agent AgentSpec, run AgentRun, policy supervisor.Policy, restarts int) AgentRun {
	reason := fmt.Sprintf("%s: %d restarts within %s (max %d); last exit: %s", RestartIntensityReason, restarts, policy.Within, policy.Max, run.ExitStatus.Reason)
	event, _ := e.Kernel.RecordEvent(run.PID, EscalatedEvent, map[string]string{
		"agent_id":    agent.ID,
		"restarts":    strconv.Itoa(restarts),
		"within":      policy.Within.String(),
		"max":         strconv.Itoa(policy.Max),
		"last_reason": run.ExitStatus.Reason,
	})
	e.emitKernelTrace(agent.ID, run.PID, "escalated", reason, event.ID)
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
