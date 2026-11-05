#!/usr/bin/env python3
"""A scripted OpenAI-compatible endpoint for the ipc-live preflight: no real model.

It tells the agents apart by their system prompts and plays each one's script, so the real
aos run, sandbox, compiler and GPU do real work. It writes a tiny Metal program that is not a
rasterizer: this checks the machinery, not the task. Its benchmarker tries the raw record inline
and is refused, and its verifier asks for the record first and is refused, as a model may.
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


def tool(name, **args):
    return {"type": "tool", "tool_call": {"name": name, "args": args}}


def run(command):
    return tool("run", command=command)


def done(reason, code=0):
    return {"type": "exit", "exit_status": {"code": code, "reason": reason, "retryable": False}}


def delegate(agent, goal):
    return {"type": "delegate", "delegate": {"agent_id": agent, "goal": goal}}


class Run:
    root = ""


def build(d):
    return f"clang++ -O2 -std=c++17 -fobjc-arc -I {d}/include {d}/src/*.cpp {d}/src/*.mm -framework Metal -framework Foundation -o {d}/rasterizer"


def coordinator(me, workspace, verification):
    d = f"{workspace}/metal"
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
    d = f"{workspace}/metal"
    yield tool("write_file", path=f"{d}/include/host.h", content=HEADER)
    yield tool("write_file", path=f"{d}/src/main.cpp", content=MAIN)
    yield tool("write_file", path=f"{d}/src/host.mm", content=HOST)
    yield tool("write_file", path=f"{d}/shaders/double.metal", content=SHADER)
    yield run(build(d))
    yield run(f"{d}/rasterizer")
    yield tool("message_agent", to=coordinator_pid, body="A question: does the first round want tile binning, or a plain loop?")
    yield done("writer finished")


def benchmarker(me, workspace, coordinator_pid):
    d = f"{workspace}/metal"
    yield run(f"which perl clang++ python3; {build(d)} && echo built")
    yield run(
        f"for i in $(seq 1 20); do echo \"trial $i: {d}/rasterizer --benchmark 10 --threads 8\"; "
        f"/usr/bin/time -l perl -e 'alarm 30; exec @ARGV' {d}/rasterizer 2>&1; echo \"exit $?\"; done > $TMPDIR/raw.txt; wc -c $TMPDIR/raw.txt"
    )
    text = yield run("cat $TMPDIR/raw.txt")
    raw = text.split("stdout:", 1)[1] if "stdout:" in text else text
    # Without the runtime's own lines that came after the result.
    raw = re.split(r"\n(?:\[#\d+\] )?(?:budget |runtime notice:)", raw)[0]
    text = yield tool("message_agent", to=coordinator_pid, body=raw)   # refused: too large
    text = yield tool("publish_artifact", type="raw_timings", body=raw)
    handle = re.findall(r"art-\d+", text)[-1]
    yield tool("message_agent", to=coordinator_pid, body="The raw per-trial record.", artifacts=handle)
    yield tool("write_file", path=f"{workspace}/metal_benchmark.json", content='{"preflight": true}\n')
    yield done("benchmarker finished")


def verifier(me, workspace, verification):
    d = f"{workspace}/metal"
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
    yield run(
        f"{base}/baseline/rasterizer --mesh {base}/assets/stanford-bunny.obj --scene-config {base}/scene-small.json --out {verification}/cpu.ppm"
        f" && head -c 20 {verification}/cpu.ppm | head -2 && python3 -I -c 'print(open(\"{verification}/cpu.ppm\",\"rb\").read(2))'"
    )
    yield run(f"perl -e 'alarm 15; exec @ARGV' {d}/rasterizer; echo exit=$?; touch {d}/should-be-refused; echo touch=$?")
    yield tool("write_file", path=f"{verification}/metal_verification.txt", content="preflight: a scripted verdict, overall PASS\n")
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
            m = re.search(r"Maintain a coordinator ledger at (\S+)/metal_coordinator_log.txt", system)
            workspace = m.group(1)
            script = coordinator(me, workspace, workspace.replace("metal-workspace", "metal-verification"))
            action = next(script)
        elif script is None or (role != "verifier" and role not in self.started_for or self.started_for.get(role) != me and role != "verifier"):
            goal = re.search(r'"goal": "(.*?)"', everything).group(1)
            workspace = re.search(r"(\S+/metal-workspace)", goal).group(1)
            pid = re.search(r"process_id is (pid-\d+)", goal)
            if role == "writer":
                script = writer(me, workspace, pid.group(1))
            elif role == "benchmarker":
                script = benchmarker(me, workspace, pid.group(1))
            else:
                script = verifier(me, workspace, workspace.replace("metal-workspace", "metal-verification"))
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
HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
