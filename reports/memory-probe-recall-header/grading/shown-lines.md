# Index lines the A1 header showed, per draw and checkpoint

## d1 art-000648

### art-000414: 16 of 18 lines shown

- 1-1: round-2 goal text (gates: RMSE default 0.848/alt 0.755/closeup 2.058, caustic mean 2.224/2.252/3.043, 4x-spp ratio<=0.6, target beat 109.656/109.397ms, snapshot to pt_round_02, append writer log).
- 3-11: dir layout: phase3 has pathtracer/ (include,src,shaders), pt_round_01/, scene-default/alt-view/caustic-closeup.json, out/, logs, pathtracer_benchmark.json.
- 13-13: pt_trace.metal round-1 source (PCG RNG pt_pcg_next/pt_random/pt_seed_pixel, slab AABB, Moller-Trumbore, 64-entry stack traversal, plane+one-sided light rect+mesh intersect, pt_occluded via full intersect w/ test_light=false).
- 15-15: pt_integrator.metal round-1 (Schlick Fresnel, pt_refract, pt_sample_light, cosine sampler, NEE formula albedo/pi*Le*cos_i*cos_l*A/d^2, RR depth>=4 survival clamp(max throughput,0.05,0.95), max_depth 12, per-sample atomic counter, one thread per pixel looping spp).
- 17-17: round-1 pt_gpu_types.hpp (40B PtBvhNode bmin/bmax/start/count/left/right, PtVertex, PtParams field list).
- 23-23: round-1 pt_metal_host.mm (concatenates pt_trace->pt_integrator->pt_tonemap after prepending <metal_stdlib>+header, newLibraryWithSource, dispatchThreads with maxTotalThreadsPerThreadgroup as X, one command buffer trace+tonemap, GPUStartTime/EndTime, shared buffers).
- 25-25: pt_main.cpp (CLI args, setup outside timed region, timed loop render_frame, prints render_ms_best... line, writes ppm/pfm).
- 27-27: pathtracer_benchmark.json round-1: render_ms_best=109.656 gpu_ms_best=109.397 avg 118.310, samples 16777216, 153M samples/s, determinism sha1 87c956f....
- 29-29: scene-default.json (512x512 spp64 max_depth12, camera (0,0.28,0.95)->(0,0.13,0) fov38, bunny scale 2 translate (0.0337,0,0.0031), floor 0 albedo .8, env .02, light center (0.12,0.8,-0.3) size .08 normal (0,-1,0) radiance 650/590/510).
- 31-31: round-1 writer log (design, bug history: shader order, ptr<> unsupported, NSUInteger, refract sign, inside_glass inverted, correlated phi/polar, fresnel eta, self-occluding shadow ray; light placement chosen to hide direct pool; next-round targets SAH/compact nodes/short stack/no per-sample atomics).
- 33-33: pt_mesh.hpp (Mesh = vertices+triangle indices; load_obj_mesh computes averaged vertex normals).
- 35-37: run tool needs {command}; verification dir /Users/.../pathtracer-bunny-phase3-verification is NOT readable (sandbox Operation not permitted) so no local RMSE vs reference.
- 39-39: pt_metal_host.hpp round-1 interface (GpuMeshData w/ vertices/triangles/nodes/order).
- 41-41: exact build command works (BUILD_OK).
- 43-45: baseline re-measured on this machine: render_ms_best=105.947 render_ms_avg=113.523 gpu_ms_best=105.574; /tmp not writable (ppm error) -> write outputs to phase3/out.
- 46-52: created out/ dir; saved working-memory entries baseline-timing and round2-plan.

### art-000507: 15 of 16 lines shown

- 58-61: wrote new pt_gpu_types.hpp (32B PtBvhNode, PtTri origin/e1/e2, PtTriNormals, PtParams); added PT_BVH_LEAF_BIT 0x80000000, stack 20, max depth 16.
- 63-63: pt_bvh.hpp new Bvh{nodes,triangles,normals} in leaf order.
- 65-67: first binned-SAH pt_bvh.cpp (had wrong right-bounds sweep, rewritten).
- 69-69: pt_metal_host.hpp GpuMeshData{nodes,triangles,normals}.
- 71-71: pt_metal_host.mm rewrite: 5 buffers, params as constant, 2D 16x16 groups, memset counter per frame.
- 73-73: pt_trace.metal rewrite: pre-order nodes (left=index+1, a=right child, b=leafbit|count), packed tri soup, normals only on closest hit, dedicated any-hit shadow traversal, slab test pt_ray_aabb.
- 75-75: pt_integrator.metal rewrite: constant params, per-thread atomic sample counter (+spp once), same RNG order/PCG32, NEE on floor only, Fresnel/TIR, RR depth>=4.
- 77-77: pt_main.cpp passes bvh.nodes/triangles/normals to upload_mesh.
- 79-81: build failed: PtVertex was dropped from shared header; re-added it (host-only).
- 83-87: link error: transform_mesh missing from rewrite; restored it.
- 89-89: build OK.
- 91-91: smoke spp8 OK samples=2097152 but bvh_nodes=7107 (suspicious).
- 93-93: BENCHMARK REGRESSION render_ms_best=4416.3 gpu_ms_best=4413.7 vs 105.9 baseline.
- 95-95: wrote out/bvh_stats.cpp scratch tool (leaf histogram, max depth).
- 98-100: memory updates for regression + open bugs (shadow t_limit missing; SAH degenerate leaves).

### art-000557: 14 of 15 lines shown

- 107-109: pt_round_01/src/pt_bvh.cpp is the round-1 median-split-on-widest-axis builder (nth_element on centroid, kLeafSize 4, node b>0 = leaf) that produced the 105ms tree.
- 111-117: replaced my binned-SAH partition with that median split in live src/pt_bvh.cpp; kept 32-byte pre-order nodes + packed triangle soup (PtTri origin/e1/e2 + separate PtTriNormals table); rewrote file header/comments.
- 113-113: fixed shadow-ray bias bug - pt_shadow_hit now requires t < t_limit before returning true.
- 119-119: bvh_stats on median split: nodes=40597 internal=20298 leaves=20299 max_leaf=4 mean_leaf=3.42 max_depth=15 (cap 16 not binding).
- 121-121: median-split + 16x16 groups: render_ms_best=103.290 gpu_ms_best=102.951.
- 123-125: PT_BVH_STACK_SIZE 20->16 and PT_BVH_MAX_DEPTH 16->15 REGRESSED to 119.866/119.359.
- 127-129: reverted to stack 20/cap 16: 101.771 best, gpu 101.237, avg 114.621.
- 131-133: MTLCompileOptions fastMathEnabled=YES gave 101.150/gpu 100.887 (neutral, deprecated in macOS 15 warning).
- 135-143: root-AABB exit pruning (pt_ray_aabb_exit + t_limit=min(best_t,root_exit)) in nearest and shadow traversals REGRESSED: 107.004 best / 125.973 avg.
- 145-151: fully reverted root-exit pruning; 103.410 best / 109.625 avg / gpu 103.098.
- 153-165: added PT_TG env knob in pt_metal_host.mm for threadgroup shape (group_w/group_h, clamped by maxTotalThreadsPerThreadgroup) plus PT_DEBUG logging.
- 167-167: shape sweep best-of-3: 8x8=101.0, 16x8=96.4/gpu93.6, 16x16=100.5, 32x8=104.6, 16x32=110.0, 32x16=118.8.
- 169-169: more shapes best-of-4: 8x16=94.4, 32x4=101.3, 16x4=91.4, 4x32=93.3, 16x8=90.5 and 96.6 -> default 16x8; noise about +/-5ms.
- 171-173: default group set to 16x8; snapshot of that state copied to out/pt_snapshot_pre_stackless.

### art-000646: 0 of 0 lines shown


## d1 art-001768

### art-001608: 14 of 14 lines shown

- 1-1: round-5 goal text (candidates, constraints, gates 0.848/0.755/2.058, best 45.220/44.907)
- 3-3: phase3 dir listing (pt_round_01..04, ab_r3, _ab.sh, scene-*.json, out/)
- 5-5: writer log tail: rounds 1-4 history, kept/rejected lists, round-5 ideas (normals one-cache-line, ballot, 256spp harness, child ordering)
- 7-7: file sizes: pt_trace.metal 506L, pt_integrator.metal 258L, pt_bvh.cpp 430L
- 10-10: pt_trace.metal full source (qray slab test, payload carry, near-first walks, pt_intersect_scene, pt_occluded)
- 12-12: pt_gpu_types.hpp (PtTri 48B padded, PtTriNormals 36B separate, PtBvhNodeQ 16B, PtParams incl pad field)
- 14-14: pt_integrator.metal full source (fresnel/refract asymmetry note, NEE block, glass branch, morton decode, trace_kernel)
- 16-16: _ab.sh + _abenv.sh harness usage; benchmark.json round-4 numbers 45.220/44.907, exact compile cmd
- 18-18: ab_r3 = frozen round-3 base layout (binary+include+shaders); live build OK w/ fastMath warning
- 20-20: ab_r4 frozen base created; binary md5 5c318568 == pt_round_04
- 22-22: live sources diff-identical to pt_round_04
- 24-24: mesh path typo (must be /Users/peihongguo/Code/aos/reports/rasterizer-bunny/assets/stanford-bunny.obj)
- 27-27: round-4 reference PFMs out/r4_{default,alt-view,caustic-closeup}.pfm md5 53600ca.../1f0de1.../0e38e7...
- 29-29: pt_metal_host.mm (single dispatch+wait+readback in timed region, PT_TG knob, runtime shader concat order trace,tonemap,integrator)

### art-001661: 18 of 18 lines shown

- 30-30: no offline metal compiler (metal toolchain missing); occupancy max_threads_per_group trace=1024
- 31-36: made scratch profiling copy _prof/ (params.pad <- PT_PROF env in pt_scene.cpp)
- 43-50: added 4 biased PT_PROF bits in _prof shaders (1 no shadow, 2 no mesh, 4 no NEE, 8 glass always-reflect)
- 54-56: shader compile error 'undeclared params' in pt_intersect_scene -> use scene.params->pad
- 58-58: PT_PROF cost shares gpu_ms_best: full 45.35, no shadow 44.05, no NEE 43.17, no mesh 4.77, glass always-reflect 35.76 -> closest-hit BVH walk is ~89% of frame
- 59-64: glass refract-skip edit; caught two RNG-stream bugs (short-circuit !refracted||pt_random means TIR consumes 0 draws; reflectance==1 path consumes exactly 1)
- 66-66: glass edit bit-identical on all 3 scenes
- 68-76: A/B base ab_r4 produced no output: cp -R nesting bug created ab_r4/include/include so round-3 header (float[3] PtTri) mismatched round-4 shaders -> silent shader compile failure
- 78-78: glass micro-opt REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base; ior 1.5 means Fresnel TIR guard never fires (dead branch)
- 80-80: reverted pt_integrator.metal to round-4 text
- 81-84: entered-distance stack design + fixed control-flow bug (prune continue left stale node_index; replaced with pop loop + resumed flag)
- 86-86: entered-stack bit-identical on default/alt-view/caustic-closeup
- 88-88: A/B entered-stack: base 44.493 min/46.042 mean vs cand 44.425/45.607, 6/8 pairs -> KEEP
- 90-96: added stack_payload (push child payload too, pop needs no global load); bit-identical 3 scenes
- 98-98: built _e/ = entered-stack-only base (stack_payload lines stripped with python)
- 100-108: wrote working memory: round5-refs (ab_r4 base, out/r4_*.pfm refs, _ab.sh harness, exact build cmd), cost shares, glass rejection + stack keep
- 110-110: A/B stack_payload vs _e: base 43.875/45.583 vs cand 43.226/44.906, 5/8 pairs -> KEEP
- 112-118: read pt_gpu_types.hpp (16B quantized node, outward quantization); built _b/ snapshot of kept state (entered-stack + stack_payload)

### art-001766: 10 of 10 lines shown

- 122-122: pt_metal_host.mm head — buffers, upload_mesh, render_frame start, PT_TG threadgroup knob (32x4 default)
- 124-124: full pt_trace.metal read — pt_intersect_mesh and pt_shadow_hit with entered-distance + stack_payload stacks, box tests with `exited > 0`
- 126-126: pt_main.cpp 138-195 — quantize_bvh call, GpuMeshData build, params fill, timed region start
- 128-134: child-pair header edit; PtBvhNodeQ delete failed once (text mismatch), removed with python by line numbers
- 136-144: quantize_bvh rewritten to emit 32-byte child-pair blocks (pre-order block index, leaves carry no block); kInvalidNode added; pt_bvh.hpp/pt_main.cpp/pt_metal_host.mm switched to PtBvhChild
- 146-146: both GPU walks rewritten for child-pair layout (nodes indexed payload*2, stack of (payload, entered), root box test dropped)
- 153-155: build error — pt_metal_host.hpp GpuMeshData::nodes still PtBvhNodeQ*; sed fixed to PtBvhChild*
- 157-157: clean build
- 159-159: wrong scene path scenes/scene-default.json — scenes live at top level (scene-default.json etc.)
- 163-163: child-pair PFMs byte-identical on default/alt-view/caustic-closeup, samples=16777216

## d1 art-003135

### art-003062: 6 of 6 lines shown

- 1-1: round-8 goal (stop-rule round; ranked experiments 1 work-item reordering, 2 tile re-sweep, 3 quantized normals, 4 occupancy; bit-identity gate on 3 scenes; snapshot pt_round_08; frozen r7 = 30.984ms/30.706gpu, 3.539x)
- 3-3: phase3 dir listing (pt_round_01..07, scene-default/alt-view/caustic-closeup.json, many _r7* scratch files)
- 7-7: benchmark.json tail (r7 vs r6 10/10 pairs, mean -0.2925ms; gpu 99.1% of wall; samples 546.4M/s) + coordinator ledger rounds 4-7 (all PASS; r7 best verified)
- 9-9: writer log tail (r7 kept f32 node -0.44%, shadow payload -0.49%, 12B sample buffer -0.19%; rejected stk16 +27%, spt2 +1.08%, leaf2 +0.9%; CRITICAL BVH topology frozen by strict t<best_t tie-break) + file sizes (pt_trace.metal 552L, pt_integrator.metal 367L, pt_metal_host.mm 309L)
- 11-11: full pt_integrator.metal (pt_morton_decode 5 bits/tile 32, pt_tiled_pixel, trace_kernel classic, trace_samples_kernel grid(tiles*32,tiles*32,spp) sample-major store, combine_kernel ordered sum, fresnel/refract/NEE/cosine sampling)
- 13-13: pt_gpu_types.hpp (PtBvhNodeQ = float lo[4]+hi[4] = 32 BYTES despite the '16-byte' comment, PtTri 48B padded, PtTriNormals 36B separate table, PtSample3 12B, PT_BVH_STACK_SIZE 20, PT_BVH_MAX_DEPTH 16) + pt_metal_host.mm (PT_TG/PT_CLASSIC/PT_DEBUG env knobs, grid rounded to 32, group 32x4, trace pipeline limit 1024, sample buffer cap 1.5GB, one command buffer, gpu_ms from GPUStartTime)

### art-003133: 12 of 12 lines shown

- 21-21: _r8b frozen round-7 A/B binary built OK (only fastMathEnabled deprecation warning)
- 23-25: _r8p profiler: PT_SPLIT=1 splits trace/combine into two command buffers, prints trace_gpu_ms; trace=30.3ms, combine=0.42ms
- 30-30: writer log grep - line 163 shows round-7 REJECTED 32-byte sibling-pair BVH blocks as a wash (+0.104ms +-0.218, n=41)
- 32-32: OBJ has ZERO 'vn' lines -> normals are host-computed, so a 16-bit quantized normal table cannot be bit-exact (E3 dead)
- 36-38: _r8p env hooks PT_DEFINE (inject shader #defines) and PT_DIAG_DEPTH; cost sweep d1=5.82 d2=10.96 d3=17.10 d12=30.28 ms; PT_DIAG_NOSHADOW=26.68 -> shadow walks only 3.65ms (12%)
- 40-40: quantize_bvh in pt_bvh.cpp clamps every quantized coordinate to <=65535 and the scale is a power of two, writing them as float32
- 42-42: PtBvhNodeQ lives in include/pt_gpu_types.hpp; upload_mesh sizes the buffer by sizeof(PtBvhNodeQ)
- 44-46: E5 edit - PtBvhNodeQ to six u16 + u32 payload (16 bytes), host writes integers, shader pt_load_node reads one uint4 and float(u16)
- 50-50: E5 bit-IDENTICAL PFM on scene-default, alt-view, caustic-closeup at 64spp seed 1; build clean
- 52-52: E5 paired A/B n=16: wins 0/16, mean +0.2796ms, geo +0.90%, sd_log 0.00221, t=16.25 -> REJECTED, walk is ALU/latency-bound not node-bandwidth-bound
- 54-54: E5 candidate preserved in _r8_node16/; pathtracer/ reverted from _r8b and PFM confirmed byte-identical
- 56-56: maxTotalThreadsPerThreadgroup for the samples pipeline = 1024 (no occupancy lever); created _r8_abenv.sh

## d2 art-000648

### art-000414: 14 of 18 lines shown

- 1-1: round-2 task goal (gates: RMSE <=1.1x baseline 0.848/0.755/2.058, caustic mean in [0.8,1.25]x 2.224/2.252/3.043, samples==w*h*spp, 4x-spp RMSE ratio <=0.6, bit-identical PFM, no forbidden APIs, host threads <=8, snapshot to pt_round_02, log entry required)
- 3-11: dir layout: phase3 has pathtracer/ (include,src,shaders), pt_round_01/, scene-default/alt-view/caustic-closeup.json, logs; shaders = pt_trace/pt_integrator/pt_tonemap.metal
- 13-13: round-1 pt_trace.metal: PCG rng, pt_seed_pixel(seed,pixel,sample), slab AABB test, Moller-Trumbore w/ vertex indirection, 64-entry stack traversal, floor plane + one-sided light rect + mesh, occlusion via full intersect_scene(test_light=false)
- 15-15: round-1 pt_integrator.metal: Schlick fresnel, pt_refract, NEE on floor only ((albedo/pi)*Le*cos_i*cos_l*A/d^2), cosine diffuse sampler, glass BSDF w/ (n_t/n_i)^2, RR at depth>=4 survival=clamp(max throughput,0.05,0.95), kernel = 1 thread/pixel spp loop + atomic per sample
- 17-17: old pt_gpu_types.hpp: 40-byte node {bmin,bmax,start,count,left,right}, PtVertex, PtParams field list
- 19-21: round-1 BVH: median split on widest axis, leaf size 4, 40597 nodes for 69451 tris, no SAH
- 23-23: round-1 pt_metal_host.mm: buffers, newLibraryWithSource concatenating header+3 .metal in fixed order, dispatchThreads with maxTotalThreadsPerThreadgroup (1024-wide groups), one command buffer trace+tonemap, commit+waitUntilCompleted, GPUStartTime/EndTime, shared readback
- 25-25: pt_main.cpp: timed region = render_frame only; setup (mesh, bvh, metal init, upload, allocate) outside; prints benchmark line
- 27-27: pathtracer_benchmark.json round-1: render_ms_best=109.656 gpu_ms_best=109.397, samples=16777216, determinism sha1 87c956f4..., ~153M samples/s, GPU-bound (0.26ms host overhead)
- 29-29: scene-default.json: 512x512 spp64 depth12, cam (0,0.28,0.95)->(0,0.13,0) fov38, bunny scale 2 translate (0.0337,0,0.0031), floor 0 albedo 0.8, env 0.02, light center (0.12,0.8,-0.3) size 0.08 normal (0,-1,0) radiance (650,590,510)
- 31-31: round-1 writer log: design notes, fixed bugs (shadow self-occlusion, refract sign, inside_glass, correlated sampler angles, fresnel eta), caustic mask mean 9.05 over 1185px, listed next-round targets (SAH, compact nodes, short stack, no per-sample atomics)
- 33-39: pt_mesh.hpp (Mesh{vertices,triangles}), pt_metal_host.hpp interface (init/upload_mesh/allocate/render_frame)
- 35-37: run tool needs {command}; verification dir /Users/.../pathtracer-bunny-phase3-verification is NOT readable (Operation not permitted) so I cannot compute RMSE locally
- 41-41: exact build command works (BUILD_OK)

### art-000507: 13 of 14 lines shown

- 58-61: wrote new pt_gpu_types.hpp (32B node bmin/bmax/a/b, PtTri origin+e1+e2, PtTriNormals, stack 20, max depth 16, PT_BVH_LEAF_BIT)
- 63-67: rewrote pt_bvh.hpp/cpp with binned SAH (16 bins) + pre-order emission + packed triangle soup
- 69-71: rewrote pt_metal_host.hpp/.mm: buffers nodes/triangles/normals, params constant, 16x16 groups, atomic counter
- 73-73: rewrote shaders/pt_trace.metal: pre-order traversal (left=index+1), dedicated pt_shadow_hit any-hit
- 75-75: rewrote shaders/pt_integrator.metal: same RNG order, per-thread atomic sample count
- 77-77: pt_main.cpp updated for new GpuMeshData
- 79-79: build error PtVertex undeclared -> 81 re-added PtVertex to shared header
- 83-83: link error transform_mesh missing -> 87 restored it
- 89-89: build OK
- 91-91: smoke spp8 OK samples=2097152 but bvh_nodes=7107 (bad)
- 93-93: BENCHMARK REGRESSION render_ms_best=4416.3 gpu=4413.7 (binned SAH broken)
- 95-95: wrote scratch out/bvh_stats.cpp (leaf histogram, max depth)
- 98-100: memory updates on regression + shadow t_limit bug

### art-000557: 13 of 20 lines shown

- 107-107: phase3 dir listing; pt_round_01 snapshot holds round-1 sources
- 109-109: pt_round_01/src/pt_bvh.cpp = median-centroid split on widest axis, kLeafSize 4, 40-byte nodes (the 105ms builder)
- 111-111: live pt_bvh.cpp partition replaced binned-SAH -> median split
- 113-113: shadow-ray bug fix: added `t < t_limit` in pt_shadow_hit
- 115-117: full rewrite of pt_bvh.cpp (median split, pre-order emit, packed PtTri + PtTriNormals)
- 119-119: bvh_stats: nodes=40597 internal=20298 leaves=20299 max_leaf=4 mean_leaf=3.42 max_depth=15
- 121-121: bench 5f: render_ms_best=103.290 avg=113.313 gpu=102.951
- 123-125: PT_BVH_STACK_SIZE 20->16 + cap 15 REGRESSED: 119.866/126.379 gpu 119.359
- 127-129: reverted to stack 20/cap 16: 101.771/114.621 gpu 101.237
- 131-133: MTLCompileOptions fastMathEnabled=YES: 101.150/116.465 gpu 100.887 (neutral; deprecated in macOS 15, use mathMode)
- 135-141: added pt_ray_aabb_exit + root-exit t_limit pruning in nearest and shadow traversals
- 143-143: that REGRESSED: 107.004/125.973 gpu 106.738
- 145-151: root-exit fully reverted; bench 103.410/109.625 gpu 103.098

### art-000646: 13 of 21 lines shown

- 196-196: pt_bvh.cpp link_escapes terminator changed to PT_BVH_NO_NODE
- 198-198: read of stack-based pt_trace.metal before rewriting
- 200-200: wrote STACKLESS pt_trace.metal (escape links in node.b low 20 bits, leaf count bits 25..20)
- 202-202: benchmark run TIMED OUT at 60s = GPU hang
- 204-204: smoke test EXIT=1 was grep's status, inconclusive
- 206-206: PROG_EXIT=142 alarm, no PPM -> confirmed GPU infinite loop
- 208-208: root cause: build error 'link_escapes is private' (#216 in file) left a STALE ./pathtracer while shaders were new; bvh_stats output nodes=40597 internal=20298 leaves=20299 max_leaf=4 mean_leaf=3.42 max_depth=15
- 210-210: made link_escapes public in pt_bvh.cpp
- 212-212: clean rebuild + smoke spp2 OK (PROG_EXIT=0, PPM 786447 bytes)
- 214-214: STACKLESS benchmark render_ms_best=102.084 avg=114.592 gpu_ms_best=101.802
- 216-216: pre-stackless A/B twice: 93.366/103.183 gpu 92.993 and 96.614/100.692 gpu 92.915
- 218-218: REVERTED stackless; copy kept at pathtracer_backup_stackless; live = out/pt_snapshot_pre_stackless, PT_BVH_STACK_SIZE 20
- 220-220: full read of pt_integrator.metal (pt_fresnel, pt_refract, pt_sample_light, pt_sample_cosine, pt_trace_path NEE+RR, trace_kernel)

## d2 art-001768

### art-001608: 13 of 13 lines shown

- 1-1: task goal round 5 (beat 45.220ms/44.907ms gpu, round-4 pt_round_04; constraints, rejected list, candidate list)
- 3-3: workspace listing (ab_r3, pt_round_01..04, _ab.sh harness files, scenes)
- 5-5: writer log tail: rounds 1-4 history, kept/rejected lists, round-5 ideas (normal interleave, ballot, SAH child order)
- 7-7: source layout: shaders pt_trace 506L, pt_integrator 258L, pt_tonemap 22L; src pt_bvh.cpp 430L, pt_main 252L, pt_metal_host.mm 244L
- 10-10: pt_trace.metal full text: qray/quantized slab test fma, payload carry, near-first both-child walk, shadow walk, intersect_scene, occluded
- 12-12: pt_gpu_types.hpp: PtTri 48B padded float[4], PtTriNormals 36B separate table, PtBvhNodeQ 16B, PtParams fields incl pad
- 14-14: pt_integrator.metal: fresnel/refract asymmetry comment, NEE block, glass branch RNG order (!refracted || random<F), morton tile 32, trace_kernel
- 16-16: _ab.sh usage (base cand rounds frames spp) + benchmark.json round4 best line 45.220/44.907
- 18-18: build cmd OK; ab_r3 = round-3 base dir (include/shaders/pathtracer)
- 20-20: ab_r4 created (later found nested bug); binary md5s
- 22-22: live sources == pt_round_04 sources diff-clean
- 24-24: wrong mesh path typo (missing reports/) -> mesh error
- 27-27: round-4 ref PFMs out/r4_*.pfm: default md5 53600ca347f0b76ab1a0ff894f6a8f4e, alt-view 1f0de145..., caustic 0e38e718...

### art-001661: 26 of 26 lines shown

- 31-31: no offline metal compiler check; xcrun metal exists under Xcode.app
- 33-33: PT_DEBUG prints max_threads_per_group trace=1024; Xcode metal toolchain missing (cannot execute tool 'metal')
- 36-40: made scratch copy _prof/ of pathtracer; params.pad set in pt_scene.cpp:206 (fill_params end)
- 42-48: _prof patches: PT_PROF env -> params.pad; bit0 skip shadow walk, bit1 skip mesh intersect, bit2 skip NEE block
- 50-50: bit3 glass always-reflect patch in _prof/shaders/pt_integrator.metal
- 52-54: first sweep printed nothing; _prof binary built fine, failure was shader compile
- 56-56: bit1 fix: pt_intersect_scene has no local params, use scene.params->pad
- 58-58: COST SHARES 512x512/64spp gpu_ms_best: full 45.35, no-shadow 44.05, no-mesh 4.77 (BVH walk ~89%), no-NEE 43.17, glass always-reflect 35.76
- 60-62: glass refract-skip attempt; caught two RNG-stream bugs (short-circuit !refracted || pt_random means TIR consumes no draw)
- 64-64: fixed to consume exactly one draw when reflectance>=1
- 66-66: candidate bit-identical on all 3 scenes (out/c1_*.pfm vs out/r4_*.pfm)
- 68-68: A/B produced empty base line -> suspicious
- 70-75: base ab_r4 silently failed shader compile (no stdout, exit 0)
- 76-76: ROOT CAUSE: cp -R nesting created ab_r4/include/include so round-3 header was used; rebuilt ab_r4 flat, verified it prints a benchmark line
- 78-78: GLASS MICRO-OPT REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base
- 80-80: reverted integrator to round-4 text
- 82-84: entered-distance stack written; caught control-flow bug (stale node_index on continue) and replaced pop with inner pop loop
- 86-86: entered-stack bit-identical on 3 scenes
- 88-88: entered-stack A/B: base 44.493 min/46.042 mean vs cand 44.425/45.607, 6/8 pairs favour cand -> KEPT
- 90-94: added stack_payload (push child payload too, pop does no global load)
- 96-96: stack_payload bit-identical on 3 scenes
- 98-98: built _e/ = entered-stack-only base (stack_payload lines stripped by python)
- 100-108: working memory written (round5-refs, cost shares, glass rejection + stack keep)
- 110-110: stack_payload WIN: base(_e) 43.875 min/45.583 mean vs cand 43.226/44.906, 5/8 pairs, mean -0.68ms -> KEPT
- 112-114: reviewed node encoding (16B quantized PtBvhNodeQ, pre-order left=index+1, payload=right)
- 118-118: snapshot _b/ built = entered-stack + stack_payload (current kept state, revert point)

### art-001766: 6 of 6 lines shown

- 122-126: host plumbing read — node_buffer upload, render_frame start, PT_TG env knob, quantize_bvh call site, GpuMeshData fields
- 128-134: pt_gpu_types.hpp — added PtBvhChild (16B lo0/hi0/lo1/hi1/lo2/hi2/payload), deleted PtBvhNodeQ (edit_file whitespace mismatch; removed via python line slice)
- 136-144: quantize_bvh rewritten to emit 32-byte child-pair blocks (pre-order block index per internal node, leaves carry no block, payload = leaf range or child block index); kInvalidNode added; pt_bvh.hpp/pt_main.cpp/pt_metal_host.mm switched to PtBvhChild
- 146-146: both GPU walks rewritten for child-pair layout (nodes indexed payload*2 and +1; stack = (payload, entered); root box test dropped as provably redundant)
- 153-155: build errors — GpuMeshData::nodes still typed PtBvhNodeQ* in pt_metal_host.hpp; fixed to PtBvhChild*
- 157-163: child-pair build clean; default-scene PFM byte-identical to round 4, samples=16777216; scene configs are at top level (scene-default.json), NOT scenes/ — wrong path gave 'cannot open scene config'

## d2 art-003135

### art-003062: 6 of 6 lines shown

- 1-1: round-8 goal: beat r7 30.984ms render/30.706 gpu (3.539x of 109.656); bit-identical PFM gate on 3 scenes; paired A/B n>=15 vs frozen _r8b; snapshot pt_round_08; append '== Round 8 ==' to writer log.
- 3-7: dir has pt_round_01..07, _r7b, 3 scene jsons, benchmark.json, 3 logs, _r7_ab.sh/_r7_pair.pl. benchmark: gpu=99.1% of wall (0.278ms host), 546.4M samples/s, speedup 3.539. Ledger: r6 36.283, r7 30.984 (+0.93% win, sub-5 counter=1); stop rule = r7 AND r8 both <5%.
- 9-9: writer log r7: kept f32 node -0.44%, shadow stack payload -0.49%, 12B PtSample3 -0.19%; rejected stk16 +27.1%, spt2 +1.08%, leaf1 +0.4%; BVH TOPOLOGY IS THE ESTIMATOR (MAX_DEPTH 16->20 changed alt-view PFM) - leaf/depth/axis/bin changes forbidden.
- 11-11: integrator: pt_seed_pixel(seed,pixel,sample); morton_decode 5 bits/axis 32-tile; trace_samples_kernel grid(tiles*32,tiles*32,spp) writes samples_out[sample*pixels+index]; combine_kernel sums sample order + atomic count.
- 13-13: pt_gpu_types.hpp: PtBvhNodeQ = float lo[4]+hi[4] = 32 BYTES (comment claiming 16 is stale); PtTri 48B; PtTriNormals 36B separate; PtSample3 12B; STACK 20; MAX_DEPTH 16.
- 13-13: pt_metal_host.mm: startup shader compile (pt_trace,pt_tonemap,pt_integrator + header, fastMathEnabled), env knobs PT_TG=WxH (no rebuild), PT_CLASSIC, PT_DEBUG (max threads/group=1024); grid rounds to 32; sample buffer cap 1.5GB; one command buffer trace+combine.

### art-003133: 18 of 18 lines shown

- 19-19: _r7_ab.sh + _r7_pair.pl contents and the three scene-*.json files (camera/light params) printed
- 21-21: _r8b/ frozen round-7 baseline built OK (only fastMathEnabled deprecation warning)
- 23-23: _r8p/ profiler: PT_SPLIT splits trace and combine into two command buffers
- 25-25: first split measurement trace_gpu_ms=30.8/30.3, combine gpu_ms_best=0.42
- 27-27: full pt_trace.metal 330-552: near-first child order, pop-skip payload, pt_shadow_hit, pt_intersect_scene (floor plane, light rect, mesh)
- 30-30: writer-log grep: line 163 = sibling 32-byte node pairs already measured a wash in an earlier round
- 32-32: stanford-bunny.obj has ZERO vn lines -> normals are host-computed smooth normals, so 16-bit normal quantization cannot be bit-exact
- 34-36: _r8p diag patches: PT_DIAG_NOSHADOW in pt_occluded, host hooks PT_DEFINE (inject shader defines) and PT_DIAG_DEPTH
- 38-38: cost split at 512x512 64spp trace_gpu_ms: d1=5.82 d2=10.96 d3=17.10 d12=30.28, noshadow=26.68
- 40-40: pt_bvh.cpp quantize_bvh: power-of-two scale, coordinates clamped to <=65535, payload packed, floats stored as float32
- 42-42: where PtBvhNodeQ/GpuMeshData appear (pt_gpu_types.hpp:51, pt_bvh.cpp:360, pt_main.cpp:148, pt_metal_host.mm:164)
- 44-48: E5 edits: PtBvhNodeQ -> 6 u16 + u32 payload aligned 16, host writes integers, shader pt_load_node reads one uint4 and does float(u16)
- 50-50: E5 PFM byte-identical on scene-default, alt-view, caustic-closeup at 64spp seed 1
- 52-52: E5 paired A/B n=16: 0/16 wins, +0.2796ms, geo +0.90%, t=16.25 -> REJECTED
- 54-54: E5 preserved in _r8_node16/, pathtracer/ reverted from _r8b, PFM byte-identical confirmed
- 56-56: _r8_abenv.sh written; maxTotalThreadsPerThreadgroup trace=1024 sample_parallel=1
- 58-60: _r8e scratch tree + PT_DEFINE host hook; PT_MORTON_T decode swap; trace_lanes_kernel and combine_lanes_kernel appended
- 63-69: working-memory entries created; update_memory evidence must be a numeric message range like 50-56

## d3 art-000648

### art-000414: 13 of 15 lines shown

- 1-1: round-2 goal text (gates: RMSE<=1.1x baseline 0.848/0.755/2.058, caustic mean in [0.8,1.25]x 2.224/2.252/3.043, samples==w*h*spp, 4x-spp RMSE ratio<=0.6, bit-identical PFM, no forbidden APIs, host<=8 threads, target beat 109.656/109.397ms).
- 3-11: dir listings: phase3 has pathtracer/, pt_round_01/, scene-default/alt-view/caustic-closeup.json, logs, out/; pathtracer has include/{pt_bvh,pt_gpu_types,pt_image,pt_math,pt_mesh,pt_metal_host,pt_parallel,pt_scene}.hpp, src/{pt_bvh,pt_image,pt_main,pt_mesh,pt_parallel,pt_scene}.cpp+pt_metal_host.mm, shaders/{pt_trace,pt_integrator,pt_tonemap}.metal.
- 13-13: pt_trace.metal round-1: PCG-XSH rng (pt_pcg_next/pt_random/pt_seed_pixel), slab AABB test, Moller-Trumbore, 64-entry stack traversal, floor plane + one-sided light rect + mesh intersect, pt_occluded via full intersect with test_light=false.
- 15-15: pt_integrator.metal round-1: Schlick fresnel, pt_refract, pt_sample_light (2 randoms), pt_sample_cosine (2 randoms), path loop w/ NEE on floor only, emission on depth0||last_specular, RR depth>=4 survival=clamp(max throughput,0.05,0.95), trace_kernel 1 thread/pixel spp loop, atomic per sample.
- 17-17: round-1 pt_gpu_types.hpp: PtVertex(pos+normal), PtBvhNode 40B (bmin,bmax,start,count,left,right), PtParams field list (unchanged in my rewrite).
- 23-23: round-1 pt_metal_host.mm: shader concat order trace->integrator->tonemap after prepending metal_stdlib + pt_gpu_types.hpp; buffers params/verts/tris/bvh/order/radiance/image/counter; dispatchThreads with maxTotalThreadsPerThreadgroup (1024-wide groups); one command buffer trace+tonemap, waitUntilCompleted, GPUStartTime/EndTime.
- 25-25: pt_main.cpp: timed region = render_frame only; setup (OBJ parse, transform_mesh, build_bvh, metal init, upload, allocate) outside; prints benchmark line; writes PPM+PFM.
- 27-27: pathtracer_benchmark.json round-1: render_ms_best=109.656 gpu_ms_best=109.397 (4 trials), 153M samples/s, determinism sha1 87c956f..., samples check pass.
- 29-29: scene-default.json: 512x512/64spp/max_depth 12, camera (0,0.28,0.95)->(0,0.13,0), fov 38, bunny scale 2.0 translate (0.0337,0,0.0031), floor y=0 albedo 0.8, env 0.02, light center (0.12,0.80,-0.30) size 0.08x0.08 normal (0,-1,0) radiance (650,590,510).
- 31-31: round-1 writer log: design notes, fixed bugs (self-occluding shadow ray, refract sign, inside_glass, correlated cosine angles, fresnel eta), light placement rationale, known next targets (SAH, compact nodes, short stack, per-sample atomics).
- 35-37: run tool needs {"command":...}; verification dir /Users/.../pathtracer-bunny-phase3-verification is NOT readable (Operation not permitted) so RMSE must be judged indirectly.
- 46-46: created out/ dir.
- 48-52: update_memory evidence must be a range like "43-45"; stored measurements/baseline-timing and decisions/round2-plan.

### art-000507: 12 of 14 lines shown

- 58-58: wrote new pt_gpu_types.hpp (32B node, PtTri, PtTriNormals, PtParams)
- 59-61: added PT_BVH_LEAF_BIT 0x80000000 macro to shared header
- 62-63: pt_bvh.hpp: Bvh{nodes,triangles,normals} in leaf order
- 64-67: pt_bvh.cpp binned-SAH build (later replaced)
- 68-71: pt_metal_host.mm new buffers nodes/triangles/normals, params constant, 16x16 groups
- 72-75: pt_trace.metal (32B pre-order nodes, packed soup, any-hit shadow) + pt_integrator.metal (per-thread atomic sample count)
- 76-77: pt_main.cpp GpuMeshData updated
- 79-81: build error PtVertex missing -> re-added PtVertex to shared header
- 83-87: link error transform_mesh dropped -> restored
- 89-89: build OK
- 91-91: smoke spp8 samples=2097152 OK, bvh_nodes=7107 (BAD)
- 93-93: benchmark render_ms_best=4416.317 gpu_ms_best=4413.659 (40x REGRESSION)

### art-000557: 12 of 25 lines shown

- 107-107: ls of phase3 dir shows pt_round_01 snapshot exists with src/*.cpp,*.mm
- 109-109: full round-1 pt_bvh.cpp source (median-centroid split on widest axis, kLeafSize 4, nth_element, pre-order-ish push of 2 nodes, node.count/start for leaf)
- 111-111: replaced binned-SAH partition() in live src/pt_bvh.cpp with median split (kept 32B pre-order layout + packed triangle soup)
- 113-113: FIX shadow-ray bug: pt_shadow_hit now requires t < t_limit before returning true
- 115-115: read of live pt_bvh.cpp showing leftover dead SAH helpers
- 117-117: rewrote pt_bvh.cpp clean: median split, BvhBuilder class, emit_leaf a=triangle start b=LEAF_BIT|count, append_triangle stores origin/e1/e2 + separate normals table, out.nodes.reserve(2*tri_count)
- 119-119: bvh_stats output for median split: nodes=40597 internal=20298 leaves=20299 tris_packed=69451 max_leaf=4 mean_leaf=3.42, leaf-size hist 2:3915 3:3915 4:12469, max_depth=15
- 121-121: benchmark after median-split fix: render_ms_best=103.290 avg=113.313 gpu_ms_best=102.951, samples=16777216
- 123-123: tried PT_BVH_STACK_SIZE 20->16 and PT_BVH_MAX_DEPTH 16->15
- 125-125: that REGRESSED: 119.866/126.379, gpu 119.359
- 127-127: reverted to stack 20 / cap 16
- 129-129: rebuild confirms 101.771/114.621, gpu 101.237

### art-000646: 6 of 6 lines shown

- 195-200: made link_escapes public and rewrote pt_trace.metal with stackless Wald traversal (escape links in node.b low 20 bits, leaf count bits 25..20)
- 202-212: stackless build FAILED to compile ('link_escapes is private') leaving a stale ./pathtracer -> GPU infinite loop and 60s tool timeouts; fix = rm -f pathtracer and grep for error before benchmarking; smoke at spp 2 then wrote PPM
- 214-218: stackless measured 102.08 best/114.59 avg vs stack-based 93.37/96.61 (gpu 92.9) in the same A/B -> REGRESSION, reverted to out/pt_snapshot_pre_stackless; stackless copy kept at pathtracer_backup_stackless
- 220-220: read pt_integrator.metal - pt_refract and pt_sample_cosine end with a normalize() on analytically unit vectors; pt_fresnel and pt_refract each compute sin2_t
- 222-222: read pt_metal_host.mm - two encoders (trace + tone), PT_TG env knob, threadgroup_size helper
- 224-241: merged tone mapping into trace_kernel: pt_tonemap.metal became inline pt_tone_map(), host concat order pt_trace,pt_tonemap,pt_integrator, image buffer bound at index 6, tone pipeline/encoder/member deleted

## d3 art-001768

### art-001608: 15 of 15 lines shown

- 1-1: task goal round 5, best 45.220/44.907 round4, rejected/kept lists, constraints
- 3-3: phase3 dir listing (ab harnesses, pt_round_01..04, scenes)
- 5-5: writer log full history rounds1-4: per-change timings, rejected ideas, round4 final 45.635 vs r3 47.828, 2.403x
- 7-7: file layout: pt_trace.metal 506L, pt_integrator.metal 258L, pt_gpu_types.hpp shared
- 8-8: notice run non-zero exit (wc glob)
- 10-10: pt_trace.metal full text: quantized 16B nodes, payload carry, near-first both-child walk, shadow walk pop skips slab
- 12-12: pt_gpu_types.hpp: PtTri 48B padded, PtTriNormals 36B separate, PtBvhNodeQ 16B, PtParams fields incl pad
- 14-14: pt_integrator.metal: PCG rng, fresnel asymmetry note, NEE, glass branch, morton tile 32, trace_kernel
- 16-16: _ab.sh/_abenv.sh harness + benchmark.json round4 (45.220/44.907, md5 of default pfm 53600ca3..)
- 18-18: ab_r3 layout; live build OK (fastMath deprecation warning only)
- 20-20: ab_r4 frozen base binary md5 == pt_round_04 binary
- 22-22: live sources == pt_round_04 sources (diff clean)
- 24-24: failed ref render: wrong mesh path typo
- 25-25: notice non-zero exit
- 27-27: round-4 ref PFMs saved out/r4_*.pfm md5s default 53600ca347f0b76ab1a0ff894f6a8f4e alt 1f0de145.. caustic 0e38e718..

### art-001661: 15 of 15 lines shown

- 30-33: no offline metal compiler (xcrun metal missing Metal Toolchain); occupancy proxy max_threads_per_group trace=1024
- 36-50: scratch profiling copy _prof/: params.pad carries PT_PROF env; 4 biased shortcuts (1 skip shadow, 2 skip mesh, 4 skip NEE, 8 glass always-reflect)
- 44-44: pt_intersect_scene has no local 'params' - use scene.params->pad
- 58-58: PT_PROF cost shares gpu_ms_best 512x512/64spp: full 45.35, no shadow 44.05, no NEE 43.17, no mesh 4.77 (closest-hit walk ~89% of frame), glass always-reflect 35.76
- 60-64: glass refract-skip had 2 RNG-stream bugs: original `if (!refracted || pt_random(state) < reflectance)` short-circuits (TIR consumes no draw); reflectance>=1 case must consume exactly one draw
- 66-66: glass micro-opt bit-identical on 3 scenes (out/c1_*.pfm vs out/r4_*.pfm)
- 68-76: A/B base printed nothing: cp -R nested include/include so ab_r4 used round-3 header (float[3] PtTri) with round-4 shaders -> shader compile error; fixed with rm -rf + flat copies; always verify base prints a benchmark line
- 78-78: glass micro-opt REJECTED: base 43.315 min/45.632 mean vs cand 45.025/46.563, 6/8 pairs favour base; ior 1.5 means Fresnel TIR guard never fires (sin2_t<=0.444) - dead code
- 80-80: pt_integrator.metal reverted to round-4 text
- 82-84: entered-distance stack rewrite; bug caught: prune path continued with stale node_index/node_tested=false -> fixed with inner pop loop setting node_tested=true
- 86-88: entered-stack bit-identical 3 scenes; A/B base 44.493/46.042 vs cand 44.425/45.607, 6/8 pairs -> KEPT
- 90-96: stack_payload added (push child payload; pop does no global load); bit-identical 3 scenes
- 98-98: _e/ = entered-only variant built as isolation base
- 110-110: stack_payload WIN: base(_e) 43.875/45.583 vs cand 43.226/44.906, 5/8 pairs, mean -0.68ms -> KEPT
- 112-118: read code: PtBvhNodeQ 16B quantized node, pre-order left=index+1, payload=right child, leaf bit31|start<<8|(count-1); quantize_bvh in src/pt_bvh.cpp; node buffer upload in pt_metal_host.mm; both walks in pt_trace.metal; _b/ snapshot = kept state (entered-stack + stack_payload), A/B base and revert point

### art-001766: 7 of 7 lines shown

- 122-126: host plumbing read - node_buffer creation, upload_mesh, render_frame threadgroup 32x4 + PT_TG env, quantize_bvh call site in pt_main.cpp
- 128-134: pt_gpu_types.hpp now has PtBvhChild (16B: lo0/hi0/lo1/hi1/lo2/hi2 + payload); PtBvhNodeQ deleted by python line-splice (edit_file mismatched on whitespace)
- 136-142: quantize_bvh rewritten to emit 32B child-pair blocks (two PtBvhChild per internal node, pre-order block index, payload = leaf range or child block index); kInvalidNode added next to kInfinity in pt_bvh.cpp
- 144-146: API switched everywhere (pt_bvh.hpp signature, pt_main.cpp vector type, pt_metal_host.mm sizeof); both GPU walks rewritten - nodes indexed payload*2 / payload*2+1, stack = (payload, entered), ROOT BOX TEST DROPPED (monotone outward quantization => quantized root contains every child box)
- 148-151: update_memory wrote decisions/round5-stack-payload and decisions/round5-childpair (in-flight child-pair description, A/B base = _b/pathtracer)
- 153-157: build failed - pt_metal_host.hpp GpuMeshData::nodes still PtBvhNodeQ*; fixed to PtBvhChild*; build clean after
- 159-161: scene configs live at TOP level (scene-default.json, scene-alt-view.json, scene-caustic-closeup.json), not in scenes/; wrong path gives 'cannot open scene config'

## d3 art-003135

### art-003062: 11 of 11 lines shown

- 1-1: round-8 task: 4 ranked experiments, bit-identity gate, paired A/B n>=15, snapshot pt_round_08, stop rule; r7 best 30.984/30.706ms 3.539x
- 3-3: dir listing: pt_round_01..07, _r7b frozen r7 A/B base, scene-*.json, many _ab scratch dirs
- 5-5: run tool needs {command}
- 7-7: benchmark.json: r7 vs r6 10/10 wins mean -0.2925ms, 99.1% GPU, wall-gpu 0.278ms, speedup 3.539
- 7-7: coordinator ledger: rounds 4-7 results, sub-5% counter=1, round-8 plan = the 4 experiments
- 9-9: writer log r7: kept f32 node -0.44%, shadow payload -0.49%, 12B sample buffer -0.19%; rejected stk16 +27%, spt2 +1.08%, leaf2 +0.9%; BVH TOPOLOGY FROZEN (MAX_DEPTH 16->20 changes alt-view PFM)
- 9-9: file sizes: pt_trace.metal 552, pt_integrator.metal 367, pt_metal_host.mm 309, pt_bvh.cpp 435
- 11-11: pt_integrator.metal text: pt_morton_decode 5 bits/axis tile 32, pt_tiled_pixel, trace_kernel, trace_samples_kernel sample-major store, combine_kernel ordered sum
- 13-13: pt_gpu_types.hpp: PtBvhNodeQ = lo[4]+hi[4] = 32 BYTES (stale 16B comment), PtTri 48B, PtTriNormals 36B, PtSample3 12B, STACK 20, MAX_DEPTH 16
- 13-13: pt_metal_host.mm: env knobs PT_TG (default 32x4), PT_CLASSIC, PT_DEBUG; grid rounds to 32; sample buffer cap 1.5GB; fastMath YES
- 15-15: pt_trace.metal 1-200: PCG rng, pt_seed_pixel, PtQRay origin_times_inv fma slab test, pt_load_node = two float4 loads

### art-003133: 15 of 15 lines shown

- 19-19: _r8b frozen round-7 A/B tree built OK (only fastMathEnabled deprecation warning)
- 24-25: _r8p profiler patch — PT_SPLIT commits the trace CB separately and prints trace_gpu_ms
- 26-26: split measured trace_gpu_ms=30.30 vs combine gpu_ms_best=0.42 (trace = 98.6% of GPU)
- 27-27: full pt_trace.metal text — pt_intersect_mesh child handling, pt_shadow_hit, pt_intersect_scene, pt_occluded
- 30-30: writer-log grep results; line 163 = sibling 32-byte node pairs already measured a wash (n=41, +0.104ms); line 167 simdgroup-cooperative traversal dropped; line 171 suggests node compression
- 33-33: E5 idea — nodes are actually 32 bytes (PtBvhNodeQ lo[4]+hi[4]), stale 16-byte comment
- 35-39: cost split — PT_DEFINE/PT_DIAG_DEPTH hooks in _r8p; d1=5.82 d2=10.96 d3=17.10 d12=30.28ms; PT_DIAG_NOSHADOW=26.68 (shadow walks 3.65ms)
- 40-41: pt_bvh.cpp quantize_bvh — integers clamped <=65535, power-of-two scale, origin snapped; safe for u16
- 43-46: E5 implemented: PtBvhNodeQ = 6 u16 + u32 payload aligned 16; pt_bvh.cpp writes shorts; shader pt_load_node reads one uint4 and unpacks with float(x&0xFFFF)/float(x>>16)
- 49-50: E5 build clean and PFM byte-identical on scene-default, alt-view, caustic-closeup at 64spp seed 1
- 51-52: created _r8_ab.sh (cand bin, base bin, pairs, scene, spp) and _r8_pair.pl (wins, mean delta, geo-mean ratio, sd_log, se, t)
- 53-54: E5 REJECTED 0/16 wins +0.90% t=16.25 → walk is ALU/latency bound; preserved in _r8_node16/; pathtracer/ reverted from _r8b, PFM identical
- 55-56: maxTotalThreadsPerThreadgroup trace=1024 sample_parallel=1 (no occupancy lever); _r8_abenv.sh written
- 57-60: _r8e scratch tree + PT_DEFINE host hook; PT_MORTON_T decode swap; trace_lanes_kernel/combine_lanes_kernel appended (lanes = 32 samples of one pixel)
- 63-69: working-memory entries written: r8-baseline-and-harness, r8-cost-split, r8-rejected-e5-e3-e4
