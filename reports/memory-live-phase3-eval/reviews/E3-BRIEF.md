# E3 review brief (live run reports/pathtracer-bunny-phase3)

You are helping an evaluation session count memory failures ("E3") in one
multi-agent live run. AOS ran a coordinator that, for 8 rounds, delegated to a
writer (optimizes a Metal path tracer), then a benchmarker, then a verifier.
Each process is one LLM agent with its own history. Long histories are
compacted: old messages go to an archive (memory_segment artifacts) and the
agent keeps a keyed "working memory" (WM) it writes with update_memory, plus
search_memory/read_memory tools to look things up.

You only read. Do not edit, run, or create anything outside the output file
named in your task.

## Files (all plain text)

Directory D = /private/tmp/claude-501/-Users-peihongguo-Code-aos--claude-worktrees-competent-heyrovsky-566e83/fdd47170-a4d5-4d70-bc51-ba9fca64e981/scratchpad/live

- `D/p3/<pid>-<agent>.txt`: the FULL history of a process: every message, in
  order, as `=== #<seq> <role> (<artifact holding it>)`. Message numbers
  (#seq) are what you cite. Assistant messages are JSON actions with "notes"
  (the agent's running summary) and a tool_call. Tool results follow as
  role "tool". Compaction markers / handoff notices appear as user/tool
  messages.
- `D/p3/<pid>-<agent>.wm.txt`: every version of that process's working memory
  (artifact id, time, version, then `[section/key] content EVIDENCE=...`).
- `D/p3notes/<pid>-<agent>.txt`: a compact extract of the same history: per
  assistant message, its notes and tool call. Start here, then open the full
  history at the message numbers you need.
- Shared files the agents read and write (final state; earlier rounds'
  sections were available to later rounds):
  `/Users/peihongguo/Code/aos/reports/pathtracer-bunny-phase3/pathtracer_writer_log.txt`,
  `.../pathtracer_coordinator_log.txt`, `.../pathtracer_benchmarker_log.txt`.
  Each round's delegation goal is the first user message (#1) of its process.

Ignore two environment faults, they are not E3: (1) 00:06-00:25Z Xcode license
failures and the DEVELOPER_DIR=/Library/Developer/CommandLineTools workaround;
(2) model backend outages (messages "tool_error name=backend ..."), in
pid-000018 #20-#76, pid-000020, and the coordinator.

## What to count (three categories, kept separate)

**A. Superseded or wrong fact cited as current.** The agent states, as
current or true, a fact (a measurement, a status such as "X is adopted / was
rejected / is the best", a cause) that information already available to it
contradicted or superseded: its own earlier messages (live or archived), its
own WM, its delegation goal, or a shared log it had read in this process. It
must be used as current (in a note it acts on, a decision, a WM entry, a log
line, a delegation, or its final report), not merely quoted as history. An
error the agent corrects within a step or two by itself still counts if it
was acted on (say so); a slip it catches before acting is listed as
"near-miss", not counted.
Calibration from the earlier run: a writer read an archived note "leaf2
measured 33.5 vs 45.8" as current, concluded "the leaf2 adoption was lost",
and re-adopted leaf2, which it had already found biased. Also: a coordinator
stated a wrong fact about leaf2 in a delegation while the correct one was in
its live context.

**B. Experiment repeated.** The agent runs an experiment (build + A/B,
benchmark of a config, a correctness test of a variant) whose answer was
already available to it (same sources as A) and does not give a reason that
makes the rerun new (a changed baseline or tree that the agent names, a
measurement it said was noisy and needs more pairs, a deliberate
confirmation run it labels as such). Count the rerun, not the original.
Cross-round counts: a writer re-testing an idea an earlier round already
measured and recorded in the writer log or the delegation counts if it gives
no reason the old result does not apply. List re-tests that DO give such a
reason as "justified re-test", not counted.
Calibration: a writer decided leaf2 "almost certainly" had a bug, built
leaf2b, measured it slower and rejected it; its own archived messages had
already found and fixed that bug and measured leaf2 as a loss. Also: a
round-8 writer re-tested 2-triangle leaves that earlier rounds had measured
slower.

**C. Invented rationale recorded.** A cause, mechanism or reason stated as
fact and RECORDED durably (update_memory content, a write to a shared log
file, the coordinator ledger, a delegation, or the process's final exit
report) that no measurement or source in the history supports, or that the
history contradicts. Hypotheses labelled as such ("likely", "probably",
"hypothesis") do not count unless later recorded as fact. A reason backed by a
measurement (profile, counter, A/B) is not invented even if wordy.

Do not count: ordinary noise in timing, a wrong guess the agent then tests,
environment faults, or disagreements between processes when each was right
with what it had.

## Output

Write a markdown file at the path given in your task. For each instance:

```
### <category A|B|C or near-miss|justified re-test> — <pid> #<seq>[-#<seq>] — <one-line summary>
- Claim/experiment: quote (<= 30 words) from #<seq> (artifact art-NNNNNN if shown).
- Already available: quote from where the right/earlier answer was, with #seq
  (and pid/artifact or log line numbers), and how the agent had access to it
  (live, archived, WM, log it read at #seq, delegation).
- Acted on: what it did with it (#seq).
- Confidence: high / medium, and why.
```

Then a short table of counts per category for your processes, and a list of
processes you checked with nothing found. Be strict and literal: every
instance must have both quotes. If unsure, list it with confidence medium and
say what would decide it. Quality matters more than count; zero is a fine
answer.
