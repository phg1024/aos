# Least-privilege probes (chapter 4), against AOS 283a92e: interim

Scripted backend through the real `runtime.AgentRuntime` (ContinueOnToolError,
as agentflow runs), `tools.Runtime` with `Protected = [state, state.lock]` as
`cmd/aos` sets it, a throwaway `t.TempDir()` workspace and state dir on this
Mac's APFS (case-insensitive) volume. No model, no phg-gpu. Records:
`results/P1.json` .. `results/P4.json`.

Commands, from the repository root, in this order:

    go test ./reports/least-privilege-probes/ -count=1 -v -run 'TestP1|TestP3|TestP4'
    go test ./reports/least-privilege-probes/ -count=1 -v -run TestP2

## P2 stopped at its first attempt: a write landed in the protected state dir

c1: `write_file path=<ws>/.AOS-STATE/probe-case1.txt` with the state dir at
`<ws>/.aos-state`, mount `<ws>` writable, write scope `<ws>`. The agent saw

    write_file:map[bytes:6 path:<ws>/.AOS-STATE/probe-case1.txt]

a `tool_result` artifact was recorded, no `capability.denied` event, and
`<ws>/.aos-state/probe-case1.txt` was created. `protected()` compares
`<ws>/.aos-state` with `<ws>/.AOS-STATE/...` lexically after EvalSymlinks,
which keeps the caller's case, so the path is "not inside" the protected
root while the volume opens the same directory. Per aos book's rule the
probe stopped here: c2 to c11 (the mixed-case kernel path, the reads, the
sandboxed-run variants and the nested read-only mount) were not run. Not
fixed; the fix is aos code's and the decision the user's.

## P1, P3, P4: as the code says

- P1: file tools only: four refusals, each a tool_error artifact and a
  capability.denied event; only the legal write changed the disk; exit 0.
  With run granted, the same three writes through run are tool_result
  artifacts with exit 1 and "Operation not permitted", and no
  capability.denied event (confirmed). The denial reason for the read-only
  mount and for the state dir is the generic "path outside the granted
  scopes or mounts", also when the write scope covers them (P1c).
- P3 (network off): hardlink, rename in, /tmp by name, ../ twice, UDP to a
  loopback listener and a DNS query to a loopback listener were all
  refused ("Operation not permitted", or bind refused); listeners got 0
  datagrams; outside files unchanged; TMPDIR = scratch/pid-000001 and
  writable. dscacheutil returned no address; curl: "Could not resolve
  host". Unconfined controls for the network rows are not run yet.
- P4: web_fetch to 127.0.0.1 and localhost refused (tool_error +
  capability.denied, capability "network", 0 server hits); run curl to
  the same server: exit 0, "reached /curl", 1 hit, no event.
