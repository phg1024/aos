# Negative-claim probe selection log

Method: `WRITER.md`. Candidates were taken in the seeded order of
`negative-candidates.json`. Each was checked against
`context/<checkpoint>.txt` (working memory and live history) with
case-insensitive fixed-string searches; the hit counts are given as
`string`=N. A segment stopped at its first acceptance. Rounds come from the
checkpoints: art-000648 is round 2, art-001768 is round 5, art-003135 is
round 8.

Existing probes compared under (c): `bench/memory/pathtracer-bunny-phase3-own-memory-probes.json`
and `reports/memory-probe-recall-header/probes-new.json`.

## art-000414 (checkpoint art-000648, round 2): 1 candidate, 0 accepted

1. #46 `mkdir -p .../out && echo OK` printed `OK`. **Reject (b):** this is
   only a confirmation and has no outcome. (`mkdir`=0, `echo OK`=0.)

## art-000507 (checkpoint art-000648): 1 candidate, 0 accepted

1. #89: the full clang++ rebuild after restoring `transform_mesh` printed
   `BUILD_DONE` with no error lines. **Reject (c) and (a):** this is the fix
   for existing probe h-507-1, attempt 1's `transform_mesh` link failure. Its
   outcome, that attempt 1 built, also follows from working memory
   round2-attempt1, which records attempt 1 benchmarked at 4416 ms (#93).
   Also, `BUILD_DONE` follows `| head` and so prints unconditionally.
   (`transform_mesh`=0, `BUILD_DONE`=0, `attempt 1`=2, `4416`=2.)

## art-000557 (checkpoint art-000648): 5 candidates, 0 accepted

1. #173: `cp -R pathtracer out/pt_snapshot_pre_stackless && ls`. **Reject
   (b) and (a):** the output is a plain listing. Working memory
   round2-progress records the snapshot (#173).
   (`pt_snapshot_pre_stackless`=2, `ls out/pt_snapshot`=0.)
2. #107: `ls` of the phase3 folder and of `pt_round_01/src`. **Reject (b):**
   this is a directory listing whose only content is file names. The use of
   the round-1 median-split builder that came out of it is already in working
   memory round2-progress (a) ("replaced with round-1
   median-split-on-widest-axis").
   (`pt_round_01`=0, `ls /Users`=0, `round-1 median`=1, `median-split`=2.)
3. #121: rebuild with the median-split tree and a 5-frame benchmark,
   render_ms_best=103.290. **Reject (a):** working memory round2-progress (a)
   gives "101.8-103.4ms best (#119,#121,#129)".
   (`103.290`=0, `103.3`=0, `113.313`=0, `102.951`=0. The action and its
   range are in memory.)
4. #119: bvh_stats reported nodes=40597, max_leaf=4, max_depth=15. **Reject
   (a):** this is in working memory round2-progress (a) ("40597 nodes, max
   leaf 4, depth 15"). (`40597`=2, `max_leaf`=2.)
5. #129: rebuild with stack 20 / cap 16, render_ms_best=101.771. **Reject
   (a):** this is in working memory round2-progress (a) and (b) (101.8, #129).
   (`101.771`=0, `114.621`=0, `101.237`=0. The action is in memory.)

## art-000646 (checkpoint art-000648): 2 candidates, 0 accepted

1. #204: an spp-2 smoke test with a 40 s alarm printed only `ls -l` and
   `EXIT=1`. **Reject (b) and (c):** EXIT=1 was grep's status, and the agent
   called it "inconclusive" in #205, so there is no definite outcome. The
   episode (stale binary, stackless hang) is existing probe h-646-1, and
   working memory round2-progress2 (h) records the hang.
   (`smoke`=0, `EXIT=1`=0, `alarm 40`=0, `spp 2`=0, `stale`=1, `hang`=42.)
2. #212: a clean rebuild after making `link_escapes` public, then an spp-2
   smoke test. It printed PROG_EXIT=0 and smoke.ppm was written. **Reject (c)
   and (a):** this is the same stale-binary episode as h-646-1. Working
   memory round2-progress2 (f) records that the fixed stackless build ran and
   benchmarked 102.1 ms (#214), so its success is already stated.
   (`PROG_EXIT`=0, `smoke.ppm`=0, `stale`=1.)

## art-001608 (checkpoint art-001768, round 5): 1 of 2 considered, 1 accepted

1. #18: `ls ab_r3`, `which metal`, a Metal.framework Headers check, then a
   rebuild of the live `pathtracer/` from the unchanged round-4 sources
   (`clang++ ... && echo BUILD_OK`). The note shows that the rebuild is the
   action ("rebuild live dir"; #19: "Live dir rebuilt OK"). Outcome: pass,
   `BUILD_OK`, with one warning, "'fastMathEnabled' is deprecated ... Use
   mathMode instead".
   - (a) Working memory round5-refs gives the build command but not this
     rebuild. Hits: `BUILD_OK`=0, `fastMathEnabled`=0, `deprecat`=0,
     `mathMode`=0, `ab_r3`=0, `SOURCES_IDENTICAL`=0, `which metal`=0,
     `md5`=0, `rebuilt`=0. `rebuild`=6, all at later reverts or experiments
     (#191, #193, #197, #199, #242, #246). `1 warning`=1, at #249, a
     different occupancy-diag build that printed "1 warning and 1 error".
   - (b) Pass (BUILD_OK) plus the specific warning.
   - (c) No existing probe covers this rebuild. h-1608-1 to h-1608-3 cover
     the reference renders and the writer log. r5-register-counts covers the
     missing offline metal compiler, so the `which metal` part is
     deliberately left out of the question. h-557-2 (fastMath benchmark,
     round 2) is a different event.
   - (d) "unchanged round-4 sources, before making any edit of your own"
     fits only #18.
   - **ACCEPT, n-1608-1.** #20 was not considered.

## art-001661 (checkpoint art-001768): 8 of 15 considered, 1 accepted

1. #66: glass micro-opts build, all 3 scenes BIT_IDENTICAL. **Reject (a):**
   working memory round5-glass-and-stack says "bit-identical on 3 scenes".
   (`out/c1_`=0, `bit-identical`=25.)
2. #114: `sed -n 1,140p include/pt_gpu_types.hpp`. **Reject (b):** the
   output is only file contents. (`sed -n 1,140p`=0.)
3. #86: entered-stack build and bit-identity check, 3 scenes BIT_IDENTICAL.
   **Reject (a):** working memory round5-glass-and-stack says "KEPT:
   entered-distance stack ... Bit-identical on 3 scenes".
   (`out/c2_`=0, `197.2`=0.)
4. #72: the ab_r4 base with stderr discarded printed no benchmark line
   (EXIT=0). **Reject (a) and (c):** working memory round5-refs records "a
   silent shader-compile failure of the base — always verify the base prints
   a benchmark line", and existing probe r5-empty-base covers it.
   (`silent shader-compile`=1, `benchmark line`=1.)
5. #118: snapshot to `_b` and build, printed `BUILT _b`. **Reject (b) and
   (a):** this is a confirmation only, and working memory round5-stack-payload
   records the `_b` snapshot. (`BUILT _b`=0, `_b/pathtracer`=24.)
6. #70: the ab_r4 base printed a `pt_v3_16` shader diagnostic. **Reject (c)
   and (a):** this is the same empty-base episode as #72 and r5-empty-base.
   (`pt_v3_16`=0, `silent shader-compile`=1.)
7. #98: build of the entered-only base `_e`, printed `0` and `BUILT`.
   **Reject (b):** confirmation only. Working memory round5-stack-payload
   names the `_e` base. (`_e`=20.)
8. #38: `grep -n "params.pad\|params.seed =\|getenv" src/pt_main.cpp` in
   `_prof/`. Outcome: no matches (empty stdout). The setting lives in
   pt_scene.cpp:206 (#36), where #41 added the PT_PROF getenv.
   - (a) Hits: `getenv`=0, `params.seed`=0, `pt_scene.cpp:206`=0.
     `params.pad`=1 and `PT_PROF`=2, all in working memory
     round5-cost-shares, which describes the bitmask and not this search.
     `pt_main.cpp`=4: plane-filter edit #219 (×3) and a #243 file listing.
     `pt_scene.cpp`=1: the #243 listing. `_prof`=3: memory only.
   - (b) Not found (empty grep).
   - (c) Differs from h-1661-1, the shader compile error of the PT_PROF
     guard, and from h-1661-2/3 (PT_PROF=3 timing, glass gpu_min). This is a
     host-side search.
   - (d) The file, folder and three patterns fit only #38.
   - **ACCEPT, n-1661-1.** Candidates 9-15 were not considered.

## art-001766 (checkpoint art-001768): 8 of 9 considered, 1 accepted

1. #122: `sed` of pt_metal_host.mm. **Reject (b):** file contents.
   (`pt_metal_host.mm`=14, all unrelated.)
2. #126: `sed` of pt_main.cpp 138-195 plus a grep of `GpuMeshData`.
   **Reject (b):** file contents and a reference listing.
   (`sed -n 138,195p`=0.)
3. #136: grep of `quantize_bvh` / `sizeof(PtBvhNodeQ)` uses. **Reject (b):**
   it lists where references are, with no finding. (`sizeof(PtBvh`=0.)
4. #134: a python deletion of the PtBvhNodeQ lines, then `sed`. **Reject
   (b) and (c):** this is a patch confirmation plus file contents. The
   deletion is what caused the existing probe h-1766-1's 'unknown type name
   PtBvhNodeQ' error. (`Kept for the host`=0, `PtBvhNodeQ`=1.)
5. #146: printed `walks rewritten`. **Reject (b):** patch confirmation.
   (`walks rewritten`=0.)
6. #138: printed `rewritten` plus one grep line. **Reject (b):** patch
   confirmation.
7. #132: grep/sed showing that PtBvhChild is at line 56 and PtBvhNodeQ still
   at lines 66-75. **Reject (b) and (c):** the output is file contents, and
   PtBvhNodeQ is the subject of h-1766-1. (`PtBvhNodeQ`=1, live history
   only, as current reverted code.)
8. #140: `grep -rn 'kInvalidNode' ...; grep -n 'constexpr\|static const'
   src/pt_bvh.cpp`. Outcome: the only hit was the use at
   `src/pt_bvh.cpp:399`, and the only constexpr was `kInfinity`, so
   kInvalidNode was not defined. #141 says "uses undefined kInvalidNode" and
   adds it.
   - (a) Hits: `kInvalidNode`=0, `kInvalid`=0, `invalid`=0, `kInfinity`=0,
     `constexpr`=0, `undefined`=0, `undeclared`=0, `not defined`=0,
     `0xFFFFFFFF`=0, `:399`=0. `quantize_bvh`=3, all in the round5-childpair
     memory entry and its #252 update, which do not mention the constant.
   - (b) Not found (no definition).
   - (c) h-1766-1 asks about the compile errors of the first child-pair
     build (PtBvhNodeQ type and pointer type). This constant was caught
     before that build, so it is a different fact. h-1766-2/3 cover the scene
     path and setup_ms.
   - (d) The question names the constant and the quantize_bvh rewrite.
   - **ACCEPT, n-1766-1.** #144 was not considered.

## art-003062 (checkpoint art-003135, round 8): 3 candidates, 0 accepted

1. #15: `sed -n '1,200p' pt_trace.metal`. **Reject (b):** file contents.
2. #17: `sed -n '200,330p' pt_trace.metal`. **Reject (b):** file contents.
3. #13: `cat pt_gpu_types.hpp` and the host .mm. **Reject (b):** file
   contents.

## art-003133 (checkpoint art-003135): 9 candidates, 0 accepted

1. #23: the `_r8p` PT_SPLIT patch printed `patched`. **Reject (b):** patch
   confirmation. (`PT_SPLIT`=2, `_r8p`=2: the split profile is in working
   memory r8-cost-split.)
2. #42: grep of `PtBvhNodeQ` / `GpuMeshData` uses. **Reject (b):** a
   reference listing with no finding. The stale-comment finding came from
   earlier reads. (`GpuMeshData`=0, `PtBvhNodeQ`=0.)
3. #56: wrote `_r8_abenv.sh` and a PT_DEBUG run, which printed
   `max_threads_per_group trace=1024 sample_parallel=1`. **Reject (a) and
   (c):** working memory records "E4 occupancy (maxTotalThreadsPerThreadgroup
   ... = 1024, unchanged from round 5 ...)", and the occupancy figure is the
   topic of r5-register-counts.
   (`1024`=2, `maxTotalThreads`=2, `occupancy`=4,
   `max_threads_per_group`=0.)
4. #19: `cat` of the `_r7_ab.sh` and `_r7_pair.pl` scripts, the scenes, and
   `which python3 perl`. **Reject (b):** file contents.
   (`_r7_ab.sh`=0, `_r7_pair`=0.)
5. #60: printed `integrator variants ok`. **Reject (b):** patch confirmation.
6. #21: frozen `_r8b` baseline build, BUILD_OK with the fastMathEnabled
   warning. **Reject (a) and (c):** working memory r8-baseline-and-harness
   names `_r8b/` as the frozen A/B binary used throughout, which implies the
   build succeeded. The fact (a clean build with the fastMathEnabled warning)
   is also the one already accepted as n-1608-1. (`_r8b`=12, `BUILD_OK`=0,
   `fastMath`=0, `warning`=0.)
7. #34: printed `diag patched` plus a grep line. **Reject (b):** patch
   confirmation. (`PT_DIAG_NOSHADOW`=1.)
8. #58: printed `host hook ok`. **Reject (b):** patch confirmation.
   (`PT_DEFINE`=62.)
9. #48: printed `trace patched` / `integrator patched 3` and a grep listing.
   **Reject (b):** patch confirmation.

## Totals

| segment | checkpoint | candidates | considered | accepted |
|---|---|---|---|---|
| art-000414 | art-000648 | 1 | 1 | 0 |
| art-000507 | art-000648 | 1 | 1 | 0 |
| art-000557 | art-000648 | 5 | 5 | 0 |
| art-000646 | art-000648 | 2 | 2 | 0 |
| art-001608 | art-001768 | 2 | 1 | 1 (n-1608-1) |
| art-001661 | art-001768 | 15 | 8 | 1 (n-1661-1) |
| art-001766 | art-001768 | 9 | 8 | 1 (n-1766-1) |
| art-003062 | art-003135 | 3 | 3 | 0 |
| art-003133 | art-003135 | 9 | 9 | 0 |
| **total** | | **47** | **38** | **3** |

The regexes were compiled with Go's `regexp` (RE2). For each probe, the
rubric's main and exact strings match `topic` and `expect` and do not match
`reject`. "No, I never did that." and "No." match `reject`. Answers that
begin "No matches" or "It wasn't defined" are not rejected.
