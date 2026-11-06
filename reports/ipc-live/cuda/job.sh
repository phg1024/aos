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

mkdir -p "$run_dir"
# Before the trap: cleanup would kill what runs from that directory and overwrite its job.ended.
if [[ -e $run_dir/workflow.json ]]; then
  echo "job: $run_dir/workflow.json exists: this run name was used: nothing else ran" >&2
  exit 5
fi
fake=
cleanup() {
  local status=$?
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
  { date -u +%FT%TZ; echo "exit=$status"; } > "$run_dir/job.ended"
}
trap cleanup EXIT
trap 'exit 143' TERM INT

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

workflow=$(python3 -I "$here/render.py" "$name" cuda-handles) || exit 5
echo "job: $workflow"

# The kernel check, before any model call: compile and run one small kernel. This outside
# the sandbox says the card and driver work; aos's own runs say the sandbox does. It compiles
# with nvcc's default and for this card's own architecture and runs both, so that one window
# says what each does here. Only the one the workflow's agents are told to use decides.
check=$run_dir/kernel-check
mkdir -p "$check"
cat > "$check/check.cu" <<'CU'
#include <cuda_runtime.h>
#include <chrono>
#include <cstdio>
__global__ void doubled(unsigned* values) { values[threadIdx.x] = 2 * threadIdx.x; }
static int fail(const char* step, cudaError_t error) {
    std::printf("kernel_ok=0 step=%s error=%s (%d): %s\n", step, cudaGetErrorName(error), (int)error, cudaGetErrorString(error));
    return 1;
}
int main() {
    const unsigned untouched = 0xABABABABu;
    cudaDeviceProp prop;
    cudaError_t error = cudaGetDeviceProperties(&prop, 0);
    if (error != cudaSuccess) return fail("properties", error);
    std::printf("device=%s compute_capability=%d.%d\n", prop.name, prop.major, prop.minor);
    unsigned* device = nullptr;
    unsigned host[256] = {0};
    if ((error = cudaMalloc(&device, sizeof(host))) != cudaSuccess) return fail("malloc", error);
    // A marker, so that a kernel which never ran does not read as a wrong answer.
    if ((error = cudaMemset(device, 0xAB, sizeof(host))) != cudaSuccess) return fail("memset", error);
    auto start = std::chrono::steady_clock::now();
    doubled<<<1, 256>>>(device);
    if ((error = cudaGetLastError()) != cudaSuccess) return fail("launch", error);
    if ((error = cudaDeviceSynchronize()) != cudaSuccess) return fail("synchronize", error);
    std::printf("launch_ms=%.3f\n", std::chrono::duration<double, std::milli>(std::chrono::steady_clock::now() - start).count());
    if ((error = cudaMemcpy(host, device, sizeof(host), cudaMemcpyDeviceToHost)) != cudaSuccess) return fail("memcpy", error);
    cudaFree(device);
    unsigned wrong = 0, marked = 0, first = 256;
    for (unsigned i = 0; i < 256; i++) {
        if (host[i] != 2 * i) { wrong++; if (first == 256) first = i; }
        if (host[i] == untouched) marked++;
    }
    if (wrong > 0) {
        std::printf("kernel_ok=0 step=values wrong=%u untouched=%u first_wrong=%u got=0x%x expected=0x%x\n", wrong, marked, first, host[first], 2 * first);
        return 1;
    }
    std::printf("kernel_ok=1\n");
    return 0;
}
CU
results=$check/results.txt
base="-O2 -std=c++17"
cap=$(nvidia-smi --id="$AOS_GPU_UUID" --query-gpu=compute_cap --format=csv,noheader | tr -d '. ')
if [[ ! $cap =~ ^[0-9]+$ ]]; then
  echo "job: the card's compute capability is unreadable: $cap: nothing else ran" >&2
  exit 6
fi
card="$base -arch=sm_$cap"
# Exactly what the agents are told, so that this check cannot drift from the task.
uses=$(grep -oE 'nvcc [^<]+ -I <' "$workflow" | sort -u | sed -E 's/^nvcc (.*) -I <$/\1/')
case $uses in
  "$base") gate=default ;;
  "$card") gate=card ;;
  *) echo "job: the workflow's nvcc flags are neither the default nor this card's: $uses: nothing else ran" >&2; exit 6 ;;
esac
{
  echo "nvcc: $(nvcc --version | grep -m1 release)"
  echo "driver: $(nvidia-smi | grep -oE 'CUDA Version: [0-9.]+')"
  echo "card: sm_$cap"
  echo "workflow compiles with: $uses ($gate)"
  nvcc $base -ptx "$check/check.cu" -o "$check/default.ptx" && echo "default ptx: $(grep -m1 '^\.version' "$check/default.ptx"), $(grep -m1 '^\.target' "$check/default.ptx")"
} 2>&1 | tee -a "$results"

# Compile and run one form. Its status is the check's; it never stops the other form.
check_one() {
  local form=$1 flags=$2 status
  if ! nvcc $flags "$check/check.cu" -o "$check/check-$form" > "$check/compile-$form.log" 2>&1; then
    sed "s/^/check $form: /" "$check/compile-$form.log" | tee -a "$results"
    echo "check $form ($flags): did not compile" | tee -a "$results"
    return 1
  fi
  [[ $mode == dry ]] && return 0
  timeout 60 "$check/check-$form" > "$check/run-$form.log" 2>&1
  status=$?
  sed "s/^/check $form: /" "$check/run-$form.log" | tee -a "$results"
  echo "check $form ($flags): exit $status" | tee -a "$results"
  return $status
}
check_one default "$base"; default_status=$?
check_one card "$card"; card_status=$?
if [[ $gate == default ]]; then gate_status=$default_status; else gate_status=$card_status; fi
if (( gate_status != 0 )); then
  echo "job: the kernel check failed with the workflow's flags ($gate: $uses): nothing else ran" >&2
  exit 6
fi

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
