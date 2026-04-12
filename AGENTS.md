# AOS Agent Notes

## Non-Negotiables

- Keep the deterministic kernel independent from inference providers.
- Prefer stdlib-only Go unless a dependency is clearly justified.
- State lives in durable artifacts and logs, not in model context.
- Workers do not verify themselves; verification must be externalized.
- Every subsystem needs unit tests before it is considered complete.
- HTML reports are required after each completed implementation phase.

## Current Assumptions

- Implementation language: Go
- First live backend: local `vllm`
- Early persistence: JSON/file-backed durable store to avoid dependency drag
- Initial focus: phases 0 through 4 from `implementation-plan.md`
