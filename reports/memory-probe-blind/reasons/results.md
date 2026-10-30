# Reason grades by phase (key released)

The grades in `grades.md` were written by the aos book session without
opening `key.json` or `blind.json`. The key's SHA-256 still matches the
value published in `blind-reasons.md`
(232d070dfd600b20e494399220c4e3f0204a9ae6ce5f8ca84db2ddd801058d98).
Phase 1 and 2 answers come from the committed phase-1/2 sample files.
Phase 3 answers come from the evaluation's `memory,check-rule` samples
(74510b7, injected working memory).

## Reason grade by phase (10 answers each)

| reason | phase 1 | phase 2 | phase 3 |
| --- | --- | --- | --- |
| correct | 5 | 6 | 6 |
| correct, minor invention in an aside | 0 | 2 (3, 21) | 2 (6, 15) |
| incomplete | 1 (9) | 2 (16, 28) | 0 |
| wrong: superseded fact as the reason | **4 (13, 17, 19, 27)** | 0 | 0 |
| wrong: detail | 0 | 0 | 1 (18, TG4 tile shape) |
| invented | 0 | 0 | 1 (23) |

Verdicts: phase 1 9 correct, 1 wrong (17); phase 2 10 correct; phase 3 9
correct, 1 partly correct (18).

## What the breakdown shows

- **The superseded reasons are all phase 1.**
  - Items 13, 19 and 27 explain the leaf2 PFM difference by tie-breaking, the writer's first hypothesis (art-001056 msg 4/6), which msg 38/42 overturned. All three are phase-1 answers.
  - So is 17, which repeats a live-history note ("not in writer log") instead of reading art-000969 msg 36.
  - That is 4 of 10 in phase 1 against 0 of 20 in phases 2 and 3 (Fisher two-sided p = 0.008). Against phase 3 alone it is 0 of 10 (p = 0.087).
  - Phase 1 had no message-level search or read, so an agent reading a segment page could stop at the first hypothesis. Phase 2's retrieval and phase 3's working memory both carry the later result.
- **Phase 3's errors are of a different kind.**
  - Item 18 is the TG4 tile-shape error: a lossy entry filled in wrongly.
  - Item 23 is invented. It describes the short pass as a "win that flipped" and offers a locality mechanism that is not in the record. It is also the answer that quotes the probe ID (memory tridata-order #8).
  - Wrong or invented: phase 1 4 of 10, phase 3 2 of 10 (p = 0.63). The count does not differ at n = 10; the kind does.
- **Phase 2's misses are incomplete, not wrong.** Two leaf2-status answers (16, 28) give the current state without the bug.
- **The frozen regex agrees with the reader on 26 of 30 verdicts.**
  - The regex fails 9 and 13, whose verdicts are correct.
  - The regex passes 17, whose verdict is wrong ("never measured"), and 18, which is partly correct.
  - It cannot see any of the reason errors.
