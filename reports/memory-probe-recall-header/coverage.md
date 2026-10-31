## h-414-1 (art-000414 #56)

Q: Early in round 2 you tried to save a round-1 reference render by copying the round-1 binary into the out/ folder and running it from there. Did that run produce the reference image? If not, what error did it print?

Truth: #56 'metal init error: cannot read shared shader header' (and 'shasum: out/r1_default.pfm: No such file'). Not in the checkpoint's working memory (art-000643) or live history (checked: 'shared shader header', 'shader header', 'cannot read', 'init error', pathtracer_r1, r1_default).

- d1 56-57 [hidden]: copying binary to out/ and running from there fails 'cannot read shared shader header' (needs include/ next to binary) -> keep snapshots self-contained with include+shaders.
- d2 56-57 [hidden]: copying binary to out/ fails to find shaders (host resolves shaders/ relative to argv[0]) -> must run from pathtracer/ dir
- d3 56-57 [hidden]: copying binary to out/ fails to find shaders (needs include/ + shaders/ next to binary) -> do not run a copied binary from out/.

## h-414-2 (art-000414 #31)

Q: At the start of round 2 you read the round-1 writer log. It recorded a caustic check on the 64 spp default render. How bright was the caustic compared with the floor?

Truth: #31 (round-1 writer log) 'floor median luminance 0.56, pixels >= 3x median 1185 ... with mean luminance 9.05, i.e. ~16x the floor'. Not in the checkpoint's working memory (art-000643) or live history (checked: 9.05, 1185, 0.56, luminance, 3.66, 3499; '16x' occurs only as threadgroup shapes such as 16x8). The writer log is on disk, so the probed agent can also re-read it.

- d1 31-31 [shown]: round-1 writer log (design, bug history: shader order, ptr<> unsupported, NSUInteger, refract sign, inside_glass inverted, correlated phi/polar, fresnel eta, self-occluding shadow ray; light placement chosen to hide direct pool; next-round targets SAH/compact nodes/short stack/no per-sample atomics).
- d2 31-31 [shown]: round-1 writer log: design notes, fixed bugs (shadow self-occlusion, refract sign, inside_glass, correlated sampler angles, fresnel eta), caustic mask mean 9.05 over 1185px, listed next-round targets (SAH, compact nodes, short stack, no per-sample atomics)
- d3 31-31 [shown]: round-1 writer log: design notes, fixed bugs (self-occluding shadow ray, refract sign, inside_glass, correlated cosine angles, fresnel eta), light placement rationale, known next targets (SAH, compact nodes, short stack, per-sample atomics).

## h-507-1 (art-000507 #83)

Q: During round-2 attempt 1 (binned SAH build with the packed triangle table), once the sources compiled, did the build link? If not, what failed?

Truth: #83 'Undefined symbols for architecture arm64: "pt::transform_mesh(pt::Mesh&, float, float const*)" ... linker command failed with exit code 1'. Not in the checkpoint's working memory (art-000643) or live history (checked: transform_mesh, 'Undefined symbol', linker, 'ld:', 'link error').

- d1 83-87 [shown]: link error: transform_mesh missing from rewrite; restored it.
- d2 83-83 [shown]: link error transform_mesh missing -> 87 restored it
- d3 83-87 [shown]: link error transform_mesh dropped -> restored

## h-507-2 (art-000507 #79)

Q: In round-2 attempt 1 (binned SAH, 32-byte nodes, packed triangle soup), did the very first build after the host-side rewrite compile? If not, what was the compiler error?

Truth: #79 "include/pt_mesh.hpp:14:17: error: use of undeclared identifier 'PtVertex'" plus 'unknown type name PtVertex' in pt_bvh.cpp and pt_mesh.cpp. Not in the checkpoint's working memory (art-000643) or live history (checked: PtVertex, undeclared, 'unknown type', pt_mesh.hpp).

- d1 79-81 [shown]: build failed: PtVertex was dropped from shared header; re-added it (host-only).
- d2 79-79 [shown]: build error PtVertex undeclared -> 81 re-added PtVertex to shared header
- d3 79-81 [shown]: build error PtVertex missing -> re-added PtVertex to shared header

## h-646-1 (art-000646 #208)

Q: In round 2 your first stackless-traversal build hung the GPU because the host binary was stale: the host build had failed. What was the compiler error in that failed build?

Truth: #208 "src/pt_bvh.cpp:216:13: error: 'link_escapes' is a private member of 'pt::(anonymous namespace)::BvhBuilder'". The checkpoint's working memory (art-000643, round2-progress2 (h)) says only that 'a failed compile left a stale ./pathtracer binary', not why it failed. Not in live history (checked: link_escapes, 'private member', private).

- d2 208-208 [shown]: root cause: build error 'link_escapes is private' (#216 in file) left a STALE ./pathtracer while shaders were new; bvh_stats output nodes=40597 internal=20298 leaves=20299 max_leaf=4 mean_leaf=3.42 max_depth=15
- d3 202-212 [shown]: stackless build FAILED to compile ('link_escapes is private') leaving a stale ./pathtracer -> GPU infinite loop and 60s tool timeouts; fix = rm -f pathtracer and grep for error before benchmarking; smoke at spp 2 then wrote PPM

## h-1608-1 (art-001608 #24)

Q: At the start of round 5, your first attempt to render the round-4 reference images (default, alt-view, caustic-closeup) produced no files. Why?

Truth: #24 'mesh error: cannot open mesh: /Users/peihongguo/Code/rasterizer-bunny/assets/stanford-bunny.obj' three times, 'ls: out/r4_*: No such file' (the path lacks aos/reports/; #27 reran with the right path). Not in the checkpoint's working memory (art-001763, which holds only the correct mesh path) or live history (checked: 'cannot open mesh', 'mesh error', 'Code/rasterizer-bunny', 'mesh path').

- d1 24-24 [shown]: mesh path typo (must be /Users/peihongguo/Code/aos/reports/rasterizer-bunny/assets/stanford-bunny.obj)
- d2 24-24 [shown]: wrong mesh path typo (missing reports/) -> mesh error
- d3 24-24 [shown]: failed ref render: wrong mesh path typo

## h-1608-2 (art-001608 #5)

Q: At the start of round 5 you read the tail of the writer log. What final benchmark time (render_ms_best) had round 4 recorded for itself?

Truth: #5 (writer log tail) 'round 4 (pt_round_04): render_ms_best=45.635 render_ms_avg=46.957 gpu_ms_best=45.349 ... round 3 (ab_r3): render_ms_best=47.828 ... 8/8 interleaved pairs favoured round 4 ... Speedup 1.074x over round 3's verified 49.020ms'. Not in the checkpoint's working memory (art-001763) or live history (checked: 45.635, 47.828, 1.074, 2.403, 8/8, 'round 4 ('; '45.6' occurs only as 45.607, the entered-stack mean). The writer log is on disk, so the probed agent can also re-read it.

- d1 5-5 [shown]: writer log tail: rounds 1-4 history, kept/rejected lists, round-5 ideas (normals one-cache-line, ballot, 256spp harness, child ordering)
- d2 5-5 [shown]: writer log tail: rounds 1-4 history, kept/rejected lists, round-5 ideas (normal interleave, ballot, SAH child order)
- d3 5-5 [shown]: writer log full history rounds1-4: per-change timings, rejected ideas, round4 final 45.635 vs r3 47.828, 2.403x

## h-1661-1 (art-001661 #54)

Q: In round 5, when you first ran the PT_PROF cost-share sweep with the scratch profiling build in _prof/, it printed no timings. What was wrong?

Truth: #54 "metal init error: shader compile failed: program_source:615:10: error: use of undeclared identifier 'params'" (the PT_PROF bit-2 guard in pt_intersect_scene; #55 changed it to scene.params->pad). Not in the checkpoint's working memory (art-001763, round5-cost-shares gives only the results) or live history (checked: undeclared, program_source, 'shader compile failed'; 'params.pad' occurs once, in the memory's description of the bitmask).

- d1 54-56 [shown]: shader compile error 'undeclared params' in pt_intersect_scene -> use scene.params->pad
- d2 52-54 [shown]: first sweep printed nothing; _prof binary built fine, failure was shader compile

## h-1766-1 (art-001766 #153)

Q: In round 5, when you first built the child-pair 32-byte block layout, did it compile? If not, what was the error?

Truth: #153 "pt_main.cpp:165:37: error: cannot initialize a member subobject of type 'const PtBvhNode *' with an rvalue of type ... 'PtBvhChild *'" and "pt_metal_host.hpp:18:11: error: unknown type name 'PtBvhNodeQ'". Not in the checkpoint's working memory (art-001763) or live history (checked: 'cannot initialize', 'unknown type', 'did you mean'; PtBvhNodeQ and GpuMeshData appear in live history only as the current reverted code).

- d1 153-155 [shown]: build error — pt_metal_host.hpp GpuMeshData::nodes still PtBvhNodeQ*; sed fixed to PtBvhChild*
- d2 153-155 [shown]: build errors — GpuMeshData::nodes still typed PtBvhNodeQ* in pt_metal_host.hpp; fixed to PtBvhChild*
- d3 153-157 [shown]: build failed - pt_metal_host.hpp GpuMeshData::nodes still PtBvhNodeQ*; fixed to PtBvhChild*; build clean after

## h-1766-2 (art-001766 #159)

Q: During the round-5 child-pair experiment, one byte-identity check of the default scene against out/r4_default.pfm reported a difference. What caused it?

Truth: #159 'scene error: cannot open scene config: scenes/scene-default.json' and 'cmp: out/cp_default.pfm: No such file or directory' -> DIFF_DEFAULT; the rerun with the right path (#163) was IDENTICAL_DEFAULT. Not in the checkpoint's working memory (art-001763) or live history (checked: 'scenes/', 'cannot open scene', 'scene error', DIFF_DEFAULT).

- d1 159-159 [shown]: wrong scene path scenes/scene-default.json — scenes live at top level (scene-default.json etc.)
- d2 157-163 [shown]: child-pair build clean; default-scene PFM byte-identical to round 4, samples=16777216; scene configs are at top level (scene-default.json), NOT scenes/ — wrong path gave 'cannot open scene config'
- d3 159-161 [shown]: scene configs live at TOP level (scene-default.json, scene-alt-view.json, scene-caustic-closeup.json), not in scenes/; wrong path gives 'cannot open scene config'

## h-3062-1 (art-003062 #9)

Q: In the round-7 section of the writer log you read at the start of round 8, what happened when the BVH's maximum depth was raised from 16 to 20, and what did the round-7 writer conclude about changing the tree's shape?

Truth: #9 (writer log, Round 7) 'CRITICAL FINDING - the BVH TOPOLOGY IS PART OF THE FROZEN ESTIMATOR. Raising PT_BVH_MAX_DEPTH from 16 to 20 (nodes 81521 -> 123159 ...) changes the scene-alt-view PFM at byte 1022129 ... the cause is the tie-break ... rules out the whole class of tree-quality ideas'. Not in the checkpoint's working memory (art-003130) or live history (checked: 1022129, 123159, topology, MAX_DEPTH, tie-break, 'test order', 'tree quality', 'leaf size', 'deeper tree'). The writer log is on disk, so the probed agent can also re-read it.

- d1 9-9 [shown]: writer log tail (r7 kept f32 node -0.44%, shadow payload -0.49%, 12B sample buffer -0.19%; rejected stk16 +27%, spt2 +1.08%, leaf2 +0.9%; CRITICAL BVH topology frozen by strict t<best_t tie-break) + file sizes (pt_trace.metal 552L, pt_integrator.metal 367L, pt_metal_host.mm 309L)
- d2 9-9 [shown]: writer log r7: kept f32 node -0.44%, shadow stack payload -0.49%, 12B PtSample3 -0.19%; rejected stk16 +27.1%, spt2 +1.08%, leaf1 +0.4%; BVH TOPOLOGY IS THE ESTIMATOR (MAX_DEPTH 16->20 changed alt-view PFM) - leaf/depth/axis/bin changes forbidden.
- d3 9-9 [shown]: writer log r7: kept f32 node -0.44%, shadow payload -0.49%, 12B sample buffer -0.19%; rejected stk16 +27%, spt2 +1.08%, leaf2 +0.9%; BVH TOPOLOGY FROZEN (MAX_DEPTH 16->20 changes alt-view PFM)
- d3 9-9 [shown]: file sizes: pt_trace.metal 552, pt_integrator.metal 367, pt_metal_host.mm 309, pt_bvh.cpp 435

## h-414-3 (art-000414 #43)

Q: At the start of round 2 you benchmarked the rebuilt round-1 code with its image outputs pointed at /tmp (base.ppm / base.pfm). Did that run write its image? If not, what did it report?

Truth: #43 'ppm error: cannot open output: /tmp/base.ppm' (exit 1; the benchmark line itself printed). Second pass, secondary fact (the primary, render_ms_best=105.947, is in working memory). Not in the checkpoint's working memory (art-000643) or live history (checked: /tmp, 'cannot open output', 'ppm error', base.ppm).

- d1 43-45 [shown]: baseline re-measured on this machine: render_ms_best=105.947 render_ms_avg=113.523 gpu_ms_best=105.574; /tmp not writable (ppm error) -> write outputs to phase3/out.
- d2 43-43 [hidden]: baseline rebuilt+benchmarked here: render_ms_best=105.947 render_ms_avg=113.523 gpu_ms_best=105.574 (512x512/64spp seed1); /tmp not writable, outputs must go under phase3/out

## h-557-1 (art-000557 #125)

Q: In round 2, after going back to the median-split tree, you tried shrinking the traversal stack to 16 entries with a depth cap of 15. Besides its best time, what render_ms_avg did that 5-frame benchmark print?

Truth: #125 'render_ms_best=119.866 render_ms_avg=126.379 gpu_ms_best=119.359'. Second pass, secondary fact (the best time, 119.9, is in working memory round2-progress (b)). Not in the checkpoint's working memory (art-000643) or live history (checked: 126.379, 126.4, 119.359; '126.' occurs only as the root-exit 'avg 126.0').

- d1 123-125 [shown]: PT_BVH_STACK_SIZE 20->16 and PT_BVH_MAX_DEPTH 16->15 REGRESSED to 119.866/119.359.
- d2 123-125 [shown]: PT_BVH_STACK_SIZE 20->16 + cap 15 REGRESSED: 119.866/126.379 gpu 119.359
- d3 125-125 [shown]: that REGRESSED: 119.866/126.379, gpu 119.359

## h-557-2 (art-000557 #133)

Q: In round 2, when you rebuilt with Metal fastMathEnabled turned on and ran the 5-frame benchmark, what gpu_ms_best did it print?

Truth: #133 'render_ms_best=101.150 render_ms_avg=116.465 gpu_ms_best=100.887'. Second pass, secondary fact (render_ms_best 101.15 is in working memory round2-progress (c)). Not in the checkpoint's working memory (art-000643) or live history (checked: 100.887, 116.465, 116.5).

- d1 131-133 [shown]: MTLCompileOptions fastMathEnabled=YES gave 101.150/gpu 100.887 (neutral, deprecated in macOS 15 warning).
- d2 131-133 [shown]: MTLCompileOptions fastMathEnabled=YES: 101.150/116.465 gpu 100.887 (neutral; deprecated in macOS 15, use mathMode)
- d3 133-133 [hidden]: fastMath result 101.150/116.465 gpu 100.887 (neutral) + deprecation warning macOS 15 (use mathMode)

## h-557-3 (art-000557 #167)

Q: In round 2's first threadgroup-shape sweep (the PT_TG knob, 3 frames per shape), what render_ms_best did the 8x8 shape measure?

Truth: #167 'TG=8x8 render_ms_best=101.026 render_ms_avg=106.438 gpu_ms_best=100.761'. Second pass, secondary fact (the sweep's winner 16x8 and the other shapes are in working memory round2-progress (e); 8x8 is not). Not in the checkpoint's working memory (art-000643) or live history (checked: 8x8, 101.026, '101.0', 100.761).

- d1 167-167 [shown]: shape sweep best-of-3: 8x8=101.0, 16x8=96.4/gpu93.6, 16x16=100.5, 32x8=104.6, 16x32=110.0, 32x16=118.8.
- d2 167-167 [hidden]: shape sweep 512x512/64spp best-of-3: 8x8=101.0 16x8=96.4(gpu93.6) 16x16=100.5 32x8=104.6 16x32=110.0 32x16=118.8
- d3 167-167 [hidden]: shape sweep best-of-3: 8x8=101.026, 16x8=96.408(gpu 93.635), 16x16=100.467, 32x8=104.591, 16x32=110.010, 32x16=118.828

## h-646-2 (art-000646 #216)

Q: In round 2, when you rebuilt the pre-stackless stack-based snapshot (out/pt_snapshot_pre_stackless) and ran it twice for the A/B against the stackless walk, what render_ms_avg values did the two runs print?

Truth: #216 run 1 'render_ms_best=93.366 render_ms_avg=103.183', run 2 'render_ms_best=96.614 render_ms_avg=100.692'. Second pass, secondary fact (the best times 93.4/96.6 and gpu 92.9 are in working memory round2-progress2 (f)). Not in the checkpoint's working memory (art-000643) or live history (checked: 103.183, 100.692, 103.2, 100.7).

- d2 216-216 [shown]: pre-stackless A/B twice: 93.366/103.183 gpu 92.993 and 96.614/100.692 gpu 92.915
- d3 214-218 [shown]: stackless measured 102.08 best/114.59 avg vs stack-based 93.37/96.61 (gpu 92.9) in the same A/B -> REGRESSION, reverted to out/pt_snapshot_pre_stackless; stackless copy kept at pathtracer_backup_stackless

## h-646-3 (art-000646 #245)

Q: In round 2, after merging the tone mapping into the trace kernel, you ran a 4 spp smoke test writing out/merged.ppm. What setup_ms did that run print?

Truth: #245 'setup_ms=180.8 samples=1048576 width=512 height=512 spp=4 threads_used=8 triangles=69451 bvh_nodes=40597'. Second pass, secondary fact (the run passing is implied by working memory round2-final). Not in the checkpoint's working memory (art-000643) or live history (checked: 180.8, 1048576, merged.ppm).

- d2 245-245 [hidden]: merged-tone build clean, smoke spp4 samples=1048576 == 512*512*4, PFM 3145744 / PPM 786447

## h-1608-3 (art-001608 #27)

Q: At the start of round 5, when you rendered the round-4 reference images with the correct mesh path, what setup_ms did the default scene's render print?

Truth: #27 'setup_ms=318.0 ... bvh_nodes=81521' for the default scene (alt-view 269.6, caustic-closeup 272.2). Second pass, secondary fact. Not in the checkpoint's working memory (art-001763) or live history (checked: 318.0, 'setup_ms=318', 269.6, 272.2; setup_ms occurs in live history only with other values).

- d1 27-27 [shown]: round-4 reference PFMs out/r4_{default,alt-view,caustic-closeup}.pfm md5 53600ca.../1f0de1.../0e38e7...
- d2 27-27 [shown]: round-4 ref PFMs out/r4_*.pfm: default md5 53600ca347f0b76ab1a0ff894f6a8f4e, alt-view 1f0de145..., caustic 0e38e718...
- d3 27-27 [shown]: round-4 ref PFMs saved out/r4_*.pfm md5s default 53600ca347f0b76ab1a0ff894f6a8f4e alt 1f0de145.. caustic 0e38e718..

## h-1661-2 (art-001661 #58)

Q: In round 5's PT_PROF cost-share sweep, what did the combined setting PT_PROF=3 (no shadow walk and no mesh closest-hit) measure?

Truth: #58 'PT_PROF=3 render_ms_best=0.623 render_ms_avg=1.073 gpu_ms_best=0.458'. Second pass, secondary fact (settings 0, 1, 2, 4, 8 are in working memory round5-cost-shares; 3 is not). Not in the checkpoint's working memory (art-001763) or live history (checked: 0.458, 0.623, 'PT_PROF=3').

- d1 58-58 [shown]: PT_PROF cost shares gpu_ms_best: full 45.35, no shadow 44.05, no NEE 43.17, no mesh 4.77, glass always-reflect 35.76 -> closest-hit BVH walk is ~89% of frame
- d2 58-58 [shown]: COST SHARES 512x512/64spp gpu_ms_best: full 45.35, no-shadow 44.05, no-mesh 4.77 (BVH walk ~89%), no-NEE 43.17, glass always-reflect 35.76
- d3 58-58 [shown]: PT_PROF cost shares gpu_ms_best 512x512/64spp: full 45.35, no shadow 44.05, no NEE 43.17, no mesh 4.77 (closest-hit walk ~89% of frame), glass always-reflect 35.76

## h-1661-3 (art-001661 #78)

Q: In round 5's 8x12 interleaved A/B of the glass refract-skip candidate against the round-4 binary, what gpu_min did the candidate measure?

Truth: #78 '_ab_cand.txt render_min=45.025 render_mean=46.563 gpu_min=44.757' (base gpu_min 43.026). Second pass, secondary fact (render_min/mean of both sides are in working memory round5-glass-and-stack; gpu_min is not). Not in the checkpoint's working memory (art-001763) or live history (checked: 44.757, 44.76, 43.026).

- d1 78-78 [shown]: glass micro-opt REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base; ior 1.5 means Fresnel TIR guard never fires (dead branch)
- d2 78-78 [shown]: GLASS MICRO-OPT REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base
- d3 78-78 [shown]: glass micro-opt REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base; ior 1.5 means Fresnel TIR guard never fires (sin2_t<=0.444) - dead code

## h-1766-3 (art-001766 #163)

Q: In round 5, the first successful byte-identity check of the child-pair layout on the default scene (64 spp, seed 1, against out/r4_default.pfm) printed a setup line. What setup_ms did it show?

Truth: #163 'setup_ms=245.7 samples=16777216 ... threads_used=4 ... bvh_nodes=81521' then IDENTICAL_DEFAULT. Second pass, secondary fact (the identity result is in working memory round5-childpair). Not in the checkpoint's working memory (art-001763) or live history (checked: 245.7; setup_ms occurs in live history only with other values).

- d1 163-163 [shown]: child-pair PFMs byte-identical on default/alt-view/caustic-closeup, samples=16777216
- d2 157-163 [shown]: child-pair build clean; default-scene PFM byte-identical to round 4, samples=16777216; scene configs are at top level (scene-default.json), NOT scenes/ — wrong path gave 'cannot open scene config'

## h-3062-2 (art-003062 #7)

Q: In the round-7 benchmark record (pathtracer_benchmark.json) you read at the start of round 8, how did round 7 do against round 6 when the two were run interleaved on the same quiet machine?

Truth: #7 'candidate_wins: 10/10', 'mean_delta_render_ms: -0.2925', verdict '31.003 ms vs 31.264 ms best-of-50-frames (1.008x, 10/10 pair wins)'. Second pass, secondary fact (the verified 30.984 / 3.539x is in working memory r8-baseline-and-harness). Not in the checkpoint's working memory (art-003130) or live history (checked: 31.003, 31.264, 1.008, 10/10, 0.2925, 36.283, 1.171).

- d1 7-7 [shown]: benchmark.json tail (r7 vs r6 10/10 pairs, mean -0.2925ms; gpu 99.1% of wall; samples 546.4M/s) + coordinator ledger rounds 4-7 (all PASS; r7 best verified)
- d2 3-7 [shown]: dir has pt_round_01..07, _r7b, 3 scene jsons, benchmark.json, 3 logs, _r7_ab.sh/_r7_pair.pl. benchmark: gpu=99.1% of wall (0.278ms host), 546.4M samples/s, speedup 3.539. Ledger: r6 36.283, r7 30.984 (+0.93% win, sub-5 counter=1); stop rule = r7 AND r8 both <5%.
- d3 7-7 [shown]: benchmark.json: r7 vs r6 10/10 wins mean -0.2925ms, 99.1% GPU, wall-gpu 0.278ms, speedup 3.539
- d3 7-7 [shown]: coordinator ledger: rounds 4-7 results, sub-5% counter=1, round-8 plan = the 4 experiments

## h-3133-1 (art-003133 #38)

Q: In the round-8 E0 profile (scratch _r8p/ build with PT_SPLIT=1), what render_ms_best (wall time) did the default configuration, with no diagnostic flags, print?

Truth: #38 cfg='' '[split] trace_gpu_ms=30.329' then 'render_ms_best=31.236 render_ms_avg=35.855 gpu_ms_best=0.425'. Second pass, secondary fact (trace 30.30 and the depth sweep are in working memory r8-cost-split; the wall time is not). Not in the checkpoint's working memory (art-003130) or live history (checked: 31.236, 35.855, 27.593, 7.204).

- d1 36-38 [shown]: _r8p env hooks PT_DEFINE (inject shader #defines) and PT_DIAG_DEPTH; cost sweep d1=5.82 d2=10.96 d3=17.10 d12=30.28 ms; PT_DIAG_NOSHADOW=26.68 -> shadow walks only 3.65ms (12%)
- d2 38-38 [shown]: cost split at 512x512 64spp trace_gpu_ms: d1=5.82 d2=10.96 d3=17.10 d12=30.28, noshadow=26.68
- d3 35-39 [shown]: cost split — PT_DEFINE/PT_DIAG_DEPTH hooks in _r8p; d1=5.82 d2=10.96 d3=17.10 d12=30.28ms; PT_DIAG_NOSHADOW=26.68 (shadow walks 3.65ms)
