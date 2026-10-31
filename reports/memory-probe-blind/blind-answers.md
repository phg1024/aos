# Blind grading: 10 memory-probe answers

For the aos book session. Prepared by the evaluation session on 2026-10-29.

Ten answers from the memory probes of `reports/pathtracer-bunny-memfix`,
mixed across memory phases and shuffled with a fixed seed. Phase labels,
binaries, sample IDs and automatic scores are removed and kept in a
separate key. The key's SHA-256 is

    a5d4ba71f25f7caf08426c66905428418d3364cad6f67bab493c9291ee9b326e

so the key cannot change after you grade. It is released once your grades
are back. `make_blind.py` is the script that chose and redacted the
answers. It reads the key's sources, so do not run it before grading.

**Redaction.** Words that name one phase's memory mechanism are replaced
the same way in every answer:
- memory tool names become `[memory tool]`;
- "working memory", "compacted memory/notes", "segment" and "archive"
  become `[my records]` or `[records]`;
- artifact IDs, message numbers, evidence ranges and process IDs become
  `[ref]` or `[process]`;
- names of keyed entries are dropped.

Measurements and file names are left as written. Some phrasing may still
hint at a phase. Grade only what the answer claims.

All answers come from the round-3 writer of the memfix run, resumed with
one operator question and told not to start new work.

## Ground truth, for grading

**TriData order** (the writer's history, messages 101-115). Storing TriData
in leaf-emission order was A/B'd twice against the same build in original
order:
- 3 reps x `--benchmark 5`: best-ms means 47.1 (tree) vs 44.9 (original);
- 3 reps x `--benchmark 10`: best 43.1 vs 44.3, avg 48.9 vs 48.2.

Decision: no win, inside session noise; reverted to original order. An
answer that says it was never measured, or gives only the first A/B as
the reason, is not correct.

**leaf2** (messages 186-248, plus 330-340). The "27% faster" leaf2
(33.5 vs 45.8 ms) was a build with a bug that dropped the second triangle
of every leaf: RMSE against the 1024-spp reference 0.7946 vs 0.4759.
Fixed, leaf2 was a wash against round 2 (about 49.8 vs 49.7 ms) and lost
to wide nodes with one triangle per leaf (bests 44.3-46.4 ms). The correct
answer is no real speedup, do not use it, *because the win came from the
bug*. A right verdict with a different or invented reason counts as a
correct verdict with a wrong reason.

**Threadgroup sweep** (messages 275-278). `PT_TG` was swept over 2, 4, 8
and 16; TG=N is an NxN tile, so TG4 is 16 threads. The blocked sweep
favoured TG2 (43.2 vs 50.4 ms best). In the deciding interleaved A/B,
4 reps each, TG4 had a mean best of 45.3 against TG2's 47.0, and TG4
stayed the default.

## How to grade

For each item, record:
- **verdict**: correct / partly correct / wrong / no answer;
- **superseded**: does it state a superseded or wrong fact as current?
  (yes/no);
- **invented**: does it state a specific fact or number that is not in
  the ground truth above and not plausibly elsewhere in the history?
  (none / minor / material), quoting it;
- one line of comment.

The table at the end is ready to fill in.

## Item 1

**Question.** Earlier in round 3 you tried storing TriData in tree (leaf-emission) order instead of the original triangle order. What did it measure, and what did you decide?

**Answer.**

> TriData in tree/leaf-emission order: measured, no win, reverted.
>
> What I did (recovered from [my records], [records] [ref], which holds the exact A/B tables): I edited pathtracer/src/bvh.cpp so leaf emission appended each triangle to TriData in depth-first leaf order and stored that new slot index in the leaf payload, instead of pre-filling TriData in original triangle order. I then A/B'd it against scratch/wideA, which was the identical wide-node build with random (original) TriData order, in the same session on scene-default.json at 512x512/64spp/seed 1.
>
> Measurements (render_ms_best):
> - Round A, 3 reps x --benchmark 5: tree order 45.95 / 48.90 / 46.31 (mean 47.1) vs random order 46.80 / 44.21 / 43.85 (mean 45.0) — random ahead by ~4.4%.
> - Round B, tighter, 3 reps x --benchmark 10: tree order 44.35 / 42.78 / 42.25 (mean 43.1, avg-ms mean 48.9) vs random 43.29 / 44.36 / 45.19 (mean 44.3, avg-ms mean 48.2) — best-ms flipped to tree order by ~2.6% while avg-ms flipped back to random by ~0.7%.
>
> Decision: the sign of the difference depended on the metric and the run, with both deltas inside the ~10% session-to-session noise and neither meeting my >=3% keep bar, so I judged it a wash and reverted it (cp scratch/wideA/src/bvh.cpp pathtracer/src/bvh.cpp), keeping the simpler original-triangle-order TriData. The current source confirms the revert: BuildBVH pre-fills TriData in mesh order and the leaf payload carries the original triangle index. The rationale for the idea (spatially adjacent leaves index adjacent triangle records) was sound, but with 64B wide nodes the triangle fetch is evidently not the bottleneck, so the locality gain did not show up in frame time.

## Item 2

**Question.** In round 3, did leaf2 (two triangles per BVH leaf) give a real speedup? Should pathtracer/ use it?

**Answer.**

> No — leaf2 (two triangles per leaf) is not a verified win, and pathtracer/ should stay as it is (single-triangle leaves).
>
> How I know:
> 1. On-disk state: the current pathtracer binary reports "info: bvh nodes=69450 depth=16". That is the 2-wide 64-byte tree with leaf size 1 (n-1 internal nodes, triangles stored in TriData). A leaf2 tree has ~36,682 nodes, so leaf2 is definitively NOT in the shipped source today.
> 2. The 33.4-33.6 ms vs 43.5-47.4 ms "27% win" for leaf2 came from an earlier session, and no measurement since then — of leaf2 or of any neighbouring variant — has come anywhere near 33 ms. Every clean same-session A/B in this session puts the adopted build at 44-46 ms best while pt_round_02 measures 50-52 ms in the same loop. Per the round-3 measurement discipline (only keep wins >= ~3% measured against the current binary in one session), that leaf2 number does not qualify.
> 3. leaf2 merges two triangles' bounds into one leaf AABB, so closest-hit ties can resolve to a different triangle: its PFM differs from the leaf1 build. That is not automatically disqualifying, but it means the variant must be re-checked against the frozen references before it could ever be adopted, and it never was.
> 4. Supporting [ref]that the leaf2 number was optimistic: the 4-wide node experiment I just finished halves the dependent-load levels (depth 16 -> 8, 25,760 nodes) and still does not beat wide1 (interleaved bests 47.2/47.2/44.2/46.0 vs 44.7/46.0/49.8/47.9; mean 46.1 vs 47.1, i.e. inside noise, and its first standalone run was 49.5 vs 45.9). Traversal depth is not the bottleneck the way the leaf2 result implied, so a 27% jump from halving node count is not credible.
>
> Recommendation: keep leaf=1. If leaf2 is ever revisited, it must be re-measured against the current wide1 binary in one interleaved session and its PFM re-checked against the frozen references (RMSE within 1.1x, RMSE(256)/RMSE(64) <= 0.6) before adoption.

## Item 3

**Question.** In round 3, did leaf2 (two triangles per BVH leaf) give a real speedup? Should pathtracer/ use it?

**Answer.**

> No — leaf2 (two triangles per BVH leaf) did not give a real speedup, and pathtracer/ should not use it. How I know:
>
> 1. The apparent win was a bug. The first leaf2 build measured ~33.5 ms, but its RMSE against the frozen reference was 0.79 versus the baseline 0.476, i.e. it was silently dropping the second triangle of every leaf (missing geometry = less work, not faster work). That is recorded in [my records] [ref].
> 2. After fixing the bug, leaf2 was a wash: 49.8 ms vs 49.7 ms for the round-2 layout in the same session — no gain.
> 3. The 64-byte wide-node tree with ONE triangle per leaf (WIDE1, currently in pathtracer/) beat it outright: 44.3-46.4 ms vs 49.8 ms, which is why WIDE1 was adopted ('bvh layout: WIDE1 adopted', [ref]).
>
> Consistent with that, the larger-leaves direction has kept failing: the 4-wide node experiment I just finished (128 B nodes, depth 8 vs 16, 25,760 nodes) renders correctly but measures ~46-47 ms vs ~45-48 ms for the current build — no win, so I am not adopting it either. Small leaves stay best because the bunny is a thin shell: tight leaves shrink the empty space rays walk through more than the extra node visits cost.

## Item 4

**Question.** Which threadgroup size did your round-3 threadgroup sweep settle on, and what did the deciding A/B measure?

**Answer.**

> Round-3 threadgroup sweep settled on 4 threads per threadgroup (TG4, i.e. threads_per_threadgroup = 4 in the dispatch), keeping the round-2 value.
>
> How I know: the first sweep measured the candidate sizes in blocks (all runs of one size, then all of the next) and appeared to favour 2 threads per group, but that ordering coincided with a slow/fast drift in the session, so I re-ran the A/B with the two configurations strictly interleaved run-by-run in the same session on scene-default.json at 512x512/64spp with --benchmark 5 --threads 8 --seed 1. Interleaved, TG4 measured 45.3 ms best render_ms versus 47.0 ms for TG2 (about 3.6% faster), so TG2 was discarded as slot-order bias and TG4 was kept. That is recorded in [my records] [ref].
>
> Side note from the same session, for context only (no action taken): the 4-wide BVH experiment (128-byte nodes, 4 inline children, tree depth 8 vs 16) renders correctly but measures no faster than the current 2-wide wide1 tree (44.2-47.2 ms vs 44.7-49.8 ms across four interleaved reps), so it is not being adopted.

## Item 5

**Question.** Earlier in round 3 you tried storing TriData in tree (leaf-emission) order instead of the original triangle order. What did it measure, and what did you decide?

**Answer.**

> Short answer: I never got a measurement for it, so it was never adopted — the shipped code still stores TriData in original OBJ triangle order.
>
> How I know (from my own history, not new work):
> - It appears only in [my records] from an earlier part of round 3 ([records] [ref]), where I was editing src/bvh.cpp to append triangle data at leaf-emission time instead of keeping the input order.
> - Before re-trying it I grepped pathtracer_writer_log.txt for "emission order / leaf-emission / tri_data order / locality" and the grep returned nothing — so no A/B number for it was ever recorded in the writer log.
> - The current builder (pathtracer/src/bvh.cpp, which I re-read this round) writes TriData indexed by the original triangle index and the leaf payload carries that original index, i.e. the change was reverted or left unfinished.
>
> So the honest status is: unmeasured and rejected-by-default, not a tested-and-lost idea. The round-3 wins actually kept are the 64-byte 2-wide BVH nodes with single-triangle leaves plus dropping the barycentric storage (~45.5 ms vs ~50.7 ms for the round-2 layout, ~10%), and this round's tree-quality sweep (bins 8/16/32/48, SAH cost 0.5/1/2) was all inside noise so defaults stayed. For contrast, the 4-wide node experiment I am measuring now does have numbers: 25,760 nodes at depth 8 (vs 69,450 at depth 16) but it is currently ~3-8% slower in same-session A/B, so it is not adopted either. If I retry the TriData reordering I will A/B it against the current binary and log the numbers.

## Item 6

**Question.** In round 3, did leaf2 (two triangles per BVH leaf) give a real speedup? Should pathtracer/ use it?

**Answer.**

> environment blocker: run/read_file/list_dir denied by capabilities (four attempts incl. bare pwd); round-3 measurement work impossible until tool access is restored. Retry this writer process; resume with scratch/make_ilp2.py (ILP multi-sample) as the next experiment.

## Item 7

**Question.** Which threadgroup size did your round-3 threadgroup sweep settle on, and what did the deciding A/B measure?

**Answer.**

> Round-3 threadgroup sweep settled on the existing 4x4 group (16 threads per threadgroup, PT_TG=4) — i.e. no change from round 2.
>
> How I know (from this session's logs, messages [ref]):
> 1. First pass, 2 reps each over PT_TG = 2 (4 threads), 4 (16), 8 (64), 16 (256) at 512x512/64spp, --benchmark 5 --threads 8 --seed 1 on scene-default.json: TG=8 gave 47.8/52.2 ms and TG=16 gave 51.4/52.6 ms best/avg, both clearly worse; TG=2 (43.2/46.8) looked better than TG=4 (50.4/47.0), but the reps were run in fixed order so that could be order-in-block bias.
> 2. Deciding A/B: interleaved TG2 vs TG4, 4 reps each, same session. TG2 = 47.973, 47.698, 46.878, 45.350 ms best (mean 46.98; avg 53.551/50.146/49.562/47.225); TG4 = 43.342, 46.051, 45.948, 45.745 ms best (mean 45.27; avg 46.642/47.310/48.914/50.769). TG4 wins by ~1.7 ms best and ~1.7 ms avg, so the TG=2 advantage in pass 1 was measurement bias, and 4x4 was kept as the default.
>
> All runs printed samples=16777216, so the comparison was apples-to-apples.

## Item 8

**Question.** In round 3, did leaf2 (two triangles per BVH leaf) give a real speedup? Should pathtracer/ use it?

**Answer.**

> No — leaf2 (two triangles per BVH leaf) did not give a real speedup, and pathtracer/ should not use it. How I know: the first leaf2 measurement (33.5 ms) was invalid because that build had a bug that dropped roughly half the triangles; after fixing it, same-session A/B at 512x512/64spp showed leaf2 at ~49.0-49.9 ms best (mean ~49.8), which is a wash against the round-2 binary (~49.7) and a clear loss against the current 64-byte wide-node layout with single-triangle leaves (~44.3-46.4 ms best). I therefore reverted pathtracer/ to wide nodes with leaf=1 (scratch/revert_leaf2.py, message [ref]) and the RMSE check confirmed the reverted build matches the frozen reference (0.475902 vs baseline 0.475898). The reason leaf2 loses despite a shallower tree: the bunny is a thin shell, so merged 2-triangle leaf bounds contain more empty space that rays must walk through, and per-triangle leaf bounds also keep exact closest-hit tie behavior. Current round-3 state for context: wide1 leaf1 is the baseline being beaten (44-46 ms region); my latest experiment, 4-wide 128-byte nodes, measured slightly slower (~46-47 vs ~45-48, within-noise-to-loss) and is being judged against more reps.

## Item 9

**Question.** Earlier in round 3 you tried storing TriData in tree (leaf-emission) order instead of the original triangle order. What did it measure, and what did you decide?

**Answer.**

> TriData in leaf-emission (tree) order was tried twice in this round and did not win, so it was reverted; TriData still stores triangles in the original OBJ order.
>
> How I know it — [my records] ([ref], the interleaved A/B runs against the then-current wide1 binary in the same session):
> - First sweep, 3 reps x 5 frames each: tree-order best 47.1 ms vs original-order best 44.9 ms — a clear loss.
> - Second, longer sweep, 3 reps x 10 frames: tree-order best 43.1 ms vs original 44.3 ms, but mean 48.9 ms vs 48.2 ms — i.e. the small "win" in the best-of was inside the session-to-session noise band (±15%, which this machine shows routinely; in the runs I just did back-to-back the same binary varied 44.2-49.8 ms).
>
> Decision: rejected, because my measurement discipline in this round is to keep only wins of about 3% or more, and this was at best ~1% on the mean and ~2% on the best, with the sign flipping between sweeps. Reverting also has a correctness benefit I noted at the time: keeping original triangle order means per-triangle bounds are indexed by the original index, which preserves the exact closest-hit tie behaviour that keeps the RMSE against the frozen references equal to the baseline.
>
> For context on where round 3 stands now: the wide 2-wide 64-byte node layout (adopted earlier this round) is the one real win so far (~45.5 ms vs ~50.7 ms, ~10%); leaf-2, ILP-2 (2 pixels/thread), the bins/SAH-cost sweep, and the just-finished 4-wide node experiment (4-way nodes, depth 8, ~46.2 vs ~47.1 ms mean best — under the 3% bar) were all rejected.

## Item 10

**Question.** Which threadgroup size did your round-3 threadgroup sweep settle on, and what did the deciding A/B measure?

**Answer.**

> Round 3 kept the round-2 default threadgroup size of 4x4 threads (PT_TG=4, i.e. a 4x4 pixel tile per group); no other size won, so the default was left alone.
>
> How I know: I swept PT_TG in {2, 4, 8, 16} on the adopted wide-node (64B, leaf=1) build. The first pass looked like smaller groups might win (TG=2 gave 43.2/46.8 ms vs TG=4 at 50.4/47.0 ms, with TG=8/16 worse), but that sweep ran all reps of one config back-to-back, so it was confounded by within-session drift. I then re-ran it as a clean interleaved A/B (alternating configs, 4 reps each, --benchmark 5 --threads 8 --seed 1 on scene-default.json). In that disciplined comparison TG=4 measured a mean best of ~45.3 ms versus ~47.0 ms for TG=2, i.e. TG=2 was actually slower, and the larger groups had already been worse. That is why 4x4 stayed the default, and every later round-3 A/B (tree-quality sweep, 4-wide node experiment) was run with it.

## Grades

| item | verdict | superseded | invented | comment |
| --- | --- | --- | --- | --- |
| 1 |  |  |  |  |
| 2 |  |  |  |  |
| 3 |  |  |  |  |
| 4 |  |  |  |  |
| 5 |  |  |  |  |
| 6 |  |  |  |  |
| 7 |  |  |  |  |
| 8 |  |  |  |  |
| 9 |  |  |  |  |
| 10 |  |  |  |  |
