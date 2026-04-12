# Agent Microkernel Proposal

## Goal

Build a minimal `openclaw`-style agent system that treats multi-agent collaboration as a native runtime property, using concepts borrowed from operating systems rather than a monolithic orchestrator with shared prompt state.

The guiding idea is simple:

**Agents should share artifacts and protocols, not context windows.**

---

## Core Model

The system should look like a tiny agent microkernel.

It has five core subsystems:

1. **Kernel**
   Owns the global event loop, process table, capability table, scheduler, and audit log.

2. **Agent Processes**
   Each agent is an isolated unit with:
   - `pid`
   - role/spec
   - inbox/outbox
   - mounted workspace views
   - capability set
   - resource budget
   - lifecycle state: `ready | running | blocked | sleeping | exited`

3. **IPC Layer**
   Collaboration happens through native runtime primitives:
   - mailboxes for point-to-point messages
   - channels for pub/sub events
   - shared artifacts for files, diffs, reports, and plans
   - locks and leases for coordination
   - signals like `pause`, `resume`, `cancel`, `handoff`, and `checkpoint`

4. **Workspace VFS**
   Agents do not all see the same filesystem by default.
   Each agent gets a mounted view made from:
   - private scratch space
   - shared project mounts
   - read-only reference mounts
   - per-task overlays or worktrees

5. **Supervisor Tree**
   Parent agents can spawn child agents, monitor them, aggregate results, and recover from failures without collapsing the whole system.

6. **Inference Adapter**
   The runtime should treat model access as a pluggable backend rather than a built-in assumption.
   It should expose one internal inference interface that can target:
   - local `vllm`
   - Claude Code or Anthropic-compatible endpoints
   - OpenAI-compatible endpoints
   - future local or hosted providers

---

## Why This Should Support Multi-Agent Collaboration Natively

Most agent systems model collaboration by giving multiple agents the same conversation history and asking them to "coordinate" through prompts.

That approach does not scale well because it creates:

- shared hidden state
- prompt duplication
- weak task ownership
- race conditions around files
- poor failure isolation

Instead, collaboration should emerge from runtime primitives:

- `spawn(spec, mounts, caps, budget) -> pid`
- `send(pid, message)`
- `publish(topic, event)`
- `subscribe(topic)`
- `await(pid)` or `await(group_id)`
- `acquire(resource)`
- `release(resource)`
- `checkpoint(pid)`
- `signal(pid, cancel | pause | resume | handoff)`

These APIs make collaboration explicit, inspectable, and recoverable.

---

## LLM-Agnostic Inference Layer

The runtime should be explicitly **LLM-agnostic**.

The kernel, scheduler, IPC layer, and artifact system should not know whether an agent is backed by:

- a local `vllm` server
- a hosted API
- a Claude-style agent harness
- a future model runtime

Instead, model access should be hidden behind a narrow adapter boundary.

### Suggested Adapter Interface

```ts
interface InferenceBackend {
  name: string;
  supportsTools(): boolean;
  supportsStreaming(): boolean;
  supportsJsonMode(): boolean;

  generate(request: GenerateRequest): Promise<GenerateResult>;
  stream?(request: GenerateRequest): AsyncIterable<GenerateEvent>;
}
```

Where `GenerateRequest` includes things like:

- system prompt
- messages
- tool declarations
- response format expectations
- timeout
- model name
- temperature
- metadata for tracing and budgeting

This keeps the rest of the runtime independent from any single vendor.

### Separation of Responsibilities

The layers should divide responsibilities like this:

- **Kernel**
  Scheduling, capabilities, IPC, artifacts, supervision

- **Agent runtime**
  Prompt assembly, tool loop, checkpointing, state transitions

- **Inference adapter**
  Translate runtime requests into provider-specific API calls

- **Tool executor**
  Run filesystem, shell, browser, or web actions behind capability checks

That separation is important because you may want to swap models without redesigning the operating model.

---

## OS Concepts to Reuse

The system should intentionally map to familiar operating system ideas:

| OS concept | Agent runtime concept |
| --- | --- |
| Process | Agent instance |
| Thread | Lightweight subtask |
| Scheduler | Model/tool execution allocator |
| IPC | Mailboxes, channels, artifact passing |
| Filesystem | Durable shared coordination surface |
| Capability | Least-privilege tool/file/network access |
| Signal | Pause, resume, cancel, handoff |
| Supervisor tree | Parent-child agent hierarchy |
| Quotas / cgroups | Token, CPU, wall-clock, and tool budgets |

This gives the design a clear mental model and keeps the system small.

---

## Proposed Runtime Architecture

```text
User / API
   |
Agent Kernel
   |- Scheduler
   |- Capability Manager
   |- Process Table
   |- IPC Bus
   |- Artifact Store
   |- Audit Log
   |- Inference Adapter Registry
   |
   |- Supervisor Agent
   |   |- Research Agent
   |   |- Coding Agent
   |   |- Test Agent
   |   `- Review Agent
   |
   |- Tool Workers / Sandboxes
   `- Model Backends
       |- Local vLLM
       |- Anthropic-compatible
       `- OpenAI-compatible
```

### Kernel Responsibilities

The kernel should be the only component allowed to:

- create or destroy agent processes
- assign resources and budgets
- grant or revoke capabilities
- route IPC messages
- manage locks and leases
- persist state transitions
- maintain the audit log

This keeps policy centralized while letting execution stay decentralized.

The kernel should also be responsible for selecting an inference backend per agent, based on:

- requested model class
- tool requirements
- latency budget
- cost budget
- local availability
- fallback policy

### Agent Process Responsibilities

Each agent should be small and opinionated:

- read from inbox
- inspect mounted artifacts
- execute model/tool steps
- emit artifacts and events
- block when waiting on dependencies
- exit with structured results

An agent should never need global mutable state.

### IPC Responsibilities

The IPC layer should support three collaboration modes:

1. **Directed delegation**
   One agent sends a task to another specific agent.

2. **Event-driven coordination**
   Agents subscribe to topics like `task.completed`, `artifact.created`, or `review.requested`.

3. **Artifact handoff**
   Agents exchange references to durable outputs rather than pasting large text into each other's prompts.

---

## Workspace and Isolation Model

A minimal system should avoid broad shared access.

Each agent gets:

- a private writable scratch directory
- zero or more read-only mounts
- optional shared writable mounts
- explicit write ownership for contested files

Recommended rule:

**No file should be writable by multiple active agents unless the kernel has issued a lock or lease.**

This prevents most coordination bugs.

### Artifact Types

The artifact store should make outputs addressable by handle:

- plan
- source patch
- test report
- review note
- execution log
- checkpoint snapshot

Agents should pass artifact handles, not raw blobs, whenever possible.

---

## Scheduling Model

Use a cooperative scheduler first.

Each agent consumes a bounded timeslice expressed in:

- model tokens
- wall-clock time
- tool invocations
- file writes

When an agent:

- sends work to another agent
- waits on a lock
- waits on an artifact
- requests human input

it transitions to `blocked`, and the scheduler picks another runnable agent.

This gives a simple, deterministic base before introducing more advanced policies.

One practical extension is to schedule not only CPU-like runtime slices, but also **inference slots**:

- local model queue depth
- concurrent generations
- max tokens in flight
- tool-augmented versus plain generation lanes

That matters in a multi-agent system because the model server itself becomes a shared operating resource.

---

## Supervision and Failure Handling

The system should use a supervisor tree inspired by Erlang or Unix process management.

Each parent agent can:

- spawn children
- monitor their lifecycle
- aggregate outputs
- restart them with bounded retry policy
- cancel them if upstream goals change

Children should fail independently.

A worker crash should not corrupt the whole session; it should produce:

- exit reason
- partial artifacts
- last checkpoint
- retry eligibility

---

## Default Multi-Agent Pattern

A good default operating mode is:

- **Supervisor**
  Owns the user goal and final synthesis.

- **Planner**
  Breaks the goal into tasks and dependency edges.

- **Workers**
  Execute independent tasks with narrow mounts and capabilities.

- **Verifier**
  Runs tests, checks, or validations on produced artifacts.

- **Reviewer**
  Inspects work before promotion or merge.

This is a strong default because responsibilities are separated cleanly.

---

## Minimal MVP

To keep the first version small, build only:

- one kernel process
- SQLite-backed process table and event log
- per-agent inbox/outbox
- filesystem-based artifact store
- cooperative scheduler
- `spawn`, `send`, `await`, `cancel`
- file locks
- parent-child supervision
- one sandboxed tool runner
- one inference backend interface
- one concrete local `vllm` adapter for prototyping

This is already enough for:

- planner -> implementer -> tester pipelines
- parallel research agents
- reviewer/tester gates
- resumable tasks
- safe file ownership during concurrent edits

---

## Prototype Backend: Local vLLM

For prototyping, the first backend can target your local `vllm` setup while keeping the architecture provider-neutral.

From the existing interface in [worker.py](/Users/claw/Code/real-estate-map/worker.py:1), the important lesson is not the exact command shape, but the integration pattern:

- the application already treats model execution as an external service boundary
- prompts are assembled in app code
- the runtime expects structured JSON outputs for downstream automation
- the system can tolerate a local endpoint and dummy auth values

There is also an existing local wrapper in [claude_27b.sh](/Users/claw/Code/real-estate-map/claude_27b.sh:1) that points a Claude-compatible client at a local endpoint via:

- `ANTHROPIC_BASE_URL=http://lenovo-px.local:8001`
- dummy API credentials
- a local model name such as `Qwen/Qwen3.5-27B`

That suggests a clean prototype path:

1. Implement the AOS inference interface against a local endpoint first.
2. Keep the request format internal and provider-neutral.
3. Translate that request into the provider-specific wire format inside the `vllm` adapter.
4. Return normalized outputs to the agent runtime.

### Prototype Recommendation

The prototype backend should support only a narrow feature set at first:

- basic message completion
- optional streaming
- strict JSON output mode when available
- timeout and retry handling
- trace metadata

Tool use should remain in the AOS runtime, not in the provider adapter.

That means the agent loop would be:

1. runtime builds prompt and tool state
2. `vllm` adapter generates the next action
3. runtime interprets the result
4. runtime executes approved tools
5. runtime appends tool outputs and continues

This keeps tool orchestration portable even if model providers differ in function-calling semantics.

### Important Constraint

Do not make AOS depend on a Claude-compatible environment variable contract forever just because that is the easiest first prototype.

It is fine to use your local `vllm` in prototype mode, but the long-term contract should be:

- **AOS internal request schema**
- **adapter translation layer**
- **provider-specific transport**

not:

- provider-specific prompt format leaking into the kernel

---

## Suggested Process API

```ts
type Pid = string;
type ArtifactId = string;

spawn(spec, mounts, caps, budget): Pid
send(pid, message): void
publish(topic, event): void
subscribe(pid, topic): void
await(target): ExitStatus
signal(pid, op): void
checkpoint(pid): ArtifactId
acquire(resource, pid): Lease
release(resource, pid): void
attachArtifact(pid, artifactId): void
```

The API surface should stay small. Most system behavior should be expressible through composition of a few primitives.

The same principle should apply to model access:

```ts
bindModel(pid, backend, model): void
generate(pid, request): Promise<GenerateResult>
cancelGeneration(pid): void
```

This is enough for an MVP without over-designing a provider SDK layer.

---

## Design Rules

The runtime should be constrained by a few hard rules:

1. **Artifacts over transcript sharing**
   Agents collaborate through durable outputs and protocols.

2. **Capabilities over trust**
   Access is granted explicitly, not assumed.

3. **Isolation by default**
   Shared writable state should be the exception.

4. **Structured exits**
   Every task should end with status, logs, outputs, and failure reason.

5. **Supervision over improvisation**
   Recovery and retries should be policy-driven, not ad hoc prompt behavior.

6. **Inference is a driver, not the kernel**
   The model backend should be replaceable without changing core runtime semantics.

---

## Recommendation

If this project is called `aos`, I would lean into the metaphor and define it as:

**AOS is an agent operating system: a minimal runtime where agents behave like isolated processes that collaborate through scheduling, IPC, capabilities, and shared artifacts.**

That framing is strong because it gives the system:

- a clear architectural center
- natural support for multi-agent work
- debuggable behavior
- strong safety boundaries
- an implementation path that can stay small

---

## Next Step

The best next step after this proposal would be to write:

1. a concrete process model
2. an event and artifact schema
3. an inference adapter contract
4. a first-pass kernel API
5. a toy implementation with planner, worker, and verifier agents using local `vllm`

That would turn the idea into a buildable foundation without adding framework bloat too early.
