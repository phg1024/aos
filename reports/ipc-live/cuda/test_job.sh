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
# STUB_ALARM sends what it runs SIGTERM after that many seconds, standing in for the job's limit.
cat > "$T/cuda/bin/timeout" <<'STUB'
#!/bin/bash
while [[ $1 == --* ]]; do shift; done
shift
[[ -z ${STUB_ALARM:-} ]] && exec "$@"
"$@" &
pid=$!
(sleep "$STUB_ALARM"; kill -TERM "$pid" 2>/dev/null) &
wait "$pid"
STUB
printf 'import time\ntime.sleep(5)\n' > "$T/root/bin/fake_model.py"
printf 'pass\n' > "$T/root/bin/metal.py"
printf 'pass\n' > "$T/root/bin/analyze.py"
printf '#!/bin/bash\necho ran > "$AOS_IPC_ROOT/aos-ran"\nprintf "%%s" "${AOS_LLM_TIMEOUT_SECONDS-unset}" > "$AOS_IPC_ROOT/aos-timeout"\n' > "$T/root/bin/aos"
chmod +x "$T/cuda/bin/"* "$T/root/bin/aos"

failed=0
n=0
# The real render.py renders this. Only the flags come from the case.
stub_workflow() {
  printf '{"text": "Build with exactly nvcc %s -I <dir>/include <dir>/src/*.cpp -o <dir>/rasterizer; and again nvcc %s -I <dir>/include"}\n' \
    "$STUB_FLAGS" "$STUB_FLAGS" > "$T/root/workflows/rasterizer-bunny-cuda-handles-ipc.json"
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
cp "$workflows/rasterizer-bunny-cuda-handles-ipc.json" "$workflows/rasterizer-bunny-cuda-ipc.json" "$workflows/rasterizer-bunny-metal-ipc.json" "$T/root/workflows/"
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
[[ -d $dir/cuda-bench ]] || bad="$bad; no private directory"
grep -q '"name": "rasterizer-bunny-cuda-handles-ipc"' "$dir/workflow.json" || bad="$bad; another workflow was rendered"
outcome "the real workflow, a new run: renders the handles variant with its directory, and the card's build decides" "$bad"

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
  if [[ $kind == cuda-handles ]]; then [[ -d $dir/cuda-bench ]] || bad="$bad; no private directory"; else [[ -e $dir/cuda-bench ]] && bad="$bad; a private directory it does not use"; fi
  outcome "render.py $kind, directory $made: a new run passes, a reused one is refused" "$bad"
}
render_both_ways cuda-handles made
render_both_ways cuda-handles absent
render_both_ways cuda made
render_both_ways cuda absent
render_both_ways metal made
render_both_ways metal absent

# submit.sh to the job's environment. This stands in for Ray: it runs the entrypoint on the machine's
# terms, with nothing of the submitter's environment but what the entrypoint itself sets.
mkdir -p "$T/home/aos-cuda"
ln -s "$T/root" "$T/home/aos-ipc-live"
ln -s "$T/cuda" "$T/home/aos-cuda/env"
cp "$job" "$T/root/bin/job.sh"
cat > "$T/ray" <<'STUB'
#!/bin/bash
echo called > "$STUB_RAY_MARK"
while [[ $1 != -- ]]; do shift; done
shift
exec env -i HOME="$STUB_HOME" STUB_DEFAULT=pass STUB_CARD=pass STUB_ALARM="${STUB_ALARM:-}" "$@"
STUB
chmod +x "$T/ray"
submit() {
  STUB_HOME=$T/home STUB_RAY_MARK=$T/ray-called AOS_RAY=$T/ray AOS_RAY_ADDRESS=stub AOS_RAY_RESOURCES='{}' \
    AOS_JOB_HOME=$T/home AOS_GPU_NAME="Test Card" zsh "$here/submit.sh" "id-$1" "$1" preflight 30 > "$T/out" 2>&1
}
submits_with() {
  local title=$1 want=$2 name=submit$((n + 1)) bad=
  shift 2
  rm -f "$T/root/aos-timeout" "$T/ray-called"
  env "$@" bash -c "$(declare -f submit); T='$T' here='$here'; submit $name" || bad="exit $?"
  [[ $(cat "$T/root/aos-timeout" 2>/dev/null) == "$want" ]] || bad="$bad; aos saw $(cat "$T/root/aos-timeout" 2>/dev/null)"
  outcome "submit.sh, $title: aos gets $want" "$bad"
}
refuses() {
  local value=$1 name=submit$((n + 1)) bad=
  rm -f "$T/root/aos-timeout" "$T/ray-called"
  env AOS_LLM_TIMEOUT_SECONDS="$value" bash -c "$(declare -f submit); T='$T' here='$here'; submit $name" && bad="it submitted"
  [[ -e $T/ray-called ]] && bad="$bad; Ray was called"
  grep -qF "must be a whole number" "$T/out" || bad="$bad; no reason given"
  outcome "submit.sh refuses $value, which is not a whole number of seconds" "$bad"
}
submits_with "with nothing set" 1200 -u AOS_LLM_TIMEOUT_SECONDS
submits_with "with an empty value" 1200 AOS_LLM_TIMEOUT_SECONDS=
submits_with "with a value chosen" 600 AOS_LLM_TIMEOUT_SECONDS=600
for value in 2m 0 -5 1.5; do refuses "$value"; done

# The same path with the real aos and an endpoint that never answers: what the run records and how it retries.
(cd "$here/../../.." && go build -buildvcs=false -o "$T/aos-real" ./cmd/aos) || { echo "FAIL could not build aos"; exit 1; }
cp "$T/aos-real" "$T/root/bin/aos"
cp -R "$here/../../../skills" "$T/root/skills"
cat > "$T/root/bin/fake_model.py" <<'STUB'
import socket, sys, time
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", int(sys.argv[1])))
listener.listen(16)
held = []
while True:
    held.append(listener.accept())
STUB
rm -f "$T/ray-called"
STUB_ALARM=6 AOS_LLM_TIMEOUT_SECONDS=1 submit realaos
dir=$T/root/reports/ipc-live/runs/realaos
bad=$(python3 -I - "$dir/state/kernel/events.json" <<'PY'
import json, sys
events = json.load(open(sys.argv[1]))
started = [e for e in events if e["type"] == "run.started"]
retries = [e["data"] for e in events if e["type"] == "backend.retry"]
if not started or started[0]["data"].get("timeout") != "1s":
    print("run.started says", started and started[0]["data"].get("timeout"))
elif len(retries) < 2 or not all("Client.Timeout" in r["error"] for r in retries):
    print("retries:", retries)
elif [r["delay"] for r in retries[:2]] != ["1s", "2s"]:
    print("delays:", retries)
PY
)
[[ -n $bad ]] && sed 's/^/  run.log: /' "$dir/run.log" > "$T/out"
outcome "the real aos records the value in run.started, and retries a timeout with backoff" "$bad"

(( failed == 0 )) && echo "all $n passed"
exit $failed
