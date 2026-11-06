#!/bin/bash
# Tests job.sh and the real render.py with a stub compiler, card and aos: nothing here touches a GPU.
# Usage: test_job.sh. A compiled "binary" here is a script whose outcome the case chooses.
set -u
here=$(cd "$(dirname "$0")" && pwd)
job=$here/job.sh
render=$here/../render.py
workflows=$here/../../../workflows
T=$(mktemp -d /tmp/aos-job-test.XXXXXX)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/cuda/bin" "$T/root/bin" "$T/root/workflows"
cp "$render" "$T/root/bin/render.py"
reference=$T/root/reports/rasterizer-bunny
mkdir -p "$reference/baseline/src" "$reference/baseline/include" "$reference/assets"
touch "$reference/baseline_benchmark.json" "$reference/optimized_benchmark.json" "$reference/final-summary.txt" \
  "$reference/assets/stanford-bunny.obj" "$reference/scene-closeup.json" "$reference/scene-side.json" "$reference/scene-small.json"

cat > "$T/cuda/bin/nvcc" <<'STUB'
#!/bin/bash
out=
form=default
ptx=0
while (( $# )); do
  case $1 in
    --version) echo "Cuda compilation tools, release 13.3, V13.3.73"; exit 0 ;;
    -o) out=$2; shift ;;
    -arch=*) form=card ;;
    -ptx) ptx=1 ;;
  esac
  shift
done
if (( ptx )); then printf '.version 9.3\n.target sm_75\n' > "$out"; exit 0; fi
if [[ $form == default ]]; then outcome=$STUB_DEFAULT; else outcome=$STUB_CARD; fi
if [[ $outcome == nocompile ]]; then echo "nvcc: error"; exit 1; fi
if [[ $outcome == pass ]]; then
  printf '#!/bin/bash\necho kernel_ok=1\n' > "$out"
else
  printf '#!/bin/bash\necho "kernel_ok=0 step=launch error=cudaErrorStub (1): stub"\nexit 1\n' > "$out"
fi
chmod +x "$out"
STUB
cat > "$T/cuda/bin/nvidia-smi" <<'STUB'
#!/bin/bash
case "$*" in
  *uuid,name*) echo "GPU-test, Test Card" ;;
  *compute_cap*) echo "${STUB_CAP-8.6}" ;;
  *memory.free*) echo "100 MiB" ;;
  *query-compute-apps*) ;;
  *) echo "| CUDA Version: 13.2 |" ;;
esac
STUB
printf '#!/bin/bash\nwhile [[ $1 == --* ]]; do shift; done\nshift\nexec "$@"\n' > "$T/cuda/bin/timeout"
printf 'import time\ntime.sleep(5)\n' > "$T/root/bin/fake_model.py"
printf 'pass\n' > "$T/root/bin/metal.py"
printf 'pass\n' > "$T/root/bin/analyze.py"
printf '#!/bin/bash\necho ran > "$AOS_IPC_ROOT/aos-ran"\n' > "$T/root/bin/aos"
chmod +x "$T/cuda/bin/"* "$T/root/bin/aos"

failed=0
n=0
# The real render.py renders this. Only the flags come from the case.
stub_workflow() {
  printf '{"text": "Build with exactly nvcc %s -I <dir>/include <dir>/src/*.cpp -o <dir>/rasterizer; and again nvcc %s -I <dir>/include"}\n' \
    "$STUB_FLAGS" "$STUB_FLAGS" > "$T/root/workflows/rasterizer-bunny-cuda-ipc.json"
}
# expect <case> <exit> <aos ran: yes|no> <text in results or ->
run_case() {
  local title=$1 want=$2 ran=$3 text=$4 mode=$5
  n=$((n + 1))
  rm -f "$T/root/aos-ran"
  local name=run$n dir=$T/root/reports/ipc-live/runs/run$n
  stub_workflow
  AOS_IPC_ROOT=$T/root AOS_CUDA_HOME=$T/cuda AOS_GPU_NAME="Test Card" AOS_FAKE_PORT=1 \
    bash "$job" "$name" "$mode" 30 > "$T/out" 2>&1
  local status=$? bad=
  (( status == want )) || bad="exit $status, wanted $want"
  if [[ -e $T/root/aos-ran ]]; then [[ $ran == yes ]] || bad="$bad; aos ran"; else [[ $ran == no ]] || bad="$bad; aos did not run"; fi
  [[ $(sed -n 2p "$dir/job.ended" 2>/dev/null) == "exit=$want" ]] || bad="$bad; job.ended lacks exit=$want"
  if [[ $text != - ]] && ! grep -qF -- "$text" "$dir/kernel-check/results.txt" 2>/dev/null; then bad="$bad; results lack: $text"; fi
  if [[ -n $bad ]]; then echo "FAIL $title: $bad"; sed 's/^/  /' "$T/out"; failed=1; else echo "ok   $title"; fi
}

export STUB_FLAGS="-O2 -std=c++17" STUB_DEFAULT=pass STUB_CARD=pass
run_case "both pass, workflow default" 0 yes "check card (-O2 -std=c++17 -arch=sm_86): exit 0" preflight
export STUB_DEFAULT=fail
run_case "default fails, workflow default: stops, card still recorded" 6 no "check card (-O2 -std=c++17 -arch=sm_86): exit 0" preflight
run_case "the failure is named" 6 no "step=launch error=cudaErrorStub" preflight
export STUB_FLAGS="-O2 -std=c++17 -arch=sm_86"
run_case "default fails, workflow pinned: continues" 0 yes "check default (-O2 -std=c++17): exit 1" preflight
export STUB_DEFAULT=pass STUB_CARD=fail
run_case "card fails, workflow pinned: stops" 6 no "check default (-O2 -std=c++17): exit 0" preflight
export STUB_FLAGS="-O3" STUB_CARD=pass
run_case "workflow flags are neither: stops" 6 no - preflight
export STUB_FLAGS="-O2 -std=c++17" STUB_DEFAULT=nocompile
run_case "default does not compile: stops, card still recorded" 6 no "check card (-O2 -std=c++17 -arch=sm_86): exit 0" preflight
export STUB_DEFAULT=fail
run_case "dry compiles and runs neither" 0 yes "workflow compiles with: -O2 -std=c++17 (default)" dry
export STUB_DEFAULT=pass STUB_CAP=
run_case "unreadable compute capability: stops" 6 no - preflight
unset STUB_CAP

export STUB_FLAGS="-O2 -std=c++17 -arch=sm_86"
missing=$reference/scene-small.json
rm "$missing"
run_case "render fails: stops, job.ended says so" 5 no - preflight
touch "$missing"

outcome() {
  n=$((n + 1))
  if [[ -n $2 ]]; then echo "FAIL $1: $2"; sed 's/^/  /' "$T/out"; failed=1; else echo "ok   $1"; fi
}
launch() {
  AOS_IPC_ROOT=$T/root AOS_CUDA_HOME=$T/cuda AOS_GPU_NAME="Test Card" AOS_FAKE_PORT=1 \
    bash "$job" "$1" preflight 30 > "$T/out" 2>&1
}

# The task as agents get it, through the real render.py and job.sh, for a new run and then the same name again.
cp "$workflows/rasterizer-bunny-cuda-ipc.json" "$workflows/rasterizer-bunny-metal-ipc.json" "$T/root/workflows/"
dir=$T/root/reports/ipc-live/runs/real
export STUB_DEFAULT=fail STUB_CARD=pass
rm -f "$T/root/aos-ran"
launch real
status=$? bad=
(( status == 0 )) || bad="exit $status"
[[ -e $T/root/aos-ran ]] || bad="$bad; aos did not run"
grep -q __GPU_UUID__ "$dir/workflow.json" && bad="$bad; the workflow was not filled in"
grep -qF "workflow compiles with: -O2 -std=c++17 -arch=sm_86 (card)" "$dir/kernel-check/results.txt" || bad="$bad; the card's build does not decide"
[[ $(sed -n 2p "$dir/job.ended") == exit=0 ]] || bad="$bad; job.ended lacks exit=0"
outcome "the real workflow, a new run: renders, and the card's build decides" "$bad"

before=$(cat "$dir/workflow.json" "$dir/job.ended")
bash -c 'exec -a "$0" sleep 30' "$dir/leftover" &
leftover=$!
sleep 0.3
rm -f "$T/root/aos-ran"
launch real
status=$? bad=
(( status == 5 )) || bad="exit $status"
[[ -e $T/root/aos-ran ]] && bad="$bad; aos ran"
[[ $(cat "$dir/workflow.json" "$dir/job.ended") == "$before" ]] || bad="$bad; the old run's files changed"
kill -0 "$leftover" 2>/dev/null || bad="$bad; the old run's process was killed"
kill "$leftover" 2>/dev/null
wait "$leftover" 2>/dev/null
outcome "the same name again: refused, and the old run is untouched" "$bad"

# render.py itself, after job.sh's mkdir and as run.sh calls it with no directory, for both kinds.
render_both_ways() {
  local kind=$1 made=$2 name=direct-$1-$2 dir=$T/root/reports/ipc-live/runs/direct-$1-$2 bad=
  [[ $made == made ]] && mkdir -p "$dir"
  AOS_IPC_ROOT=$T/root AOS_CUDA_HOME=$T/cuda AOS_GPU_UUID=GPU-test python3 -I "$render" "$name" "$kind" > "$T/out" 2>&1 || bad="a new run was refused"
  [[ -s $dir/workflow.json ]] || bad="$bad; no workflow.json"
  python3 -c 'import json, sys; json.load(open(sys.argv[1]))' "$dir/workflow.json" 2>/dev/null || bad="$bad; the workflow is not JSON"
  local first
  first=$(cat "$dir/workflow.json" 2>/dev/null)
  AOS_IPC_ROOT=$T/root AOS_CUDA_HOME=$T/cuda AOS_GPU_UUID=GPU-test python3 -I "$render" "$name" "$kind" > "$T/out" 2>&1 && bad="$bad; a reused run was allowed"
  [[ $(cat "$dir/workflow.json" 2>/dev/null) == "$first" ]] || bad="$bad; the reused run's workflow changed"
  grep -q Traceback "$T/out" && bad="$bad; refused with a traceback"
  outcome "render.py $kind, directory $made: a new run passes, a reused one is refused" "$bad"
}
render_both_ways cuda made
render_both_ways cuda absent
render_both_ways metal made
render_both_ways metal absent

(( failed == 0 )) && echo "all $n passed"
exit $failed
