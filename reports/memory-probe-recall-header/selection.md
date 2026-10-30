# New unrecorded-fact probes: candidate-by-candidate selection

Candidates were taken strictly in `candidates.json` order. For each, one fact
was picked (the most decision-relevant measured number or outcome the message
states). The fact was accepted only if (a) absent from `context/<checkpoint>.txt`,
(b) not an existing probe's topic, (c) distinct from facts already accepted
in the segment, and (d) askable without giving the answer. Absence was checked
with case-insensitive fixed-string search (and a regex context dump) over the
whole context file, working memory plus live history.

A message whose output is only source code, a config file, a file listing,
or a "patched"/"rewritten" confirmation states no measured number or outcome;
those are rejected as "no measured fact" (the decimal numbers they hold are
code constants, which is why `select_candidates.py` let them through).

Totals: 11 accepted from 79 considered.

| segment | checkpoint | accepted / considered |
|---|---|---|
| art-000414 | art-000648 | 2 / 13 |
| art-000507 | art-000648 | 2 / 2 |
| art-000557 | art-000648 | 0 / 10 |
| art-000646 | art-000648 | 1 / 10 |
| art-001608 | art-001768 | 2 / 5 |
| art-001661 | art-001768 | 1 / 14 |
| art-001766 | art-001768 | 2 / 9 |
| art-003062 | art-003135 | 1 / 6 |
| art-003133 | art-003135 | 0 / 10 |

## art-000414 (checkpoint art-000648, working memory art-000643)

1. #13: rejected. Source code (pt_trace.metal), no measured fact.
2. #21: rejected. Source code (round-1 pt_bvh.cpp), no measured fact.
3. #43: rejected. Fact: local round-1 rebuild benchmark render_ms_best=105.947. Present in WM measurements/baseline-timing (grep 105.947: 1 hit).
4. #56: **accepted, h-414-1**. Fact: running the copied round-1 binary from out/ failed with 'metal init error: cannot read shared shader header' (no r1_default.pfm). Grepped: 'shared shader header', 'shader header', 'cannot read', 'init error', pathtracer_r1, r1_default: all 0.
5. #25: rejected. Source code (pt_main.cpp), no measured fact.
6. #27: rejected. Fact: official round-1 gate render_ms_best 109.656 / gpu 109.397 (pathtracer_benchmark.json). Present in WM baseline-timing and round2-progress (grep 109.656: 5 hits).
7. #15: rejected. Source code (pt_integrator.metal), no measured fact.
8. #17: rejected. Source header (pt_gpu_types.hpp: stack 64, 40-byte node), constants not measurements.
9. #29: rejected. Scene config JSON, no measured fact.
10. #31: **accepted, h-414-2**. Fact (round-1 writer log): caustic mean luminance 9.05 over 1185 px, about 16x the floor median 0.56. Grepped: 9.05, 1185, 0.56, luminance, 3.66, 3499, 115.080: all 0; '16x' hits are threadgroup shapes only (16x8 etc.); 'caustic' hits are the shadow-bug note and the Fresnel code comment, neither states the measurement.
11. #54: rejected. Source code (pt_tonemap.metal), no measured fact.
12. #33: rejected. Source header (pt_mesh.hpp), no measured fact.
13. #23: rejected. Source code (pt_metal_host.mm), no measured fact.

Candidates exhausted at 2 accepted.

## art-000507 (checkpoint art-000648)

1. #83: **accepted, h-507-1**. Fact: link failed, undefined symbol pt::transform_mesh. Grepped: transform_mesh, 'Undefined symbol', linker, 'ld:', 'link error': all 0.
2. #79: **accepted, h-507-2**. Fact: compile failed, 'use of undeclared identifier PtVertex' (pt_mesh.hpp, pt_bvh.cpp, pt_mesh.cpp). Grepped: PtVertex, undeclared, 'unknown type', pt_mesh.hpp: all 0. Distinct from h-507-1 (compile vs link stage, different cause).

Candidates exhausted at 2 accepted.

## art-000557 (checkpoint art-000648)

1. #115: rejected. Source code (binned-SAH pt_bvh.cpp), no measured fact.
2. #109: rejected. Source code (pt_round_01 pt_bvh.cpp), no measured fact.
3. #125: rejected. Fact: stack 16 / depth cap 15 gave 119.866 ms. In WM round2-progress (b) as '119.9' (grep 119.9: 1).
4. #133: rejected. Fact: fastMath 101.150 ms. In WM round2-progress (c) '101.15 vs 101.77' (grep 101.15: 1).
5. #167: rejected. Fact: threadgroup sweep, 16x8 fastest (96.4/90.5 range). In WM round2-progress (e) (grep 16x8: 10, 90.5: 2). The 8x8 value (101.026) is absent but is not the most decision-relevant fact of the message.
6. #121: rejected. Fact: median-split rebuild 103.290 ms. In WM round2-progress (a) '101.8-103.4' (grep 103.4: 1).
7. #119: rejected. Fact: bvh_stats 40597 nodes, max leaf 4, depth 15. In WM round2-progress (a) (grep 40597: 2, 'max leaf 4': 1, 'depth 15': 2).
8. #192: rejected. Source header (pt_gpu_types.hpp, stack 20 / depth 16), no measured fact.
9. #129: rejected. Fact: 101.771 ms. In WM round2-progress (a)/(c) as 101.77 (grep 101.77: 1).
10. #169: rejected. Fact: second threadgroup sweep, 16x8 best at 90.498. In WM round2-progress (e) (16x8=90.5, 16x4=91.4, 4x32=93.3, 8x16=94.4).

Candidates exhausted at 0 accepted.

## art-000646 (checkpoint art-000648)

1. #216: rejected. Fact: stack-based A/B 93.366 / 96.614 best, gpu 92.9. In WM round2-progress2 (f) 'stack-based 93.4/96.6 best gpu 92.9 (#214 vs #216)'.
2. #220: rejected. Source code (pt_integrator.metal), no measured fact.
3. #208: **accepted, h-646-1**. Fact: host build failed with "'link_escapes' is a private member of BvhBuilder". WM round2-progress2 (h) says only that a failed compile left a stale binary, not the error. Grepped: link_escapes, 'private member', private: all 0.
4. #214: rejected. Fact: stackless 102.084 ms. In WM (f) '102.1' (grep 102.1: 1).
5. #222: rejected. Source code (pt_metal_host.mm), no measured fact.
6. #212: rejected. Fact: clean rebuild, the 2-spp smoke run completed (PROG_EXIT=0, PPM written). Stated in other words in WM (h) (the hang was the stale binary) and (f) (the stackless build was then measured at 102.1). Grepped smoke: 0.
7. #218: rejected. Fact: snapshot restored, stack 20, BUILD_OK. In WM (f) 'reverted; stackless copy kept at pathtracer_backup_stackless and out/pt_snapshot_pre_stackless (#218)'.
8. #224: rejected. Source code (pt_tonemap.metal), no measured fact.
9. #198: rejected. Source code (pt_trace.metal), no measured fact.
10. #245: rejected. Fact: merged-tone-map smoke run passed (exit 0, samples=1048576). Implied by WM round2-final (merged tone map kept, 'All changes bit-identical PFM'). setup_ms=180.8 is not decision-relevant. Grepped 1048576, 180.8: 0.

Candidates exhausted at 1 accepted.

## art-001608 (checkpoint art-001768, working memory art-001763)

1. #27: rejected. Fact: round-4 reference PFMs rendered for the three scenes (bvh_nodes=81521). In WM round5-refs (reference PFMs out/r4_*.pfm); 81521 also appears 7 times in live history.
2. #24: **accepted, h-1608-1**. Fact: the first reference render failed with 'mesh error: cannot open mesh' (path without aos/reports/). Grepped: 'cannot open mesh', 'mesh error', 'Code/rasterizer-bunny', 'mesh path', md5: all 0 (WM holds only the correct path).
3. #5: **accepted, h-1608-2**. Fact (writer log tail): round 4 recorded render_ms_best=45.635 (1.074x over round 3's verified 49.020, 8/8 pairs vs 47.828). Grepped: 45.635, 47.828, 1.074, 2.403, 8/8, 'round 4 (': all 0; '45.6' occurs only as 45.607 (entered-stack mean).
4. #18: rejected. Fact: the live tree builds with the DEVELOPER_DIR command (BUILD_OK); the build command is recorded in WM round5-refs. Its other outcome, no 'metal' on PATH, is the topic of existing probe r5-register-counts.
5. #10: rejected. Source code (pt_trace.metal), no measured fact.

Candidates exhausted at 2 accepted.

## art-001661 (checkpoint art-001768)

1. #114: rejected. Source header printed with sed, no measured fact.
2. #96: rejected. Fact: stack_payload BIT_IDENTICAL on 3 scenes. In WM round5-stack-payload ('bit-identical 3 scenes').
3. #68: rejected. Fact: the A/B base printed no timings (render_mean=0.000). Topic of existing probe r5-empty-base.
4. #118: rejected. Fact: _b snapshot built. In WM round5-stack-payload ('Snapshot of that kept state = _b/pathtracer'); grep _b/pathtracer: 23.
5. #58: rejected. Fact: PT_PROF cost shares, closest-hit walk about 89%. In WM round5-cost-shares (45.35, 44.05, 43.17, 4.77, 35.76 all present).
6. #116: rejected. File listing and grep of quantize_bvh, no measured fact.
7. #98: rejected. Fact: _e (entered-only base) built. No measured result; _e as A/B base is in WM round5-stack-payload.
8. #78: rejected. Fact: glass refract-skip A/B base 43.315/45.632 vs cand 45.025/46.563. In WM round5-glass-and-stack.
9. #40: rejected. Source code (pt_scene.cpp via sed), no measured fact.
10. #86: rejected. Fact: entered-distance stack BIT_IDENTICAL on 3 scenes. In WM round5-glass-and-stack ('Bit-identical on 3 scenes').
11. #110: rejected. Fact: stack_payload A/B 43.875/45.583 vs 43.226/44.906. In WM round5-stack-payload.
12. #54: **accepted, h-1661-1**. Fact: the _prof build's shader failed to compile, "use of undeclared identifier 'params'". Grepped: undeclared, program_source, 'shader compile failed': 0; 'params.pad' 1 (WM's bitmask description only).
13. #120: rejected. Source code (pt_bvh.hpp / pt_bvh.cpp via sed), no measured fact.
14. #66: rejected. Fact: glass refract-skip BIT_IDENTICAL on 3 scenes. In WM round5-glass-and-stack ('bit-identical on 3 scenes but ...').

Candidates exhausted at 1 accepted.

## art-001766 (checkpoint art-001768)

1. #146: rejected. Code rewrite via a script; output only 'walks rewritten', no measured fact.
2. #153: **accepted, h-1766-1**. Fact: the first child-pair build failed: 'unknown type name PtBvhNodeQ' in pt_metal_host.hpp and 'cannot initialize ... PtBvhChild *' in pt_main.cpp. Grepped: 'cannot initialize', 'unknown type', 'did you mean': 0 (PtBvhNodeQ and GpuMeshData appear only as current code).
3. #163: rejected. Fact: child-pair default scene IDENTICAL_DEFAULT. In WM round5-childpair ('bit-identical on 3 scenes').
4. #138: rejected. Code rewrite (quantize_bvh); output only 'rewritten', no measured fact.
5. #124: rejected. Source code (pt_trace.metal), no measured fact.
6. #122: rejected. Source code (pt_metal_host.mm via sed), no measured fact.
7. #157: rejected. Fact: the fixed child-pair build compiled cleanly. Implied by WM round5-childpair (the full implementation ran and was bit-identical). Grepped 102200: 0.
8. #159: **accepted, h-1766-2**. Fact: a DIFF_DEFAULT was spurious because 'cannot open scene config: scenes/scene-default.json' (no PFM written). Grepped: 'scenes/', 'cannot open scene', 'scene error', DIFF_DEFAULT: all 0. Distinct from h-1766-1.
9. #126: rejected. Source code (pt_main.cpp via sed), no measured fact.

Candidates exhausted at 2 accepted.

## art-003062 (checkpoint art-003135, working memory art-003130)

1. #17: rejected. Source code (pt_trace.metal via sed), no measured fact.
2. #7: rejected. Fact: round 7 verified at 30.984 ms, 3.539x over the frozen baseline. In WM r8-baseline-and-harness (grep 30.984: 1, 3.539: 1).
3. #13: rejected. Source code (pt_gpu_types.hpp and pt_metal_host.mm), no measured fact.
4. #15: rejected. Source code (pt_trace.metal via sed), no measured fact.
5. #9: **accepted, h-3062-1**. Fact (round-7 writer log): raising PT_BVH_MAX_DEPTH 16 to 20 changed the alt-view PFM (byte 1022129), so tree topology is part of the frozen estimator. Grepped: 1022129, 123159, topology, MAX_DEPTH, tie-break, 'test order', 'tree quality', 'leaf size', 'deeper tree', 'frozen estimator', 27.1, 36.283: all 0.
6. #11: rejected. Source code (pt_integrator.metal), no measured fact.

Candidates exhausted at 1 accepted.

## art-003133 (checkpoint art-003135)

1. #56: rejected. Fact: max_threads_per_group trace=1024. In WM r8-rejected-e5-e3-e4 (E4 '= 1024'); also the topic of existing probe r5-register-counts.
2. #48: rejected. Shader patch for the 16-byte u16 node; output only 'patched', no measured fact (E5 is also the topic of r8-small-node).
3. #60: rejected. Scratch-variant patch; output only 'integrator variants ok', no measured fact.
4. #36: rejected. Diagnostic-hook patch; output only 'host diag patched', no measured fact.
5. #42: rejected. grep listing of PtBvhNodeQ/GpuMeshData, no measured fact.
6. #40: rejected. Source code (quantize_bvh via grep), no measured fact.
7. #38: rejected. Fact: depth sweep and no-shadow split (trace 30.329, noshadow 26.682, d1 5.818 ...). In WM r8-cost-split (30.30, 26.68, 5.82, 10.96, 17.10, 30.28 all present).
8. #23: rejected. PT_SPLIT patch; output only 'patched', no measured fact.
9. #21: rejected. Fact: _r8b frozen base built (BUILD_OK). In WM r8-baseline-and-harness ('_r8b/ (frozen A/B binary)').
10. #19: rejected. Harness scripts and scene configs, no measured fact.

Candidates exhausted at 0 accepted.

# Second pass

Rule: for each segment with fewer than 3 accepted, revisit its candidates in
`candidates.json` order, skipping messages already used for a probe. Accept at
most one SECONDARY fact per message: any measured or observed output (a
printed number, a build/run result, one check among several). The fact must
still pass (a)-(d). Source-code and config constants still do not count. Stop
a segment at 3 accepted in total.

Totals after the second pass: 23 accepted (11 first pass + 12 second pass).

| segment | first pass | second pass new / revisited | total |
|---|---|---|---|
| art-000414 | 2 | 1 / 3 | 3 |
| art-000507 | 2 | 0 / 0 (both candidates already used) | 2 |
| art-000557 | 0 | 3 / 5 | 3 |
| art-000646 | 1 | 2 / 9 | 3 |
| art-001608 | 2 | 1 / 1 | 3 |
| art-001661 | 1 | 2 / 8 | 3 |
| art-001766 | 2 | 1 / 2 | 3 |
| art-003062 | 1 | 1 / 5 | 2 |
| art-003133 | 0 | 1 / 10 | 1 |

## art-000414 (second pass)

1. #13: rejected. Source code only, no observed output.
2. #21: rejected. Source code only, no observed output.
3. #43: **accepted, h-414-3**. Secondary fact: 'ppm error: cannot open output: /tmp/base.ppm' (exit 1; the benchmark line still printed). The other secondary values (avg 113.523, gpu 105.574) are in WM baseline-timing. Grepped: /tmp, 'cannot open output', 'ppm error', base.ppm: all 0.
(#56 already used.) Segment reaches 3; stop.

## art-000507 (second pass)

No unused candidates (#83 and #79 are both probes). Stays at 2.

## art-000557 (second pass)

1. #115: rejected. Source code only.
2. #109: rejected. Source code only.
3. #125: **accepted, h-557-1**. Secondary fact: stack-16/cap-15 run render_ms_avg=126.379. Grepped: 126.379, 126.4, 119.359: 0. '126.' hits once, the root-exit 'avg 126.0', which is a different event (expect excludes 126.0).
4. #133: **accepted, h-557-2**. Secondary fact: fastMath run gpu_ms_best=100.887. The deprecation warning is already in WM (c) ('still enabled but deprecated'), so I took the gpu number. Grepped: 100.887, 116.465, 116.5: 0.
5. #167: **accepted, h-557-3**. Secondary fact: 8x8 threadgroup render_ms_best=101.026 (WM lists every other shape except 8x8). Grepped: 8x8, 101.026, '101.0', 100.761: 0.
Segment reaches 3; stop.

## art-000646 (second pass)

1. #216: **accepted, h-646-2**. Secondary fact: the two snapshot runs' render_ms_avg 103.183 and 100.692 (the best times and gpu are in WM (f)). Grepped: 103.183, 100.692, 103.2, 100.7: 0.
2. #220: rejected. Source code only.
3. #214: rejected. Secondary fact: stackless gpu_ms_best=101.802. Fails the semantic check: WM (f) gives the same run as '102.1 best / 114.6 avg', and 101.8 vs 102.1 is the same timing to within noise. A grader could not separate a right answer from a recalled WM number (101.8 also appears in WM (a) as an unrelated range).
4. #222: rejected. Source code only.
5. #212: rejected. Secondary values: smoke.ppm size 786447 and the timestamp are trivial file facts; PROG_EXIT=0 is implied by WM (h)/(f).
6. #218: rejected. 'PT_BVH_STACK_SIZE 20' is a source constant grepped from a header; BUILD_OK is implied by WM (f) (reverted to the snapshot).
7. #224: rejected. Source code only.
8. #198: rejected. Source code only.
9. #245: **accepted, h-646-3**. Secondary fact: merged-tone-map 4 spp smoke run setup_ms=180.8. bvh_nodes=40597 is in WM, and the sample count passing is implied. Grepped: 180.8, 1048576, merged.ppm: 0.
Segment reaches 3; stop.

## art-001608 (second pass)

1. #27: **accepted, h-1608-3**. Secondary fact: setup_ms=318.0 for the default-scene reference render (bvh_nodes 81521 is in live history; the MD5 hashes are not something an operator would ask about). Grepped: 318.0, 'setup_ms=318', 269.6, 272.2: 0. setup_ms occurs in live history only with other values.
(#24 and #5 already used.) Segment reaches 3; stop.

## art-001661 (second pass)

1. #114: rejected. Source header printed with sed, no observed output.
2. #96: rejected. Its only outputs are the three BIT_IDENTICAL checks (in WM round5-stack-payload) and grep line numbers of source.
3. #68: rejected. Secondary values (candidate render_min 43.714 / mean 45.855) belong to the A/B whose base printed nothing. Asking about them means describing that event, which is frozen probe r5-empty-base's topic, so the question could not be posed cleanly under (b)/(d).
4. #118: rejected. Output is 'BUILT _b' only, implied by WM (_b snapshot).
5. #58: **accepted, h-1661-2**. Secondary fact: PT_PROF=3 (no shadow + no mesh) gpu_ms_best=0.458, render 0.623 (WM has settings 0, 1, 2, 4, 8 but not 3). Grepped: 0.458, 0.623, 'PT_PROF=3': 0.
6. #116: rejected. File listing and grep of source, no observed output.
7. #98: rejected. Output 'stack_payload' count 0 and 'BUILT' only; this confirms a scratch edit, not a measurement.
8. #78: **accepted, h-1661-3**. Secondary fact: glass refract-skip candidate gpu_min=44.757 (WM has render min/mean for both sides, not gpu_min). Grepped: 44.757, 44.76, 43.026: 0.
Segment reaches 3; stop.

## art-001766 (second pass)

1. #146: rejected. Output 'walks rewritten' only.
2. #163: **accepted, h-1766-3**. Secondary fact: child-pair default-scene identity run setup_ms=245.7. IDENTICAL_DEFAULT is in WM; threads_used=4 and bvh_nodes=81521 appear in live history. Grepped: 245.7: 0.
(#153 and #159 already used.) Segment reaches 3; stop.

## art-003062 (second pass)

1. #17: rejected. Source code only.
2. #7: **accepted, h-3062-2**. Secondary fact: round 7 vs round 6 interleaved, 10/10 pair wins, 31.003 vs 31.264 ms (1.008x), mean delta -0.2925 ms. Grepped: 31.003, 31.264, 1.008, 10/10, 0.2925, 36.283, 1.171: all 0.
3. #13: rejected. Source code only (cat of header and host).
4. #15: rejected. Source code only.
5. #11: rejected. Source code only.
(#9 already used.) Candidates exhausted; stays at 2.

## art-003133 (second pass)

1. #56: rejected. Secondary output 'sample_parallel=1' is implied by WM (r8-rejected E4 'samples pipeline', r8-measurement-table 'sample-major' buffer); 'sample-parallel' appears 6 times in context. The 1024 limit is in WM and is r5-register-counts' topic.
2. #48: rejected. Output 'trace patched / integrator patched 3' confirms a patch (3 = replacements made), not a measurement.
3. #60: rejected. Output 'integrator variants ok' only.
4. #36: rejected. Output 'host diag patched' plus grep line numbers of source.
5. #42: rejected. grep listing of source, no observed output.
6. #40: rejected. Source code via grep.
7. #38: **accepted, h-3133-1**. Secondary fact: under PT_SPLIT=1 the default configuration printed render_ms_best=31.236 (wall), while the trace dispatch alone took 30.329. The trace numbers are in WM r8-cost-split; the wall times are not. Grepped: 31.236, 35.855, 27.593, 7.204: 0.
8. #23: rejected. Output 'patched' only.
9. #21: rejected. BUILD_OK of _r8b (implied by WM 'frozen A/B binary'); the binary size is not a measurement.
10. #19: rejected. Harness scripts and scene configs. The 'which python3 perl' paths are environment facts, not a measured number or outcome.
Candidates exhausted; stays at 1.
