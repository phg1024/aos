---
name: ralph-loop
description: Use when an agent should iterate in small logged loops toward a concrete target, checking benchmark or verification artifacts after each attempt and continuing until the goal is met or a blocker is clearly established.
---

# Ralph Loop

Use this skill when success is not one-shot and must be demonstrated by artifacts on disk rather than by self-report.

## Loop Pattern

Repeat this cycle:

1. Read the current goal and the latest relevant artifacts.
2. Review what has already been tried and what the latest measured or verified result was.
2. Pick one concrete next step.
3. Execute that step.
4. Append a short log entry describing what you tried and what happened.
5. Re-check the latest benchmark or verification evidence.
6. If the target is still unmet, continue with another step instead of exiting.

Do not start a fresh round blind. Always ground the next round in the latest evidence from prior rounds.

## Evidence Rules

- Treat benchmark JSON, verifier reports, compiler output, and generated logs as the source of truth.
- Do not claim the goal is met unless those artifacts support it.
- Do not stop after merely producing an output file.
- If the latest evidence shows failure or shortfall, keep iterating unless you have a concrete blocker.

## Logging

After each meaningful attempt, append a short entry to the requested log file with:

- which round or attempt this is
- what you tried
- what changed
- what evidence you observed
- what the next likely step is if the target is still unmet

Keep logs additive and concise.

If the task is iterative source optimization, preserve important candidate versions on disk instead of only overwriting the latest file. Keep prior versions available for comparison or rollback when feasible.

## Coordinator Guidance

- Delegate one increment at a time.
- After each child finishes, read the produced artifacts before choosing the next step.
- Before launching a new round, summarize for yourself what has already been tried, what worked, what failed, and what the current best result is.
- When sending the next round, include the latest measured results and explicitly refer to prior attempts so the next agent builds on the experiment history.
- If the target is still unmet, start another iteration instead of writing a final success summary.

## Writer Guidance

- Make one focused implementation change at a time.
- Review the current experiment history before choosing the next change.
- Prefer combining the best previous ideas incrementally when the evidence suggests they are compatible.
- Compile and do a lightweight self-check before handing work off.
- If the output is clearly wrong, revise it before exiting.

## Exit Conditions

Exit successfully only when the required artifacts exist and the latest benchmark and verification artifacts show the target is satisfied.

Exit with a blocker summary only when the blocker is concrete, recorded, and explains why the loop cannot currently make further progress.
