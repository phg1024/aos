# AOS Refactor Plan

## Goal

Refactor AOS away from a built-in planner/worker/verifier harness and toward a more OpenClaw-like control plane:

- AOS core provides generic orchestration primitives
- users define agents and their roles
- verification is optional, not automatic
- multi-step LLM interaction is implemented as a reusable runtime loop, not as one-off app logic

This document also describes how AOS should support repeated LLM-tool-LLM interaction until a task is complete.

---

## Core Correction

The current AOS implementation is useful as a prototype, but it is too opinionated in one important way:

- it assumes a planner
- it assumes a worker
- it assumes a verifier
- it assumes verification is part of the default completion path

That is good as an example harness.
It is not the right core architecture.

The corrected model should be:

**AOS is an agent operating system and control plane, not a built-in workflow policy.**

That means AOS core should know about:

- sessions/processes
- artifacts
- task ledger
- spawn/send/await
- capabilities
- mounts
- tool execution
- runtime adapters
- event logs

AOS core should not know:

- which agent is a verifier
- whether verification is required
- whether a task has a planner
- whether a worker should write directly or ask for review
- whether a reviewer is mandatory

Those are workflow-layer choices.

---

## Target Architecture

Split AOS into three layers:

### 1. Control Plane

The durable runtime substrate:

- session/process table
- task ledger
- event log
- artifact store
- capability model
- mounts
- tool execution
- runtime selection
- spawn/send/await APIs

### 2. Agent Runtime

The reusable step loop that drives one agent session:

- prompt assembly
- LLM call
- parse next action
- execute tool or send message
- append results
- continue until exit/block/wait

### 3. Workflow Layer

User-defined collaboration patterns:

- solo agent
- planner + workers
- reviewer gate
- coding agent + tester
- custom named roles
- no verifier at all

This is where agent meaning lives.
Not in the control plane.

---

## Replace Built-In Roles With Declarative Agent Specs

Instead of hardcoding planner/worker/verifier, define agents declaratively.

Example shape:

```json
{
  "agents": [
    {
      "id": "researcher",
      "runtime": "vllm",
      "model": "Qwen/Qwen3.5-27B",
      "system_prompt": "You investigate source material and produce findings.",
      "capabilities": {
        "tools": ["read_file", "web_fetch"],
        "read_paths": ["/workspace"],
        "write_paths": ["/workspace/artifacts"]
      }
    },
    {
      "id": "editor",
      "runtime": "vllm",
      "model": "Qwen/Qwen3.5-27B",
      "system_prompt": "You assemble final documents from artifacts.",
      "capabilities": {
        "tools": ["read_file", "write_file"],
        "read_paths": ["/workspace"],
        "write_paths": ["/workspace/output"]
      }
    }
  ]
}
```

Then a task or workflow spec chooses who does what.

Example:

```json
{
  "task": "combine phase reports",
  "entry_agent": "editor",
  "available_agents": ["researcher", "editor"]
}
```

This lets the user define roles instead of AOS imposing them.

---

## Verification Must Become Optional

Verification should be moved from “automatic behavior” to “workflow policy.”

Possible policies:

- `none`
  no verification agent at all

- `required`
  final output must be approved by another agent

- `best_effort`
  run verification if configured, but do not block completion

- `manual`
  produce review artifacts but wait for human approval

AOS core should not assume one of these.

The workflow spec should.

---

## How To Support Multi-Round LLM Interaction

This is the most important runtime behavior to add.

An agent should not be treated as:

- one prompt in
- one response out

Instead, each agent session should run a loop like:

1. load current session state
2. assemble prompt from:
   - goal
   - session history
   - artifacts
   - available tools
   - capability limits
3. call the LLM
4. parse the returned action
5. if the action is a tool call:
   - execute the tool
   - append the tool result to session history
   - continue
6. if the action is a message to another agent:
   - send it
   - mark current agent blocked or waiting
7. if the action is “done”:
   - emit output artifact
   - exit
8. if the action is “need input”:
   - pause or block
9. checkpoint at safe boundaries
10. repeat until terminal state

This is the reusable agent runtime loop.

---

## Recommended Action Schema

The LLM should not return raw prose when running as an agent.
It should return structured next-step instructions.

Example:

```json
{
  "action": {
    "type": "tool",
    "tool_call": {
      "name": "read_file",
      "args": {
        "path": "/workspace/report-a.html"
      }
    }
  }
}
```

Other action types:

```json
{
  "action": {
    "type": "message",
    "message": "Ask the editor agent to merge artifact X and Y."
  }
}
```

```json
{
  "action": {
    "type": "spawn",
    "agent_id": "editor",
    "task": "Merge these artifacts into the final report."
  }
}
```

```json
{
  "action": {
    "type": "exit",
    "exit_status": {
      "code": 0,
      "reason": "done",
      "retryable": false
    },
    "artifact": {
      "type": "final_output",
      "content": "..."
    }
  }
}
```

This is how the LLM says what to do next.

---

## Runtime Loop Pseudocode

```go
for step := 0; step < maxSteps; step++ {
    state := loadSession(sessionID)
    prompt := assemblePrompt(state)

    action := llm.Generate(prompt)

    switch action.Type {
    case "tool":
        result := toolExecutor.Run(action.ToolCall)
        appendToolResult(sessionID, result)
        checkpoint(sessionID)

    case "message":
        sendToSession(action.TargetSession, action.Message)
        markBlocked(sessionID)
        checkpoint(sessionID)
        return

    case "spawn":
        child := spawnAgent(action.AgentID, action.Task, action.Options)
        recordChild(sessionID, child)
        appendSystemNote(sessionID, "spawned child "+child)
        checkpoint(sessionID)

    case "exit":
        emitArtifacts(action.Artifact)
        markExited(sessionID, action.ExitStatus)
        checkpoint(sessionID)
        return

    default:
        markExited(sessionID, malformedOutputError)
        return
    }
}
```

This should live in the runtime layer, not in each individual app command.

---

## What The LLM Needs To See Each Turn

For the loop to work reliably, the LLM should receive:

- the current task
- the agent’s identity and scope
- currently available tools
- current capability and filesystem constraints
- relevant artifacts
- prior tool results
- any messages from parent/child agents
- explicit instructions about valid action schemas

It should not rely on hidden implicit state.

That is why session state and artifact history matter.

---

## Tool Execution Should Stay Outside The LLM

Even in a multi-round loop, the LLM should not directly perform I/O.

Instead:

- LLM decides the next action
- AOS executes the action
- AOS records the result
- AOS feeds the result back into the next turn

That means:

- file I/O stays in AOS
- network I/O stays in AOS
- command execution stays in AOS
- capability enforcement stays in AOS

The LLM is the planner of actions.
AOS is the executor of actions.

---

## Message-Based Multi-Agent Collaboration

To support user-defined agent teams, add these action types:

- `spawn`
- `send`
- `await`
- `attach_artifact`
- `request_lock`
- `release_lock`

That lets an agent do things like:

- ask another agent to summarize a file
- wait for a child result
- request a reviewer
- ask a coding agent to write a patch
- hand artifacts to a final editor

This creates collaboration without hardcoding semantics into AOS.

---

## Needed Refactors In The Current Code

### 1. Move `internal/app/assemble.go` out of the default “truth”

It should become:

- either an example app
- or a workflow package on top of AOS

It should not define the default semantics of AOS itself.

### 2. Generalize `internal/runtime`

Right now the runtime loop mainly supports:

- tool
- message
- exit

It should be extended to support:

- `spawn`
- `send`
- `await`
- `attach_artifact`
- `request_lock`

### 3. Add session/task ledger semantics

AOS needs a first-class task/session control surface:

- task IDs
- owning session
- child sessions
- blocked reason
- waiting on session/artifact/lock
- completion state

### 4. Make the CLI load workflow specs

Instead of:

```bash
aos run ...
```

only meaning one built-in workflow,
it should support:

```bash
aos run --workflow team.json "do task X"
```

where `team.json` defines agents and policies.

### 5. Make verification a workflow choice

The workflow spec should decide:

- whether a verifier exists
- whether reviewer approval is required
- whether the final output can be written immediately

---

## Recommended Workflow Spec

Example:

```json
{
  "policy": {
    "verification": "manual"
  },
  "entry_agent": "coordinator",
  "agents": [
    {
      "id": "coordinator",
      "runtime": "vllm",
      "model": "Qwen/Qwen3.5-27B",
      "prompt": "Break work into subtasks and coordinate agents."
    },
    {
      "id": "writer",
      "runtime": "vllm",
      "model": "Qwen/Qwen3.5-27B",
      "prompt": "Produce consolidated draft text."
    },
    {
      "id": "reviewer",
      "runtime": "vllm",
      "model": "Qwen/Qwen3.5-27B",
      "prompt": "Review outputs and flag issues."
    }
  ]
}
```

Then AOS just runs the control plane.

---

## Recommended Next Steps

1. Add a `workflow` spec format for user-defined agents.
2. Refactor automatic verification out of the core `run` path.
3. Extend the runtime loop to support `spawn`, `send`, and `await`.
4. Introduce a session/task ledger for parent-child orchestration.
5. Keep current planner/worker/verifier as example workflows, not defaults.

---

## Bottom Line

To make AOS work the way you want:

- **AOS must become role-agnostic**
- **verification must be optional**
- **agent meaning must come from user-provided workflow specs**
- **multi-turn LLM interaction must be implemented as a reusable action loop**

That is the path from the current prototype to a true agent operating system.
