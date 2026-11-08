#!/usr/bin/env python3
"""A scripted OpenAI-compatible endpoint for the ipc-live preflight: no real model.

It tells the agents apart by their system prompts and plays each one's script, so the real
aos run, sandbox, compiler and GPU do real work. It writes a tiny program that is not a
rasterizer: this checks the machinery, not the task. Its benchmarker tries the raw record inline
and is refused, and its verifier asks for the record first and is refused, as a model may.

On CUDA it takes the handles workflow's path: the benchmarker works in its private directory and
publishes by path, the coordinator and verifier try that directory and are refused, the verifier
waits, is mailed the handles, resumes, and reads a record of more than one page to its end.
"""
import json, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

HEADER = """#pragma once
#include <string>
// Runs a small compute kernel; returns the device name, or the error.
std::string run_kernel(const std::string& shader_dir, double* gpu_ms, bool* ok);
"""
MAIN = """#include "host.h"
#include <cstdio>
#include <string>
int main(int argc, char** argv) {
    std::string dir = argv[0];
    dir = dir.substr(0, dir.rfind('/')) + "/shaders";
    double gpu_ms = 0;
    bool ok = false;
    std::string name = run_kernel(dir, &gpu_ms, &ok);
    std::printf("device=%s kernel_ok=%d\\n", name.c_str(), ok ? 1 : 0);
    std::printf("render_ms_best=%.3f render_ms_avg=%.3f gpu_ms_best=%.3f threads_used=1\\n", gpu_ms, gpu_ms, gpu_ms);
    return ok ? 0 : 1;
}
"""
HOST = """#include "host.h"
#import <Foundation/Foundation.h>
#import <Metal/Metal.h>
std::string run_kernel(const std::string& shader_dir, double* gpu_ms, bool* ok) {
    id<MTLDevice> device = MTLCreateSystemDefaultDevice();
    if (device == nil) {
        return "no Metal device";
    }
    NSString* path = [NSString stringWithFormat:@"%s/double.metal", shader_dir.c_str()];
    NSError* error = nil;
    NSString* source = [NSString stringWithContentsOfFile:path encoding:NSUTF8StringEncoding error:&error];
    if (source == nil) {
        return std::string("cannot read shader: ") + error.localizedDescription.UTF8String;
    }
    id<MTLLibrary> library = [device newLibraryWithSource:source options:nil error:&error];
    if (library == nil) {
        return std::string("cannot compile shader: ") + error.localizedDescription.UTF8String;
    }
    id<MTLComputePipelineState> pipeline =
        [device newComputePipelineStateWithFunction:[library newFunctionWithName:@"double_all"] error:&error];
    if (pipeline == nil) {
        return std::string("cannot build pipeline: ") + error.localizedDescription.UTF8String;
    }
    const NSUInteger count = 1024;
    id<MTLBuffer> buffer = [device newBufferWithLength:count * sizeof(uint32_t) options:MTLResourceStorageModeShared];
    uint32_t* values = (uint32_t*)buffer.contents;
    for (NSUInteger i = 0; i < count; i++) {
        values[i] = (uint32_t)i;
    }
    id<MTLCommandBuffer> commands = [[device newCommandQueue] commandBuffer];
    id<MTLComputeCommandEncoder> encoder = [commands computeCommandEncoder];
    [encoder setComputePipelineState:pipeline];
    [encoder setBuffer:buffer offset:0 atIndex:0];
    [encoder dispatchThreads:MTLSizeMake(count, 1, 1) threadsPerThreadgroup:MTLSizeMake(32, 1, 1)];
    [encoder endEncoding];
    [commands commit];
    [commands waitUntilCompleted];
    if (commands.error != nil) {
        return std::string("GPU error: ") + commands.error.localizedDescription.UTF8String;
    }
    *gpu_ms = (commands.GPUEndTime - commands.GPUStartTime) * 1000.0;
    *ok = true;
    for (NSUInteger i = 0; i < count; i++) {
        if (values[i] != 2 * i) {
            *ok = false;
        }
    }
    return device.name.UTF8String;
}
"""
SHADER = """#include <metal_stdlib>
using namespace metal;
kernel void double_all(device uint* values [[buffer(0)]], uint index [[thread_position_in_grid]]) {
    values[index] = 2 * index;
}
"""


CUDA_MAIN = """#include "host.h"
#include <cstdio>
int main() {
    double gpu_ms = 0;
    bool ok = run_kernel(&gpu_ms);
    std::printf("render_ms_best=%.3f render_ms_avg=%.3f gpu_ms_best=%.3f threads_used=1\\n", gpu_ms, gpu_ms, gpu_ms);
    return ok ? 0 : 1;
}
"""
CUDA_HEADER = """#pragma once
// Runs a small kernel on the card, and says which card; false if it could not.
bool run_kernel(double* gpu_ms);
"""
CUDA_KERNEL = """#include "host.h"
#include <cuda_runtime.h>
#include <chrono>
#include <cstdio>
__global__ void doubled(unsigned* values) { values[threadIdx.x] = 2 * threadIdx.x; }
bool run_kernel(double* gpu_ms) {
    cudaDeviceProp prop;
    cudaError_t error = cudaGetDeviceProperties(&prop, 0);
    if (error != cudaSuccess) {
        std::printf("device=none error=%s\\n", cudaGetErrorString(error));
        return false;
    }
    unsigned* device = nullptr;
    unsigned host[256] = {0};
    auto start = std::chrono::steady_clock::now();
    error = cudaMalloc(&device, sizeof(host));
    if (error == cudaSuccess) {
        doubled<<<1, 256>>>(device);
        error = cudaMemcpy(host, device, sizeof(host), cudaMemcpyDeviceToHost);
        cudaFree(device);
    }
    *gpu_ms = std::chrono::duration<double, std::milli>(std::chrono::steady_clock::now() - start).count();
    bool ok = error == cudaSuccess;
    for (unsigned i = 0; ok && i < 256; i++) {
        ok = host[i] == 2 * i;
    }
    std::printf("device=%s kernel_ok=%d %s\\n", prop.name, ok ? 1 : 0, cudaGetErrorString(error));
    return ok;
}
"""


def tool(name, **args):
    return {"type": "tool", "tool_call": {"name": name, "args": args}}


def run(command, **args):
    return tool("run", command=command, **args)


def done(reason, code=0):
    return {"type": "exit", "exit_status": {"code": code, "reason": reason, "retryable": False}}


def delegate(agent, goal, **more):
    return {"type": "delegate", "delegate": {"agent_id": agent, "goal": goal, **more}}


class Run:
    root = ""
    kind = "metal"
    dry = False
    goal_record = ""
    evidence = False
    unread = False
    inner = False
    attached = []
    workspace = "metal-workspace"
    verification = "metal-verification"


def execute_at(binary):
    """Running the program, or in a dry run, what needs no GPU memory: the card as the sandbox sees it."""
    if Run.dry:
        return """python3 -c "import ctypes; print('cuInit', ctypes.CDLL('libcuda.so.1').cuInit(0))"; nvidia-smi -L; echo render_ms_best=0 render_ms_avg=0 gpu_ms_best=0 threads_used=1"""
    return binary


def execute(d):
    return execute_at(f"{d}/rasterizer")


def build(d):
    if Run.kind == "cuda":
        return f"nvcc -O2 -std=c++17 -arch=sm_86 -I {d}/include {d}/src/*.cpp {d}/src/*.cu -o {d}/rasterizer"
    return f"clang++ -O2 -std=c++17 -fobjc-arc -I {d}/include {d}/src/*.cpp {d}/src/*.mm -framework Metal -framework Foundation -o {d}/rasterizer"


def bench_of(workspace):
    return workspace.replace(Run.workspace, f"{Run.kind}-bench")


def timed(binary, trials, out):
    """Each trial as the benchmarker's prompt asks: the command, its whole output and time -v, and the card before and after."""
    card = "nvidia-smi --query-gpu=name,uuid,memory.used,memory.total,utilization.gpu,temperature.gpu --format=csv"
    run_it = execute_at(binary).replace(chr(34), chr(92) + chr(34))
    return (
        f"for i in $(seq 1 {trials}); do echo \"trial $i: {binary} --benchmark 10 --threads 8\"; {card}; "
        f"/usr/bin/time -v perl -e 'alarm 30; exec @ARGV' sh -c \"{run_it}\" 2>&1; echo \"exit $?\"; {card}; done > {out}; wc -c {out}"
    )


def coordinator_handles(me, workspace, verification):
    d = f"{workspace}/{Run.kind}"
    yield delegate("writer", f"Write the program in {d}. Your coordinator's process_id is {me}.")
    yield delegate("benchmarker", f"Build a copy of {d} and benchmark it. Your coordinator's process_id is {me}.")
    yield tool("read_file", path=f"{bench_of(workspace)}/raw_record.txt")  # refused: only the benchmarker's
    text = yield tool("list_artifacts")
    published = dict(re.findall(r"(art-\d+)\s+published\.(\S+)", text))
    record = next(a for a, kind in published.items() if kind == "raw_timings")
    yield tool("read_artifact", id=next(a for a, kind in published.items() if kind == "benchmark_json"))
    text = yield delegate("verifier", f"Verify {d} into {verification}. The record is {record}.")
    verifier = re.search(r"pid=(pid-\d+)", text).group(1)
    yield tool("message_agent", to=verifier, body="The raw per-trial record and the JSON.", artifacts=",".join(published))
    yield delegate("verifier", "It is in your mail.")
    yield tool("write_file", path=f"{workspace}/final-summary.txt", content="preflight: a scripted run\n")
    yield done("preflight finished")


def coordinator_evidence(me, workspace, verification):
    """The evidence workflow's path: the handle goes with the delegation, and no mail or wait is needed. It puts the handles
    at the action level, as live4's coordinator wrote them, unless Run.inner. With Run.unread it plays live3's pid-000009
    instead, whose delegation carried no handle. It logs what the delegate_result said was attached."""
    d = f"{workspace}/{Run.kind}"
    yield delegate("writer", f"Write the program in {d}. Your coordinator's process_id is {me}.")
    yield delegate("benchmarker", f"Build a copy of {d} and benchmark it. Your coordinator's process_id is {me}.")
    yield tool("read_file", path=f"{bench_of(workspace)}/raw_record.txt")  # refused: only the benchmarker's
    text = yield tool("list_artifacts")
    published = dict(re.findall(r"(art-\d+)\s+published\.(\S+)", text))
    record = next(a for a, kind in published.items() if kind == "raw_record")
    yield tool("read_artifact", id=next(a for a, kind in published.items() if kind == "benchmark_json"))
    goal = f"Verify {d} into {verification}."
    if Run.unread:
        text = yield delegate("verifier", f"{goal} The record is {record}.")
    elif Run.inner:
        text = yield delegate("verifier", goal, artifacts=",".join(published))
    else:
        text = yield {**delegate("verifier", goal), "artifacts": ",".join(published)}
    told = re.findall(r"delegate_result agent=verifier pid=\S+ attached=(\S+)", text or "")
    print(f"coordinator told: attached={told[-1] if told else '(nothing)'}", file=sys.stderr, flush=True)
    yield tool("write_file", path=f"{workspace}/final-summary.txt", content="preflight: a scripted run\n")
    yield done("preflight finished")


def benchmarker_handles(me, workspace, coordinator_pid):
    d = f"{workspace}/{Run.kind}"
    bench = bench_of(workspace)
    copy = f"{bench}/build"
    record = f"{bench}/raw_record.txt"
    yield run(f"which perl python3 nvcc; pwd; cp -R {d} {copy} && {build(copy)} && echo built")
    yield run(timed(f"{copy}/rasterizer", 50, record), timeout="400s")  # 100 nvidia-smi calls among them
    yield tool("write_file", path=f"{bench}/cuda_benchmark.json", content='{"preflight": true}\n')
    text = yield run(f"head -c 20000 {record}")
    raw = text.split("stdout:", 1)[1] if "stdout:" in text else text
    yield tool("message_agent", to=coordinator_pid, body=raw)  # refused: too large
    yield tool("publish_artifact", type="raw_record" if Run.evidence else "raw_timings", path=record)
    yield tool("publish_artifact", type="benchmark_json", path=f"{bench}/cuda_benchmark.json")
    yield done("benchmarker finished: published the record and the JSON")


def verifier_handles(me, workspace, verification):
    d = f"{workspace}/{Run.kind}"
    bench = bench_of(workspace)
    base = f"{Run.root}/reports/rasterizer-bunny"
    record = f"{bench}/raw_record.txt"
    yield tool("read_file", path=record)  # refused: only the benchmarker's
    yield run(f"cat {record}; ls {bench}; cat {workspace}/cuda_coordinator_log.txt; echo tried")  # refused, each
    yield tool("list_artifacts")
    text = yield tool("read_artifact", id=Run.goal_record)  # refused: no handle yet
    text = yield {"type": "wait", "message": "I need the benchmarker's raw per-trial record and the JSON."}
    handles = list(dict.fromkeys(re.findall(r"art-\d+", text)))
    yield from verifier_work(d, verification, base, handles)


def verifier_evidence(me, workspace, verification):
    """A verifier told what came attached. Without a handle it plays live3's pid-000009: it is refused, tries to exit 0
    with what it has, and is refused until its steps run out."""
    d = f"{workspace}/{Run.kind}"
    base = f"{Run.root}/reports/rasterizer-bunny"
    bench = bench_of(workspace)
    record = f"{bench}/raw_record.txt"
    yield tool("read_file", path=record)  # refused: only the benchmarker's
    yield run(f"cat {record}; ls {bench}; cat {workspace}/cuda_coordinator_log.txt; echo tried")  # refused, each
    yield tool("list_artifacts")
    if Run.attached:
        yield from verifier_work(d, verification, base, Run.attached)
        return
    yield tool("read_artifact", id=Run.goal_record)  # refused: no handle
    yield tool("write_file", path=f"{verification}/{Run.kind}_verification.txt", content="preflight: a scripted verdict, overall PASS, on a record not read.\n")
    while True:
        yield done("verifier finished: PASS, without the record")


def verifier_work(d, verification, base, handles):
    for handle in handles:
        offset = "0"
        while True:
            text = yield tool("read_artifact", id=handle, offset=offset)
            found = re.findall(r"next_offset:(\d+)", text)
            if not found:
                break
            offset = found[-1]
    copy = f"{verification}/build"
    yield run(f"cp -R {d} {copy} && {build(copy)} && echo built")
    binary = f"{verification}/baseline"
    yield run(f"g++ -O2 -std=c++17 -pthread -I {base}/baseline/include {base}/baseline/src/*.cpp -o {binary} && echo built")
    yield run(
        f"{binary} --mesh {base}/assets/stanford-bunny.obj --scene-config {base}/scene-small.json --out {verification}/cpu.ppm"
        f" && head -c 20 {verification}/cpu.ppm | head -2 && python3 -I -c 'print(open(\"{verification}/cpu.ppm\",\"rb\").read(2))'"
    )
    yield run(timed(f"{copy}/rasterizer", 5, f"{verification}/own_trials.txt") + f"; touch {d}/should-be-refused; echo touch=$?", timeout="120s")
    yield tool("write_file", path=f"{verification}/{Run.kind}_verification.txt", content=f"preflight: a scripted verdict, overall PASS. Read: {', '.join(handles)}.\n")
    yield done("verifier finished: a scripted verdict")


def coordinator(me, workspace, verification):
    d = f"{workspace}/{Run.kind}"
    yield delegate("writer", f"Write the Metal program in {d}. Your coordinator's process_id is {me}.")
    yield delegate("benchmarker", f"Build {d} and benchmark it. Your coordinator's process_id is {me}.")
    text = yield tool("list_artifacts", type="published.raw_timings")
    handle = re.findall(r"art-\d+", text)[-1]
    text = yield delegate("verifier", f"Verify {d}, against the benchmark in {workspace}, into {verification}.")
    verifier = re.search(r"pid=(pid-\d+)", text).group(1)
    yield tool("message_agent", to=verifier, body="The raw per-trial record.", artifacts=handle)
    yield delegate("verifier", "It is in your mail.")
    yield tool("write_file", path=f"{workspace}/final-summary.txt", content="preflight: a scripted run\n")
    yield done("preflight finished")


def writer(me, workspace, coordinator_pid):
    d = f"{workspace}/{Run.kind}"
    if Run.kind == "cuda":
        yield tool("write_file", path=f"{d}/include/host.h", content=CUDA_HEADER)
        yield tool("write_file", path=f"{d}/src/main.cpp", content=CUDA_MAIN)
        yield tool("write_file", path=f"{d}/src/kernel.cu", content=CUDA_KERNEL)
    else:
        yield tool("write_file", path=f"{d}/include/host.h", content=HEADER)
        yield tool("write_file", path=f"{d}/src/main.cpp", content=MAIN)
        yield tool("write_file", path=f"{d}/src/host.mm", content=HOST)
        yield tool("write_file", path=f"{d}/shaders/double.metal", content=SHADER)
    yield run(build(d))
    yield run(execute(d))
    yield tool("message_agent", to=coordinator_pid, body="A question: does the first round want tile binning, or a plain loop?")
    yield done("writer finished")


def benchmarker(me, workspace, coordinator_pid):
    d = f"{workspace}/{Run.kind}"
    yield run(f"which perl clang++ python3; {build(d)} && echo built")
    yield run(
        f"for i in $(seq 1 20); do echo \"trial $i: {d}/rasterizer --benchmark 10 --threads 8\"; "
        f"/usr/bin/time {'-v' if Run.kind == 'cuda' else '-l'} perl -e 'alarm 30; exec @ARGV' sh -c \"{execute(d).replace(chr(34), chr(92) + chr(34))}\" 2>&1; echo \"exit $?\"; done > $TMPDIR/raw.txt; wc -c $TMPDIR/raw.txt"
    )
    text = yield run("cat $TMPDIR/raw.txt")
    raw = text.split("stdout:", 1)[1] if "stdout:" in text else text
    # Without the runtime's own lines that came after the result.
    raw = re.split(r"\n(?:\[#\d+\] )?(?:budget |runtime notice:)", raw)[0]
    text = yield tool("message_agent", to=coordinator_pid, body=raw)   # refused: too large
    text = yield tool("publish_artifact", type="raw_timings", body=raw)
    handle = re.findall(r"art-\d+", text)[-1]
    yield tool("message_agent", to=coordinator_pid, body="The raw per-trial record.", artifacts=handle)
    yield tool("write_file", path=f"{workspace}/{Run.kind}_benchmark.json", content='{"preflight": true}\n')
    yield done("benchmarker finished")


def verifier(me, workspace, verification):
    d = f"{workspace}/{Run.kind}"
    base = f"{Run.root}/reports/rasterizer-bunny"
    text = yield tool("list_artifacts", type="published.raw_timings")
    yield tool("read_artifact", id="art-000001")                       # refused: no handle
    text = yield {"type": "wait", "message": "I need the benchmarker's raw per-trial record."}
    handle = re.findall(r"attached (art-\d+)", text) or re.findall(r"art-\d+", text)
    offset = "0"
    while True:
        text = yield tool("read_artifact", id=handle[-1], offset=offset)
        match = re.search(r"next_offset:(\d+)", text[-200:])
        if not match:
            break
        offset = match.group(1)
    binary = f"{base}/baseline/rasterizer"
    if Run.kind == "cuda":
        binary = f"{verification}/baseline"
        yield run(f"g++ -O2 -std=c++17 -pthread -I {base}/baseline/include {base}/baseline/src/*.cpp -o {binary} && echo built")
    yield run(
        f"{binary} --mesh {base}/assets/stanford-bunny.obj --scene-config {base}/scene-small.json --out {verification}/cpu.ppm"
        f" && head -c 20 {verification}/cpu.ppm | head -2 && python3 -I -c 'print(open(\"{verification}/cpu.ppm\",\"rb\").read(2))'"
    )
    yield run(f"perl -e 'alarm 15; exec @ARGV' sh -c \"{execute(d).replace(chr(34), chr(92) + chr(34))}\"; echo exit=$?; touch {d}/should-be-refused; echo touch=$?")
    yield tool("write_file", path=f"{verification}/{Run.kind}_verification.txt", content="preflight: a scripted verdict, overall PASS\n")
    yield done("verifier finished: a scripted verdict")


class Handler(BaseHTTPRequestHandler):
    scripts = {}

    def log_message(self, *args):
        pass

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        messages = request["messages"]
        system = messages[0]["content"]
        role = re.search(r"Agent instructions:\nYou are the (\w+)", system).group(1)
        everything = "\n".join(str(m["content"]) for m in messages)
        me = re.search(r'"process_id": "(pid-\d+)"', everything).group(1)
        script = self.scripts.get(role)
        # Everything since this agent's last turn: a result, and any mail and notes after it.
        since = len(messages)
        while since > 0 and messages[since - 1]["role"] != "assistant":
            since -= 1
        last = "\n".join(str(m["content"]) for m in messages[since:])
        if role == "coordinator" and script is None:
            m = re.search(r"Maintain a coordinator ledger at (\S+)/\w+_coordinator_log.txt", system)
            workspace = m.group(1)
            Run.evidence = "rasterizer-bunny-cuda-evidence-ipc" in everything
            play = coordinator_evidence if Run.evidence else coordinator_handles if Run.kind == "cuda" else coordinator
            script = play(me, workspace, workspace.replace(Run.workspace, Run.verification))
            action = next(script)
        elif script is None or (role != "verifier" and role not in self.started_for or self.started_for.get(role) != me and role != "verifier"):
            goal = re.search(r'"goal": "(.*?)"', everything).group(1)
            workspace = re.search(r"(\S+/" + Run.workspace + ")", goal).group(1)
            pid = re.search(r"process_id is (pid-\d+)", goal)
            verification = workspace.replace(Run.workspace, Run.verification)
            if role == "writer":
                script = writer(me, workspace, pid.group(1))
            elif role == "benchmarker":
                script = (benchmarker_handles if Run.kind == "cuda" else benchmarker)(me, workspace, pid.group(1))
            elif Run.evidence:
                found = re.search(r"Attached to this delegation[^\n]*", everything)
                Run.attached = list(dict.fromkeys(re.findall(r"art-\d+", found.group(0)))) if found else []
                record = re.search(r"art-\d+", goal)
                Run.goal_record = record.group(0) if record else ""
                script = verifier_evidence(me, workspace, verification)
            elif Run.kind == "cuda":
                Run.goal_record = re.search(r"art-\d+", goal).group(0)
                script = verifier_handles(me, workspace, verification)
            else:
                script = verifier(me, workspace, verification)
            action = next(script)
        else:
            # The newest turns, joined, so a mail and its notes are all seen.
            action = script.send(last)
        self.scripts[role] = script
        self.started_for[role] = me
        body = json.dumps({
            "choices": [{"message": {"content": json.dumps({"action": action})}}],
            "usage": {"prompt_tokens": 10, "completion_tokens": 1},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


Handler.started_for = {}
Run.root = sys.argv[2]
if len(sys.argv) > 3:
    Run.kind = sys.argv[3]
    Run.workspace, Run.verification = f"{Run.kind}-workspace", f"{Run.kind}-verification"
Run.dry = len(sys.argv) > 4 and sys.argv[4] == "dry"
HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
