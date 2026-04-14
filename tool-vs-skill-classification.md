# Tool vs Skill Classification for AOS

## Purpose

This document defines which capabilities in AOS should be implemented as:

- built-in tools owned by the runtime, or
- external markdown skills layered on top of those tools.

The goal is to keep AOS small, OS-like, and capability-safe while still making higher-level workflows easy to add and evolve.

## Core Principle

Built-in tools should be:

- low-level primitives
- capability-checked
- resource-touching
- auditable
- stable across projects

Skills should be:

- reusable workflows
- project- or language-specific
- composed from built-in tools
- easy to iterate without changing the runtime

In short:

**Tools are substrate. Skills are procedure.**

## Decision Rules

Use a built-in tool when a capability:

- reads or writes real resources directly
- needs runtime-enforced permissions
- must produce structured audit events
- is fundamental to multi-agent orchestration
- should behave consistently across all projects

Use a skill when a capability:

- is mainly a playbook or strategy
- combines multiple tools into a workflow
- is language-specific or stack-specific
- may vary significantly across projects
- is likely to evolve rapidly

## Built-In Tool Categories

These belong in the AOS runtime.

### Workspace Primitives

- `read_file`
- `write_file`
- `edit_file`
- `search_files`
- `list_files`

These are direct workspace operations and must be capability-gated and logged.

### Execution Primitives

- `run_command`

This should remain built-in because process execution is a high-risk operation that needs sandboxing, policy checks, and observability.

### Network Primitives

- `web_search`
- `web_fetch`

These should remain built-in because they touch external systems and should be centrally permissioned and observable.

### Agent-Orchestration Primitives

- `spawn_agent`
- `message_agent`
- `await_agent`
- `checkpoint`
- `list_artifacts`
- `publish_artifact`
- `acquire_lease`
- `release_lease`

These are core to the identity of AOS as an agent operating system and should not be implemented as ad hoc skills.

## Skills by Default

These should start as markdown skills rather than built-in tools.

### Project Workflow Skills

- `run_tests`
- `run_lint`
- `format_code`
- `project_health`
- `fix_build`
- `project_index`
- `tree`

These are compositions over `run_command`, `search_files`, and file tools. Different repositories need different behavior, so they should remain easy to change without changing AOS internals.

### Language Runner Skills

- `python-runner`
- `cpp-runner`
- future `go-runner`
- future `rust-runner`

These are best represented as instructions for how to use `run_command` safely and effectively in a given language ecosystem.

### Coding Workflow Skills

- `code_review`
- `bug_triage`
- `refactor_assistant`
- `release_notes_writer`
- `docs_synthesis`

These are multi-step strategies, not substrate primitives.

### Research Skills

- `research`
- `fact-check`
- `source-comparison`

These are higher-level procedures built on top of `web_search`, `web_fetch`, file reading, and synthesis.

## Borderline Cases

Some capabilities can start as skills and later be promoted to built-in tools if AOS needs stronger guarantees.

### Git Operations

- `git_status`
- `git_diff`
- `git_commit_message`
- `git_review`

Recommendation:

- start as skills over `run_command`
- consider promoting selected read-only git operations to built-in tools later if safer structured access becomes important

### Symbol Rename

- `rename_symbol`

Recommendation:

- start as a skill
- promote only if AOS later grows AST-aware structured refactoring support

### Project Map / Structural Search

- `project_map`
- `ast_search`

Recommendation:

- start as skills that shell out to external tools
- promote only if AOS standardizes a structural indexing subsystem

## Why This Matters for AOS

If too many high-level behaviors become built-in tools, AOS turns into a growing pile of app-specific helpers instead of an agent runtime.

Keeping the runtime small gives AOS:

- a cleaner security model
- a smaller maintenance surface
- better multi-agent composability
- easier experimentation through skills
- fewer runtime-level breaking changes

This also matches the overall AOS direction:

- the runtime should provide primitives
- workflows should be defined above the runtime
- agent behavior should be configurable rather than hardcoded

## Recommended Initial AOS Boundary

### Keep Built-In

- `read_file`
- `write_file`
- `edit_file`
- `search_files`
- `list_files`
- `run_command`
- `web_search`
- `web_fetch`
- `spawn_agent`
- `message_agent`
- `await_agent`
- `checkpoint`
- `list_artifacts`
- `publish_artifact`
- `acquire_lease`
- `release_lease`

### Prefer Skills

- `run_tests`
- `run_lint`
- `format_code`
- `project_health`
- `fix_build`
- `project_index`
- `tree`
- `code_review`
- `bug_triage`
- `research`
- `docs_synthesis`
- `release_notes_writer`
- language-specific runners

## Migration Guidance for Current AOS

Current AOS already moved in the right direction by turning direct code execution behavior into skills like:

- `python-runner`
- `cpp-runner`

The next cleanup steps should be:

1. keep `run_command` as the only generic execution primitive
2. avoid adding new language-specific built-in tools
3. implement test, lint, and project-analysis behavior as skills
4. reserve built-in additions for capability-sensitive primitives only

## Future Extension Model

When a new capability is proposed, classify it in this order:

1. Can this be expressed as instructions over existing tools?
2. Does it need runtime-level permissions or resource controls?
3. Does it need stable structured output across all projects?
4. Is it core to agent orchestration?

If the answer to (1) is yes and the others are mostly no, it should probably be a skill.

If the answer to (2), (3), or (4) is yes, it likely belongs in the runtime as a built-in tool.

## Bottom Line

AOS should be conservative about adding built-in tools.

The runtime should own:

- filesystem primitives
- execution primitives
- network primitives
- artifact and coordination primitives
- agent orchestration primitives

Most workflow intelligence should live in skills.

That keeps AOS closer to an operating system and farther from a grab bag of hardcoded agent tricks.
