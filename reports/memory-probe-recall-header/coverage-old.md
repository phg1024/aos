## h-414-1 (art-000414 #56)

Q: Early in round 2 you tried to save a round-1 reference render by copying the round-1 binary into the out/ folder and running it from there. Did that run produce the reference image? If not, what error did it print?

Truth: #56 'metal init error: cannot read shared shader header' (and 'shasum: out/r1_default.pfm: No such file'). Not in the checkpoint's working memory (art-000643) or live history (checked: 'shared shader header', 'shader header', 'cannot read', 'init error', pathtracer_r1, r1_default).

- d1 56-57: copying binary to out/pathtracer_r1 fails (needs include/+shaders/ next to binary); out/ already has round1_default.pfm/ppm reference images
- d2 46-57: memory entries set (baseline-timing 43-45, round2-plan); read pt_tonemap.metal (Reinhard+gamma 2.2, NaN guard); copying round-1 binary to out/ failed because shaders must sit next to binary

## h-414-2 (art-000414 #31)

Q: At the start of round 2 you read the round-1 writer log. It recorded a caustic check on the 64 spp default render. How bright was the caustic compared with the floor?

Truth: #31 (round-1 writer log) 'floor median luminance 0.56, pixels >= 3x median 1185 ... with mean luminance 9.05, i.e. ~16x the floor'. Not in the checkpoint's working memory (art-000643) or live history (checked: 9.05, 1185, 0.56, luminance, 3.66, 3499; '16x' occurs only as threadgroup shapes such as 16x8). The writer log is on disk, so the probed agent can also re-read it.

- d1 31-31: writer log round-1: full design notes, bugs fixed (self-occluding shadow ray, fresnel eta, inside_glass, correlated cosine angles), known next targets (SAH, compact nodes, short stack, per-sample atomics, no spp tiling)
- d2 31-31: writer log round1: design notes, bug history, caustic mask mean 9.05/1185px, next-round targets (SAH, compact nodes, short stack, per-sample atomics, no spp tiling)
- d3 31-31: writer log round-1 (design notes, bug list, caustic stats: floor median 0.56, 1185 px >=3x, mean 9.05; known next targets: SAH, compact nodes, no wavefront, per-sample atomics).

## h-507-1 (art-000507 #83)

Q: During round-2 attempt 1 (binned SAH build with the packed triangle table), once the sources compiled, did the build link? If not, what failed?

Truth: #83 'Undefined symbols for architecture arm64: "pt::transform_mesh(pt::Mesh&, float, float const*)" ... linker command failed with exit code 1'. Not in the checkpoint's working memory (art-000643) or live history (checked: transform_mesh, 'Undefined symbol', linker, 'ld:', 'link error').

- d1 83-83: link error - transform_mesh missing.
- d2 79-87: build errors: I dropped PtVertex from the shared header (re-added, host-only) and dropped transform_mesh in a rewrite (restored).

## h-507-2 (art-000507 #79)

Q: In round-2 attempt 1 (binned SAH, 32-byte nodes, packed triangle soup), did the very first build after the host-side rewrite compile? If not, what was the compiler error?

Truth: #79 "include/pt_mesh.hpp:14:17: error: use of undeclared identifier 'PtVertex'" plus 'unknown type name PtVertex' in pt_bvh.cpp and pt_mesh.cpp. Not in the checkpoint's working memory (art-000643) or live history (checked: PtVertex, undeclared, 'unknown type', pt_mesh.hpp).

- d1 79-79: build errors - PtVertex undeclared (dropped from header).
- d2 79-87: build errors: I dropped PtVertex from the shared header (re-added, host-only) and dropped transform_mesh in a rewrite (restored).

## h-646-1 (art-000646 #208)

Q: In round 2 your first stackless-traversal build hung the GPU because the host binary was stale: the host build had failed. What was the compiler error in that failed build?

Truth: #208 "src/pt_bvh.cpp:216:13: error: 'link_escapes' is a private member of 'pt::(anonymous namespace)::BvhBuilder'". The checkpoint's working memory (art-000643, round2-progress2 (h)) says only that 'a failed compile left a stale ./pathtracer binary', not why it failed. Not in live history (checked: link_escapes, 'private member', private).

- d1 208-208: root cause found: build had FAILED with "'link_escapes' is a private member" so ./pathtracer was the OLD binary (b=0) reading NEW stackless shaders -> escape 0 = root = infinite loop; bvh_stats printed nodes=40597 max_leaf=4 depth=15
- d2 208-208: compile error 'link_escapes is private' (#208) proves ./pathtracer was the OLD host with NEW stackless shader -> escape 0 = root -> infinite loop; bvh_stats: nodes=40597 max_leaf=4 mean_leaf=3.42 max_depth=15
- d3 202-212: stackless build FAILED silently (link_escapes private, #208) leaving stale binary -> GPU infinite loop, 60s tool timeouts; fix = rm -f pathtracer + grep error before benching; smoke at spp2 with perl alarm.

## h-1608-1 (art-001608 #24)

Q: At the start of round 5, your first attempt to render the round-4 reference images (default, alt-view, caustic-closeup) produced no files. Why?

Truth: #24 'mesh error: cannot open mesh: /Users/peihongguo/Code/rasterizer-bunny/assets/stanford-bunny.obj' three times, 'ls: out/r4_*: No such file' (the path lacks aos/reports/; #27 reran with the right path). Not in the checkpoint's working memory (art-001763, which holds only the correct mesh path) or live history (checked: 'cannot open mesh', 'mesh error', 'Code/rasterizer-bunny', 'mesh path').


## h-1608-2 (art-001608 #5)

Q: At the start of round 5 you read the tail of the writer log. What final benchmark time (render_ms_best) had round 4 recorded for itself?

Truth: #5 (writer log tail) 'round 4 (pt_round_04): render_ms_best=45.635 render_ms_avg=46.957 gpu_ms_best=45.349 ... round 3 (ab_r3): render_ms_best=47.828 ... 8/8 interleaved pairs favoured round 4 ... Speedup 1.074x over round 3's verified 49.020ms'. Not in the checkpoint's working memory (art-001763) or live history (checked: 45.635, 47.828, 1.074, 2.403, 8/8, 'round 4 ('; '45.6' occurs only as 45.607, the entered-stack mean). The writer log is on disk, so the probed agent can also re-read it.

- d2 5-5: writer log tail: rounds 1-4 history, all kept/rejected items with timings; round-4 final 45.635 vs r3 47.828 interleaved, 2.403x over frozen 109.656; round-5 ideas (interleave normals into tri record, simdgroup ballot, 256spp harness, child ordering).
- d3 5-5: writer log tail: rounds 1-4 history, all kept/rejected items with numbers, round-5 ideas (normals interleave, ballot, 256spp harness, SAH child order)

## h-1661-1 (art-001661 #54)

Q: In round 5, when you first ran the PT_PROF cost-share sweep with the scratch profiling build in _prof/, it printed no timings. What was wrong?

Truth: #54 "metal init error: shader compile failed: program_source:615:10: error: use of undeclared identifier 'params'" (the PT_PROF bit-2 guard in pt_intersect_scene; #55 changed it to scene.params->pad). Not in the checkpoint's working memory (art-001763, round5-cost-shares gives only the results) or live history (checked: undeclared, program_source, 'shader compile failed'; 'params.pad' occurs once, in the memory's description of the bitmask).

- d1 54-56: shader compile error from using bare `params` in pt_intersect_scene; must use scene.params->pad
- d3 52-54: first sweep empty - shader compile error: undeclared 'params' in pt_intersect_scene, must use scene.params->pad

## h-1766-1 (art-001766 #153)

Q: In round 5, when you first built the child-pair 32-byte block layout, did it compile? If not, what was the error?

Truth: #153 "pt_main.cpp:165:37: error: cannot initialize a member subobject of type 'const PtBvhNode *' with an rvalue of type ... 'PtBvhChild *'" and "pt_metal_host.hpp:18:11: error: unknown type name 'PtBvhNodeQ'". Not in the checkpoint's working memory (art-001763) or live history (checked: 'cannot initialize', 'unknown type', 'did you mean'; PtBvhNodeQ and GpuMeshData appear in live history only as the current reverted code).

- d1 153-155: build failure — pt_metal_host.hpp GpuMeshData::nodes still typed PtBvhNodeQ*; fixed to PtBvhChild*
- d2 153-155: build error — GpuMeshData::nodes still typed PtBvhNodeQ*; fixed to PtBvhChild* in pt_metal_host.hpp:18.
- d3 153-155: build error - pt_metal_host.hpp GpuMeshData::nodes still PtBvhNodeQ*; fixed to PtBvhChild*.

## h-1766-2 (art-001766 #159)

Q: During the round-5 child-pair experiment, one byte-identity check of the default scene against out/r4_default.pfm reported a difference. What caused it?

Truth: #159 'scene error: cannot open scene config: scenes/scene-default.json' and 'cmp: out/cp_default.pfm: No such file or directory' -> DIFF_DEFAULT; the rerun with the right path (#163) was IDENTICAL_DEFAULT. Not in the checkpoint's working memory (art-001763) or live history (checked: 'scenes/', 'cannot open scene', 'scene error', DIFF_DEFAULT).

- d1 159-161: scene configs live at the top level (scene-default.json, scene-alt-view.json, scene-caustic-closeup.json), not in scenes/; scratch dirs present: _b _e _p _prof _tools ab_r3 ab_r4 out
- d3 157-163: child-pair build clean (only the known fastMathEnabled deprecation warning); 64spp seed-1 PFMs byte-identical to round 4 on default, alt-view and caustic-closeup, samples=16777216; workspace listing shows scenes at top level (scene-default.json, scene-alt-view.json, scene-caustic-closeup.json), snapshots _b/_e/ab_r4, reference PFMs in out/; _ab.sh is the interleaved A/B harness and the run tool caps a command at ~60s.

## h-3062-1 (art-003062 #9)

Q: In the round-7 section of the writer log you read at the start of round 8, what happened when the BVH's maximum depth was raised from 16 to 20, and what did the round-7 writer conclude about changing the tree's shape?

Truth: #9 (writer log, Round 7) 'CRITICAL FINDING - the BVH TOPOLOGY IS PART OF THE FROZEN ESTIMATOR. Raising PT_BVH_MAX_DEPTH from 16 to 20 (nodes 81521 -> 123159 ...) changes the scene-alt-view PFM at byte 1022129 ... the cause is the tie-break ... rules out the whole class of tree-quality ideas'. Not in the checkpoint's working memory (art-003130) or live history (checked: 1022129, 123159, topology, MAX_DEPTH, tie-break, 'test order', 'tree quality', 'leaf size', 'deeper tree'). The writer log is on disk, so the probed agent can also re-read it.

- d1 9-9: coordinator ledger + writer log: round7 VERIFIED PASS 9/9 (RMSE ratios 1.0003/1.0055/1.0015, caustic 0.9888/0.9742/1.0025, 20/20 stability); sub-5% counter=1 so round 8 decides the stop; round7 kept f32 nodes -0.44%, shadow payload -0.49%, 12B sample buffer -0.19%; rejected stk16 +27%, spt2 +1.08%, leaf2 +0.9%; BVH topology is part of the frozen estimator (MAX_DEPTH 16->20 changed alt-view PFM) so leaf_size/MAX_DEPTH/axis-order/bins are forbidden.
- d2 9-9: writer log r7: kept f32 node -0.44%, shadow stack payload -0.49%, 12B sample buffer -0.19%; rejected stk16 +27%, stk24 0, 2spp/thread +1.08%, leaf2 +0.9%, leaf1-spec +0.4%, combine TG sweep noise
- d2 9-9: BVH TOPOLOGY is part of the frozen estimator (MAX_DEPTH 16->20 changed alt-view PFM): leaf/depth/axis/bin changes forbidden
- d2 9-9: file sizes: bvh.cpp 435, host.mm 309, integrator.metal 367, trace.metal 552
- d3 9-9: source tree + line counts (include 8 headers, src 7 files, shaders pt_trace 552 / pt_integrator 367 / pt_tonemap 22).

## h-414-3 (art-000414 #43)

Q: At the start of round 2 you benchmarked the rebuilt round-1 code with its image outputs pointed at /tmp (base.ppm / base.pfm). Did that run write its image? If not, what did it report?

Truth: #43 'ppm error: cannot open output: /tmp/base.ppm' (exit 1; the benchmark line itself printed). Second pass, secondary fact (the primary, render_ms_best=105.947, is in working memory). Not in the checkpoint's working memory (art-000643) or live history (checked: /tmp, 'cannot open output', 'ppm error', base.ppm).

- d1 43-45: baseline re-measured on this machine: render_ms_best=105.947 render_ms_avg=113.523 gpu_ms_best=105.574; /tmp not writable (ppm error)
- d2 41-45: rebuilt round-1 OK; baseline on this machine render_ms_best=105.947 avg=113.523 gpu_ms_best=105.574; /tmp not writable, use phase3/out; sandbox prints harmless metalfe/gpuarchiver errors
- d3 43-43: baseline rebuilt+benchmarked here: render_ms_best=105.947 gpu_ms_best=105.574; /tmp not writable (ppm error).

## h-557-1 (art-000557 #125)

Q: In round 2, after going back to the median-split tree, you tried shrinking the traversal stack to 16 entries with a depth cap of 15. Besides its best time, what render_ms_avg did that 5-frame benchmark print?

Truth: #125 'render_ms_best=119.866 render_ms_avg=126.379 gpu_ms_best=119.359'. Second pass, secondary fact (the best time, 119.9, is in working memory round2-progress (b)). Not in the checkpoint's working memory (art-000643) or live history (checked: 126.379, 126.4, 119.359; '126.' occurs only as the root-exit 'avg 126.0').

- d1 125-125: benchmark with stack 16 REGRESSED: best=119.866 avg=126.379 gpu=119.359
- d2 123-125: PT_BVH_STACK_SIZE 20->16 with MAX_DEPTH 16->15 REGRESSED to 119.866/119.359
- d3 125-125: regressed: render_ms_best=119.866 gpu=119.359

## h-557-2 (art-000557 #133)

Q: In round 2, when you rebuilt with Metal fastMathEnabled turned on and ran the 5-frame benchmark, what gpu_ms_best did it print?

Truth: #133 'render_ms_best=101.150 render_ms_avg=116.465 gpu_ms_best=100.887'. Second pass, secondary fact (render_ms_best 101.15 is in working memory round2-progress (c)). Not in the checkpoint's working memory (art-000643) or live history (checked: 100.887, 116.465, 116.5).

- d1 133-133: benchmark with fastMath: best=101.150 avg=116.465 gpu=100.887; deprecation warning macOS 15
- d2 131-133: MTLCompileOptions fastMathEnabled=YES (deprecated in macOS 15): 101.150 best, gpu 100.887 - neutral gain, kept
- d3 133-133: fastMath 101.150/116.465 gpu 100.887 (neutral) + deprecation warning (macOS 15, use mathMode)

## h-557-3 (art-000557 #167)

Q: In round 2's first threadgroup-shape sweep (the PT_TG knob, 3 frames per shape), what render_ms_best did the 8x8 shape measure?

Truth: #167 'TG=8x8 render_ms_best=101.026 render_ms_avg=106.438 gpu_ms_best=100.761'. Second pass, secondary fact (the sweep's winner 16x8 and the other shapes are in working memory round2-progress (e); 8x8 is not). Not in the checkpoint's working memory (art-000643) or live history (checked: 8x8, 101.026, '101.0', 100.761).

- d1 167-167: TG sweep (3 frames): 8x8=101.0, 16x8=96.4, 16x16=100.5, 32x8=104.6, 16x32=110.0, 32x16=118.8
- d2 167-167: shape sweep best-of-3: 8x8=101.026, 16x8=96.408 (gpu 93.635), 16x16=100.467, 32x8=104.591, 16x32=110.010, 32x16=118.828
- d3 167-167: shape sweep best-of-3: 8x8=101.0, 16x8=96.4/93.6gpu, 16x16=100.5/96.0, 32x8=104.6, 16x32=110.0, 32x16=118.8

## h-646-2 (art-000646 #216)

Q: In round 2, when you rebuilt the pre-stackless stack-based snapshot (out/pt_snapshot_pre_stackless) and ran it twice for the A/B against the stackless walk, what render_ms_avg values did the two runs print?

Truth: #216 run 1 'render_ms_best=93.366 render_ms_avg=103.183', run 2 'render_ms_best=96.614 render_ms_avg=100.692'. Second pass, secondary fact (the best times 93.4/96.6 and gpu 92.9 are in working memory round2-progress2 (f)). Not in the checkpoint's working memory (art-000643) or live history (checked: 103.183, 100.692, 103.2, 100.7).

- d1 216-216: A/B of pre-stackless snapshot twice: 93.366/103.183 gpu 92.993 and 96.614/100.692 gpu 92.915 -> stackless is a ~9% regression
- d2 216-216: A/B pre-stackless stack-based snapshot = 93.366/103.183 gpu 92.993 and 96.614/100.692 gpu 92.915 -> stackless REGRESSED
- d3 216-216: stack-based A/B same machine 93.366/103.183 gpu 92.993 and 96.614/100.692 gpu 92.915 -> stackless REGRESSED, reverted (#218, backup pathtracer_backup_stackless).

## h-646-3 (art-000646 #245)

Q: In round 2, after merging the tone mapping into the trace kernel, you ran a 4 spp smoke test writing out/merged.ppm. What setup_ms did that run print?

Truth: #245 'setup_ms=180.8 samples=1048576 width=512 height=512 spp=4 threads_used=8 triangles=69451 bvh_nodes=40597'. Second pass, secondary fact (the run passing is implied by working memory round2-final). Not in the checkpoint's working memory (art-000643) or live history (checked: 180.8, 1048576, merged.ppm).

- d1 245-245: merged build smoke OK: samples=1048576 == 512*512*4, PPM+PFM written
- d2 243-245: build clean, smoke spp4 samples=1048576 == 512*512*4, PPM+PFM written
- d3 245-245: merged build smoke spp4 samples=1048576 OK.

## h-1608-3 (art-001608 #27)

Q: At the start of round 5, when you rendered the round-4 reference images with the correct mesh path, what setup_ms did the default scene's render print?

Truth: #27 'setup_ms=318.0 ... bvh_nodes=81521' for the default scene (alt-view 269.6, caustic-closeup 272.2). Second pass, secondary fact. Not in the checkpoint's working memory (art-001763) or live history (checked: 318.0, 'setup_ms=318', 269.6, 272.2; setup_ms occurs in live history only with other values).


## h-1661-2 (art-001661 #58)

Q: In round 5's PT_PROF cost-share sweep, what did the combined setting PT_PROF=3 (no shadow walk and no mesh closest-hit) measure?

Truth: #58 'PT_PROF=3 render_ms_best=0.623 render_ms_avg=1.073 gpu_ms_best=0.458'. Second pass, secondary fact (settings 0, 1, 2, 4, 8 are in working memory round5-cost-shares; 3 is not). Not in the checkpoint's working memory (art-001763) or live history (checked: 0.458, 0.623, 'PT_PROF=3').

- d1 58-58: cost shares 512x512/64spp gpu_ms_best: full 45.35, no shadow 44.05, no mesh 4.77 (walk=89% of frame), no NEE 43.17, glass always-reflect 35.76
- d3 58-58: COST SHARES 512x512/64spp gpu_ms_best: full 45.35, no shadow 44.05, no NEE 43.17, no mesh 4.77 (closest-hit walk = 89% of frame), glass always-reflect 35.76

## h-1661-3 (art-001661 #78)

Q: In round 5's 8x12 interleaved A/B of the glass refract-skip candidate against the round-4 binary, what gpu_min did the candidate measure?

Truth: #78 '_ab_cand.txt render_min=45.025 render_mean=46.563 gpu_min=44.757' (base gpu_min 43.026). Second pass, secondary fact (render_min/mean of both sides are in working memory round5-glass-and-stack; gpu_min is not). Not in the checkpoint's working memory (art-001763) or live history (checked: 44.757, 44.76, 43.026).

- d1 78-78: glass micro-opt REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base
- d3 78-78: glass micro-opt REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base; reverted

## h-1766-3 (art-001766 #163)

Q: In round 5, the first successful byte-identity check of the child-pair layout on the default scene (64 spp, seed 1, against out/r4_default.pfm) printed a setup line. What setup_ms did it show?

Truth: #163 'setup_ms=245.7 samples=16777216 ... threads_used=4 ... bvh_nodes=81521' then IDENTICAL_DEFAULT. Second pass, secondary fact (the identity result is in working memory round5-childpair). Not in the checkpoint's working memory (art-001763) or live history (checked: 245.7; setup_ms occurs in live history only with other values).

- d1 163-163: child-pair binary built and default-scene render printed samples=16777216 (correct) with the sandbox_extension warnings that are harmless
- d3 157-163: child-pair build clean (only the known fastMathEnabled deprecation warning); 64spp seed-1 PFMs byte-identical to round 4 on default, alt-view and caustic-closeup, samples=16777216; workspace listing shows scenes at top level (scene-default.json, scene-alt-view.json, scene-caustic-closeup.json), snapshots _b/_e/ab_r4, reference PFMs in out/; _ab.sh is the interleaved A/B harness and the run tool caps a command at ~60s.

## h-3062-2 (art-003062 #7)

Q: In the round-7 benchmark record (pathtracer_benchmark.json) you read at the start of round 8, how did round 7 do against round 6 when the two were run interleaved on the same quiet machine?

Truth: #7 'candidate_wins: 10/10', 'mean_delta_render_ms: -0.2925', verdict '31.003 ms vs 31.264 ms best-of-50-frames (1.008x, 10/10 pair wins)'. Second pass, secondary fact (the verified 30.984 / 3.539x is in working memory r8-baseline-and-harness). Not in the checkpoint's working memory (art-003130) or live history (checked: 31.003, 31.264, 1.008, 10/10, 0.2925, 36.283, 1.171).

- d1 7-7: benchmark.json: round7 vs round6 10/10 pairs, mean -0.2925ms, ratio 0.9917; gpu_fraction 0.991, wall-gpu 0.278ms, speedup_vs_round1 3.539, round1 109.656ms, round7 30.984/30.706ms.
- d2 7-7: bench.json: r7 render_ms_best=30.984 avg=31.035 gpu=30.706, samples=16777216, 10/10 vs r6 (-0.2925ms), speedup 3.539x, 99.1% GPU, 546M samples/s
- d2 7-7: ledger r4 45.220 / r5 ~44.0 / r6 36.283 / r7 30.984; stop if r7 AND r8 both <5%; r7 was 0.93% so counter=1
- d3 7-7: writer log tail (round 7 kept f32 node -0.44%, shadow payload -0.49%, 12B sample buffer -0.19%, combined -1.0% 18/18; rejected stk16 +27%, 2spp/thread +1.08%, leaf2 +0.9%; BVH TOPOLOGY is part of the frozen estimator - MAX_DEPTH 16->20 changes alt-view PFM).

## h-3133-1 (art-003133 #38)

Q: In the round-8 E0 profile (scratch _r8p/ build with PT_SPLIT=1), what render_ms_best (wall time) did the default configuration, with no diagnostic flags, print?

Truth: #38 cfg='' '[split] trace_gpu_ms=30.329' then 'render_ms_best=31.236 render_ms_avg=35.855 gpu_ms_best=0.425'. Second pass, secondary fact (trace 30.30 and the depth sweep are in working memory r8-cost-split; the wall time is not). Not in the checkpoint's working memory (art-003130) or live history (checked: 31.236, 35.855, 27.593, 7.204).

- d1 38-38: COST SPLIT 512x512/64spp trace_gpu_ms: d1=5.818, d2=10.956, d3=17.095, d12=30.275; PT_DIAG_NOSHADOW=26.682 (shadow walks only 3.65ms); combine gpu_ms_best=0.420-0.425.
- d2 34-38: _r8p hooks PT_DEFINE, PT_DIAG_DEPTH, PT_DIAG_NOSHADOW. trace_gpu_ms: d1=5.818, d2=10.956, d3=17.095, d12=30.275; noshadow=26.682 -> shadow walks 3.65ms (12%).
- d3 38-38: cost split at 512x512/64spp trace_gpu_ms: d1=5.82 d2=10.96 d3=17.10 d12=30.28; PT_DIAG_NOSHADOW=26.68 (shadow walks 3.65ms=12%)
