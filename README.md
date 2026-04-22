# AOS

AOS is a research prototype for an agent operating system built around deterministic runtime semantics, artifact-based collaboration, and replaceable inference backends.

The codebase is intentionally exploratory, but the core runtime is implemented and covered by Go tests. The current repo includes a file-backed kernel, IPC bus, artifact store, lease manager, capability model, tool runtime, workflow engine, live vLLM integration, and an interactive terminal UI.

## Current Status

- Research prototype, not a polished product release
- Core Go packages test cleanly with the repo-local cache path used by `make test`
- Active areas include workflow tooling, skills, local TUI ergonomics, and evaluation/demo workflows
- Generated live-run artifacts are treated as disposable outputs, not durable source assets

See [STATUS.md](./STATUS.md) for the current project snapshot and [RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) for repo hygiene before sharing milestones.

## Repository Layout

- `cmd/` entrypoints for the CLI, TUI, smoke tests, demos, and mini-project runners
- `internal/` runtime, kernel, workflow, inference, tool, and TUI implementation packages
- `skills/` local skill definitions used by workflow or interactive sessions
- `workflows/` JSON workflow specs for multi-agent experiments and demos
- `reports/` curated milestone reports and a small set of durable reference outputs

## Common Commands

- `make test`
- `make fmt`
- `go run ./cmd/aos`
- `go run ./cmd/aos-tui --workdir .`

`make test` uses repo-local `GOCACHE` and `GOTMPDIR` paths so tests run reliably in sandboxed environments too.

## Docs

- [STATUS.md](./STATUS.md)
- [Agent Microkernel Proposal](./agent-microkernel-proposal.md)
- [Guiding Principles](./guiding-principles.md)
- [Implementation Plan](./implementation-plan.md)
- [Testing Plan](./testing-plan.md)
- [Mini Projects](./multi-agent-mini-projects.md)

## Curated Reports

- [Phase 0 Report](./reports/phase-0.html)
- [Phase 1 Report](./reports/phase-1.html)
- [Phase 2 Report](./reports/phase-2.html)
- [Phase 3 Report](./reports/phase-3.html)
- [Phase 4 Report](./reports/phase-4.html)
- [Phase 5 Report](./reports/phase-5.html)
- [Phase 6 Report](./reports/phase-6.html)
- [Phase 7 Report](./reports/phase-7.html)
- [Phase 8 Report](./reports/phase-8.html)
- [Final Completion Report](./reports/final-completion.html)
