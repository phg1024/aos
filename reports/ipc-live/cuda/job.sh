#!/bin/bash
# One job for one window, on the machine with the card. Usage: job.sh <run name> <preflight|dry|live> [seconds].
# dry is preflight with no kernel, for use outside a window: the sandbox, the compiler and the mail, and
# no GPU memory.
# Ray runs this as the entrypoint. It prints which card it sees, runs a kernel check that fails
# fast, runs aos under a time limit, and leaves nothing of its own on the card when it ends.
# Everything goes to $AOS_IPC_ROOT/reports/ipc-live/runs/<name> on local disk as it happens.
set -u
name=${1:?run name}; mode=${2:?preflight, dry or live}; limit=${3:-5400}
: "${AOS_IPC_ROOT:=$HOME/aos-ipc-live}"; export AOS_IPC_ROOT
: "${AOS_CUDA_HOME:=$HOME/aos-cuda/env}"; export AOS_CUDA_HOME
: "${AOS_GPU_NAME:?AOS_GPU_NAME names the card, as nvidia-smi does}"
: "${AOS_LLM_KEY_FILE:=$AOS_IPC_ROOT/llm-key}"
here=$AOS_IPC_ROOT/bin
run_dir=$AOS_IPC_ROOT/reports/ipc-live/runs/$name

export PATH=$AOS_CUDA_HOME/bin:/usr/bin:/bin
# Match what the job declared to Ray, so a library cannot take the whole machine.
export OMP_NUM_THREADS=${AOS_JOB_CPUS:-12} MKL_NUM_THREADS=${AOS_JOB_CPUS:-12}
# Ray may set this empty for zero-GPU work. The sandbox sets it again for each run.
uuids=$(nvidia-smi --query-gpu=uuid,name --format=csv,noheader | grep -F "$AOS_GPU_NAME" | cut -d, -f1)
if [[ $(printf '%s\n' "$uuids" | grep -c GPU-) != 1 ]]; then
  echo "job: expected one card named $AOS_GPU_NAME, found: $uuids" >&2
  exit 3
fi
export AOS_GPU_UUID=$uuids CUDA_VISIBLE_DEVICES=$uuids
echo "job: card $AOS_GPU_NAME $AOS_GPU_UUID, $(nvidia-smi --id="$AOS_GPU_UUID" --query-gpu=memory.free --format=csv,noheader) free"

case $mode in
  preflight|dry)
    export AOS_LLM_BASE_URL=http://127.0.0.1:${AOS_FAKE_PORT:-18950}/v1 AOS_LLM_MODEL=scripted AOS_LLM_CONTEXT_WINDOW=100000 ;;
  live)
    : "${AOS_LLM_BASE_URL:?}" "${AOS_LLM_MODEL:?}" "${AOS_LLM_CONTEXT_WINDOW:?}"
    if [[ ! -r $AOS_LLM_KEY_FILE ]] || [[ $(stat -c %a "$AOS_LLM_KEY_FILE") != 600 ]]; then
      echo "job: $AOS_LLM_KEY_FILE must exist with mode 600" >&2
      exit 4
    fi
    AOS_LLM_API_KEY=$(cat "$AOS_LLM_KEY_FILE"); export AOS_LLM_API_KEY ;;
  *) echo "job: mode must be preflight, dry or live" >&2; exit 2 ;;
esac

workflow=$(python3 -I "$here/render.py" "$name" cuda) || exit 5
echo "job: $workflow"

# The kernel check, before any model call: compile and run one small kernel. This outside
# the sandbox says the card and driver work; aos's own runs say the sandbox does.
check=$run_dir/kernel-check
mkdir -p "$check"
cat > "$check/check.cu" <<'CU'
#include <cuda_runtime.h>
#include <cstdio>
__global__ void doubled(unsigned* values) { values[threadIdx.x] = 2 * threadIdx.x; }
int main() {
    cudaDeviceProp prop;
    cudaError_t error = cudaGetDeviceProperties(&prop, 0);
    if (error != cudaSuccess) { std::printf("error=%s\n", cudaGetErrorString(error)); return 1; }
    std::printf("device=%s\n", prop.name);
    unsigned* device = nullptr;
    unsigned host[256] = {0};
    if ((error = cudaMalloc(&device, sizeof(host))) != cudaSuccess) { std::printf("error=%s\n", cudaGetErrorString(error)); return 1; }
    doubled<<<1, 256>>>(device);
    if ((error = cudaMemcpy(host, device, sizeof(host), cudaMemcpyDeviceToHost)) != cudaSuccess) { std::printf("error=%s\n", cudaGetErrorString(error)); return 1; }
    cudaFree(device);
    for (unsigned i = 0; i < 256; i++) { if (host[i] != 2 * i) { std::printf("kernel_ok=0\n"); return 1; } }
    std::printf("kernel_ok=1\n");
    return 0;
}
CU
if ! nvcc -O2 -std=c++17 "$check/check.cu" -o "$check/check"; then
  echo "job: the kernel check did not compile: nothing else ran" >&2
  exit 6
fi
if [[ $mode != dry ]] && ! timeout 60 "$check/check"; then
  echo "job: the kernel check failed: nothing else ran" >&2
  exit 6
fi

fake=
cleanup() {
  [[ -n $fake ]] && kill "$fake" 2>/dev/null
  # Anything still on the card from this run's directory: the watchdog cannot restore past it.
  for pid in $(nvidia-smi --query-compute-apps=pid --format=csv,noheader 2>/dev/null); do
    if [[ $(readlink "/proc/$pid/exe" 2>/dev/null) == "$run_dir"/* ]]; then
      echo "job: killing leftover $pid"
      kill -9 "$pid" 2>/dev/null
    fi
  done
  pkill -9 -f "$run_dir/" 2>/dev/null
  nvidia-smi --query-compute-apps=pid,name,used_memory --format=csv,noheader 2>/dev/null | sed 's/^/job: on the card: /'
  date -u +%FT%TZ > "$run_dir/job.ended"
}
trap cleanup EXIT
trap 'exit 143' TERM INT

if [[ $mode != live ]]; then
  python3 -I "$here/fake_model.py" "${AOS_FAKE_PORT:-18950}" "$AOS_IPC_ROOT" cuda "$mode" > "$run_dir/fake-model.log" 2>&1 &
  fake=$!
  sleep 1
fi
timeout --signal=TERM --kill-after=60 "$limit" "$here/aos" run --workflow "$workflow" --state-dir "$run_dir/state" \
  --output "$run_dir/cuda-workspace/final-summary.txt" --report "$run_dir/report.html" --time-budget "${AOS_TIME_BUDGET:-60m}" \
  "Build and optimize a CUDA rasterizer for the Stanford bunny, against the CPU experiment." > "$run_dir/run.log" 2>&1
status=$?
echo "job: aos run exited $status"
python3 -I "$here/metal.py" "$run_dir/state"
python3 -I "$here/analyze.py" "$run_dir/state"
exit $status
