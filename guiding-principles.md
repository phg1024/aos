# AOS Guiding Principles

## Goal

This document defines the guiding principles for implementing, testing, and verifying AOS.

It is informed by three things:

1. the AOS microkernel proposal
2. the test and scenario documents already written for this project
3. two external reference points:
   - Ralph, a fresh-context autonomous coding loop
   - Anthropic's harness engineering principles for long-running application development

The goal is not to copy either system directly.

Instead, the goal is to extract what is durable from them and apply it to an OS-native multi-agent runtime that is:

- artifact-driven
- model-agnostic
- testable without a live LLM
- able to prototype against your local `vllm`

---

## Reference Insights

From Ralph:

- fresh instances reduce context drift
- memory should live in explicit artifacts like task state, git history, and progress logs
- work should be broken into small units
- feedback loops are mandatory
- completed work should be recorded in machine-readable form

From Anthropic's harness work:

- planner, generator, and evaluator roles solve different failure modes
- structured handoff artifacts are often better than one endless context
- self-evaluation is weak; external evaluation is stronger
- evaluator quality requires iteration and tuning
- every harness component is an assumption that should be stress-tested
- harnesses should be as simple as possible, and only become more complex when measurement proves the need

These ideas fit AOS well, but they should be expressed through kernel primitives rather than only through prompts or shell scripts.

---

## Core Principle

**AOS should treat agent collaboration as a runtime systems problem, not a prompt choreography problem.**

That means the durable center of the design should be:

- process state
- artifacts
- capabilities
- scheduling
- supervision
- verification

not:

- one giant conversation
- hidden prompt state
- informal agent cooperation

---

## Implementation Principles

## 1. Make the Runtime Deterministic Where Possible

The kernel should be more like an operating system than an agent.

Core semantics should be deterministic:

- process lifecycle
- message routing
- artifact creation
- lock acquisition
- lease expiration
- scheduling policy
- retry policy
- audit logging

Models are allowed to be probabilistic.
The runtime should not be.

This is the main way to keep the system debuggable.

---

## 2. Keep Memory Outside the Model

Ralph’s strongest idea is that persistent memory should live outside the current model context.

In AOS, durable memory should live in:

- the event log
- task state
- artifact store
- checkpoints
- source control metadata
- explicit project memory files when needed

An agent should be able to die, restart, or hand off work without losing the system’s actual state.

This principle is especially important for local `vllm` prototyping, where long-context behavior may vary by model.

---

## 3. Prefer Structured Handoffs Over Long Shared Context

Anthropic's harness article makes a useful distinction between context continuity and actual task coherence.

In AOS:

- long-running work should be partitioned
- handoffs should be artifact-based
- agents should receive a fresh working context whenever that improves reliability
- compaction and reset should be policies, not assumptions

The right handoff artifact usually includes:

- what was attempted
- what changed
- what remains
- what failed
- what should happen next

This is more robust than relying on implicit memory inside a long prompt history.

---

## 4. Keep Work Units Small and Legible

Ralph is correct that small tasks are load-bearing.

In AOS, tasks should be:

- bounded in scope
- bounded in write set
- bounded in expected runtime
- bounded in required context

If a task is too large, the failure should happen early at planning time, not late during execution.

The scheduler and planner should cooperate to create work units small enough that:

- they fit in one agent session
- they can be retried safely
- they can be reviewed independently
- they can be verified with concrete checks

---

## 5. Separate Planning, Doing, and Judging

One of the clearest lessons from the Anthropic harness is that self-evaluation is weak.

AOS should separate:

- **Planner**
  Defines scope, contracts, and dependencies

- **Worker / Generator**
  Produces code, documents, analyses, or patches

- **Evaluator / Verifier**
  Checks whether outputs satisfy contracts

- **Reviewer**
  Applies critical judgment where subjective or cross-cutting quality matters

These do not all need to be present in every flow.
But the architecture should make them easy to compose.

---

## 6. Verification Must Be Externalized

A worker saying "I think this is done" is not verification.

Verification should come from something external to the producing agent:

- unit tests
- integration tests
- browser checks
- schema validation
- diff review
- contract-based evaluation
- separate evaluator agents

This is a direct lesson from both Ralph and Anthropic's harness work.

In AOS, a task should not become complete merely because the producing agent exits successfully.
It should become complete only when the required verification artifacts exist.

---

## 7. Every Harness Component Must Earn Its Keep

Anthropic's article makes an important engineering point:

Every harness component encodes an assumption about model weakness.

That means every major AOS feature should answer:

- what failure mode does this address?
- how do we know it helps?
- when should it be removed or simplified?

Examples:

- planner role
- evaluator role
- context reset policy
- reviewer gate
- retry policy
- multi-stage task decomposition

If a component adds complexity without measurable lift, it should be removed or downgraded.

---

## 8. Treat the Inference Backend as a Driver

The AOS kernel should not be built around one provider contract.

Your local `vllm` is an excellent prototype backend, but it should remain behind an inference adapter boundary.

The rule is:

- AOS defines internal task, artifact, and tool semantics
- adapters translate those semantics into provider-specific API calls

This lets us prototype quickly with local `vllm` while preserving long-term portability.

It also lets us test most of the runtime without any live model at all.

---

## 9. Shared State Must Be Explicit and Minimal

Concurrency bugs are one of the fastest ways to make multi-agent systems feel fake.

AOS should avoid broad shared mutable state.

Shared state should be limited to:

- event log
- artifact store
- locked or leased resources
- explicit shared mounts

Everything else should be private to an agent or task.

If two agents need to collaborate, the default should be:

- pass an artifact
- publish an event
- request a lock

not:

- silently edit the same workspace with hope

---

## 10. Build for Recovery, Not Just Happy Paths

An OS metaphor is only meaningful if failure handling is real.

AOS should assume:

- workers crash
- tools fail
- models time out
- outputs are malformed
- the runtime may restart

So the system should provide:

- checkpoints
- structured exits
- restart policies
- resumable task states
- durable logs and artifacts

Recovery is not a stretch goal.
It is part of the architecture.

---

## Testing Principles

## 11. Test the Runtime Separately From the Model

The system has two very different kinds of correctness:

1. runtime correctness
2. model-mediated task quality

These should not be collapsed into one test suite.

Runtime correctness includes:

- lifecycle transitions
- IPC delivery
- scheduling fairness
- lock safety
- artifact lineage
- recovery behavior

These should mostly be tested with deterministic fakes.

Model-mediated quality includes:

- plan usefulness
- output correctness
- evaluator reliability
- multi-agent collaboration quality under real inference

These should be tested separately, including with local `vllm`.

---

## 12. Use Layered Verification

Testing should move upward in layers:

1. unit tests
2. component tests
3. integration tests
4. scenario tests
5. live local-model runs

If a behavior can be tested without a live model, it should be.

Live model runs should be reserved for:

- adapter validation
- prompt-loop robustness
- true collaboration scenarios
- performance under local inference contention

This keeps development fast while still validating the real system.

---

## 13. Verify Collaboration, Not Just Output

A passing final answer is not enough.

For AOS, scenario tests should also ask:

- did delegation happen?
- were child artifacts actually consumed?
- did locks prevent unsafe overlap?
- did a verifier or reviewer change the outcome?
- did failures stay isolated?

If one agent could have done the same work in one prompt, the runtime has not proven much.

This is why the mini-project suite matters.

---

## 14. Evaluators Need Their Own Tuning Loop

Anthropic's article is very clear on this point:
evaluators do not become strong automatically.

In AOS, evaluator prompts, contracts, and checks should be developed through a loop:

1. inspect evaluator logs
2. compare evaluator judgments to human judgment
3. identify false approvals and shallow checks
4. refine evaluator instructions or acceptance criteria
5. rerun scenarios

This applies whether the evaluator is checking:

- code behavior
- UI interactions
- extracted JSON
- design quality
- document completeness

Evaluator quality is an explicit subsystem, not a free byproduct.

---

## 15. Acceptance Criteria Should Be Machine-Checkable When Possible

The closer a task gets to machine-checkable contracts, the easier it is to verify safely.

Good acceptance criteria include:

- tests pass
- schema validates
- exact files changed are known
- browser workflow succeeds
- required artifacts exist
- expected events were emitted

Subjective quality still matters, but subjective review should sit on top of objective gates where possible.

---

## 16. Measure Cost, Latency, and Load-Bearing Complexity

The Anthropic piece emphasizes that harnesses can become expensive and bulky.

AOS should measure at least:

- wall-clock runtime
- inference calls
- tokens in and out
- retries
- tool invocations
- queue wait time
- evaluator cost
- success rate by scenario

This lets us answer:

- which components produce lift?
- which components are unnecessary overhead?
- when does local `vllm` become the bottleneck?

Without these measurements, complexity has no accountability.

---

## Verification Principles

## 17. Completion Should Be Evidence-Based

A task is not complete because an agent says it is complete.

A task is complete when:

- required artifacts exist
- required checks pass
- required reviewer or evaluator gates are satisfied
- state transitions are recorded

This should be encoded in task contracts, not left to agent discretion.

---

## 18. Auditability Is Part of Correctness

For a system like AOS, being unable to explain what happened is itself a defect.

Every significant run should leave behind:

- process tree
- event log
- artifact lineage
- lock history
- verification results
- final decision trail

This makes debugging, benchmarking, and trust much easier.

It also makes the system more useful for improving prompts, evaluators, and policies over time.

---

## 19. Human Oversight Should Focus on Policy and Exceptions

Humans should not need to micromanage every step.

The right role for humans is to:

- define policies
- review ambiguous failures
- adjust evaluator standards
- decide on tradeoffs the system cannot infer safely
- inspect unexpected behavior

This is another reason to keep the runtime structured and auditable.

The better the system records its own reasoning surface through artifacts and logs, the less human oversight turns into archaeology.

---

## 20. Prototype With Real Work, Not Synthetic Benchmarks Alone

The mini-project suite should be treated as a core verification tool, not just a demo set.

Real collaboration behaviors emerge best in tasks that involve:

- decomposition
- file coordination
- review
- verification
- recovery
- resource contention

Your local `vllm` setup is especially useful here because it lets us test the actual orchestration shape we care about, not just unit-level abstractions.

---

## Practical Implications for AOS

If we apply these principles honestly, AOS should be implemented with the following posture:

- keep the kernel small and deterministic
- keep memory in artifacts and logs
- use fresh-context handoffs when beneficial
- split tasks aggressively
- separate planning, execution, and evaluation
- require external verification
- use local `vllm` through a narrow adapter
- test runtime semantics with fakes
- test collaboration with scenario projects
- remove harness complexity that does not produce measured lift

That is the version of AOS most likely to stay understandable while still supporting serious multi-agent work.

---

## Recommended Non-Negotiables

If I had to reduce this document to a few non-negotiable rules, they would be:

1. **State lives outside the model.**
2. **Tasks must be small, explicit, and verifiable.**
3. **Workers do not verify themselves.**
4. **Artifacts and event logs are first-class system outputs.**
5. **Most runtime behavior must be testable without a live LLM.**
6. **Live local `vllm` runs are for end-to-end validation, not basic correctness.**
7. **Any harness complexity must justify itself with measured improvement.**

---

## Related Documents

- [agent-microkernel-proposal.md](/Users/claw/Code/aos/agent-microkernel-proposal.md)
- [testing-plan.md](/Users/claw/Code/aos/testing-plan.md)
- [multi-agent-mini-projects.md](/Users/claw/Code/aos/multi-agent-mini-projects.md)

---

## Sources

- Ralph repository: https://github.com/snarktank/ralph
- Anthropic engineering article: https://www.anthropic.com/engineering/harness-design-long-running-apps
