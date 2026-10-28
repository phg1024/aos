# AOS Status

## Positioning

AOS is a research prototype. It is intended to explore agent runtime design, deterministic state transitions, artifact-first coordination, and replaceable inference backends. It is not yet packaged as a general-purpose product or stable SDK.

## What Works Today

- Durable file-backed kernel and process model
- IPC bus, artifact store, and lease management
- Capability-aware tool runtime and workflow execution
- Planner/worker contract harness with an optional verifier (`none`, `required`, or `best_effort` policy)
- Local vLLM backend integration
- Interactive TUI with workflow loading, skill loading, persistent state, and recovery support
- Go test coverage across the main internal packages

## Active Work

- Workflow prompt and orchestration refinement
- Skill design and coordination patterns
- Live demos and benchmark-oriented experiments
- TUI usability and local research ergonomics

## Known Gaps

- Top-level docs and operator guidance are still catching up with the implemented surface area
- Some workflows and demos are intentionally experimental and should not be treated as stable interfaces
- Live-run report trees can produce large generated outputs, so repo hygiene needs active curation

## Current Repo Guidance

- Treat `reports/live-*` style outputs, transcript logs, and scratch programs as generated artifacts
- Prefer `make test` over bare `go test ./...` when working in constrained or sandboxed environments
- Use the curated reports and design docs for durable context; use live demo outputs for local iteration only

## Next Cleanup Priorities

- Keep the README aligned with the actual commands and project scope
- Keep generated run artifacts out of version control unless they are intentionally curated examples
- Separate stable reference workflows from ad hoc experiment workflows as the repo evolves
