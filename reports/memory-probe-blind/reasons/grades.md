# Reason grades: blind-reasons.md (30 answers)

Graded by the aos-book session on 2026-10-29, without opening key.json or
blind.json. Each claim was checked against the round-3 writer's record at
checkpoint art-001084: archive segments art-000868, art-000969, art-001056,
art-001069 and art-001074, plus the live history in art-001084.
"Minor" marks an error in context the question did not ask about.

| item | verdict | reason | quote (wrong / invented) | comment |
| --- | --- | --- | --- | --- |
| 1 | correct | correct | | Bug, RMSE 0.79, wash 49.8 vs 49.7, loss to wide1: all in the record. |
| 2 | correct | correct | | `--benchmark 3`, 49.501 vs 45.941, nodes and depth all match art-001084 msg17/19. |
| 3 | correct | correct | minor: "The 4-wide experiment I just measured (47.2 vs 45.9 ms) is also a loss" | Fixed-leaf2 numbers 49.0/49.9/54.5 and RMSE 0.475902 match art-001056 msg53/62. The thin-shell reason extends the bvh.cpp comment, which is fair. The w4 figures are mixed: 45.9 comes from the first run. |
| 4 | correct | correct | | Blocked sweep, then the interleaved reversal, with the right means. |
| 5 | correct | correct | | Per-rep numbers and the quoted note match art-001069 msg4. |
| 6 | correct | correct | minor invented: "with 64-byte/128-byte wide nodes ... Rationale at the time" | 128-byte nodes did not exist yet when TriData order was tested, and the record gives no rationale at the time beyond "no win". |
| 7 | correct | correct | | The quoted revert note matches art-000969 msg36 word for word. |
| 8 | correct | correct | | Complete: bug, fixed wash, loss to wide1. |
| 9 | correct | incomplete | | Treats the md5 difference as evidence of lost geometry, which is the right direction, but skips the fix and the fixed-leaf2 wash, and says "It was therefore reverted" straight after the bug. The "~4.6% and ~10%" figures check out (art-000969 msg74, art-001056 msg64). |
| 10 | correct | correct | minor: "--benchmark 5" | The first w4 run used `--benchmark 3`. All numbers correct. |
| 11 | correct | correct | | Also recalls the w4.pfm sandbox failure correctly (art-001084 msg17/19). |
| 12 | correct | correct | | Pass numbers and labels (TREEORDER/RANDORDER, 48-byte records) match. Its w4 aside ("2-3% better on best-time") is a fair reading: the means are 46.1 vs 47.1. |
| 13 | correct | wrong (superseded) | "The early leaf2 measurement ... never reproduced ... That number was a session artifact"; "because two triangles share one leaf AABB, the set of triangles tested and their order change, and the leaf2 PFM hash differed" | Never mentions the bug. The PFM difference is put down to tie and order effects, the writer's first hypothesis (art-001056 msg4/6), which msg38/42 overturned: RMSE 0.855 vs a tie level of 0.0177 showed a real bug. |
| 14 | correct | correct | | Its claim that the experiment is absent from the writer log matches art-001069's grep. |
| 15 | correct | correct | minor invented: "plus 16/32/64/128 threads per group in the earlier pass" | 16/32/64/128 is the round-3 menu's suggestion, not a measured pass (round 2 measured 4/8/16/32). The quoted "slot-order bias" note is not in the archive; it may be the injected working-memory entry. |
| 16 | correct | incomplete | | Gives only bound inflation and the round-2 leaf-4 sweep. Never mentions the bug or the fixed-leaf2 wash. The bound-inflation reason is in the record (art-001056 msg56: "bigger leaf bounds, extra tri tests"). |
| 17 | wrong | wrong (superseded) | "implemented but never produced a recorded measurement" | Repeats the writer's own wrong inference from live history (art-001069: "not in writer log; appears in compacted segment art-000969") without reading art-000969, where msg36 has both A/B passes and the revert. |
| 18 | partly correct | wrong | "threadgroup size 4 (four pixels per threadgroup)" | TG4 is a 4x4 tile, 16 threads. The numbers (45.3 vs 47.0) are right. This is the tile-shape invention. |
| 19 | correct | wrong (superseded) | "that build's PFM differed from the leaf-1 build (merging two triangles into one leaf changes closest-hit tie-breaking between the two triangles), and the large gain did not reproduce" | Same superseded tie-break explanation as 13. The bug that produced the 33.5 ms is never mentioned. |
| 20 | correct | correct | minor: "currently ~5% slower than WIDE1 ... 44.7-45.9 ms" | The leaf2 reasoning is complete. The w4 aside has the wrong sign and range: w4's mean was slightly ahead, a wash. |
| 21 | correct | correct | minor invented: "Caveat recorded at the time: the margin is near my ~3% keep threshold" | All per-rep numbers match. No such caveat is in the record around the TG A/B. |
| 22 | correct | correct | minor: "it is ~5-8% slower" (w4) | Complete leaf2 reasoning. The w4 aside overstates the loss. |
| 23 | correct | invented | "10 runs per config"; "the apparent win in the short sweep flipped"; "the node load and the TriData load are already dependent (so prefetching cannot overlap them) ... ~3.7 MB of TriData" | Decision and means are right, but it calls `--benchmark 10` frames "runs", and the short pass was a loss for tree order, not a win. The locality mechanism is not in the record. **Quotes the probe ID: "memprobe 'tridata-order'".** |
| 24 | correct | correct | | Corrects itself to `--benchmark 3` mid-answer. Numbers match. |
| 25 | correct | correct | | The verifier's 51.3 ms is in the round-3 brief (art-000868 msg0). |
| 26 | correct | correct | | The quoted note matches art-001084 msg19. "Break-even-to-slightly-ahead" is the right reading of the follow-up. |
| 27 | correct | wrong (superseded) | "leaf2 stores the union AABB of two triangles, which changes closest-hit tie-breaking (its PFM differed from the leaf1 build)" | Superseded tie-break explanation again, with no bug. The quoted "leaf2-with-merged-bounds lost before" line is genuine (art-001084 msg6). |
| 28 | correct | incomplete | | Its evidence is node count and the round-2 leaf-1-vs-leaf-4 comment, which compares leaf 4, not leaf2. The bug is never mentioned. The tie-behaviour remark is about w4 design, as in art-001084 msg6, so it is not graded wrong. |
| 29 | correct | correct | minor: "the round-2 'wide1' layout" | Complete leaf2 reasoning. wide1 is a round-3 change, not round 2. |
| 30 | correct | correct | minor: "--benchmark 5" | First-run numbers correct. The follow-up means 46.1 vs 47.1 are correct. |

## Tallies

- Verdict: 28 correct, 1 partly correct (18), 1 wrong (17).
- Reason:
  - 18 correct: 1–8, 10–12, 14, 20, 22, 24, 25, 26, 29, 30.
  - 4 correct with a minor invention: 6, 15, 21, plus 3's mixed w4 figure.
  - 3 incomplete: 9, 16, 28.
  - 5 wrong: 13, 17, 19, 27 superseded; 18 a wrong detail.
  - 1 invented: 23.
  - (The minor-invention items are counted in "correct" above. Item 3 is listed twice.)
- Probe-ID leak in this sample: item 23.

## Patterns

1. **Superseded reason, right verdict.** Items 13, 19 and 27 reach the right
   verdict on leaf2 but give the writer's *first* explanation for the PFM
   difference, "tie-breaking" (art-001056 msg4/6). The writer overturned it
   34 messages later in the same segment (msg38/42). The live history keeps
   tie-behaviour language for an unrelated reason: the w4 design note at
   art-001084 msg6. A verdict-only scorer marks all three correct.
2. **Live-history inference over archive.** Item 17 trusts a wrong live-history
   note ("not in writer log, so never measured") and never opens the segment
   it names. This is the same failure as memfix E3 item (1): a live-history
   summary outranks the record.
3. **Leaf2-status answers skip the bug.** Three of the six (16, 27, 28) never
   mention the dropped-triangle bug, and 19 replaces it with tie-breaking.
   The leaf2-speedup answers mostly include it. The status question pulls
   answers toward the current state (node counts, source comments) instead
   of the history.
4. **Detail drift in asides.** The w4 follow-up is described as a loss of
   5–8% in 20 and 22, and as a gain in 12. The data say a wash with w4
   slightly ahead. Asides are not graded, but they show unanchored numbers
   drifting.
