#!/bin/bash
# Tests job.sh's kernel-check logic with a stub compiler, card and aos: nothing here touches a GPU.
# Usage: test_job.sh. A compiled "binary" here is a script whose outcome the case chooses.
set -u
job=$(cd "$(dirname "$0")" && pwd)/job.sh
T=$(mktemp -d /tmp/aos-job-test.XXXXXX)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/cuda/bin" "$T/root/bin"

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
cat > "$T/root/bin/render.py" <<'STUB'
import os, sys
run_dir = os.path.join(os.environ["AOS_IPC_ROOT"], "reports/ipc-live/runs", sys.argv[1])
os.makedirs(run_dir, exist_ok=True)
out = os.path.join(run_dir, "workflow.json")
with open(out, "w") as f:
    f.write("Build with exactly nvcc " + os.environ["STUB_FLAGS"] + " -I <dir>/include <dir>/src/*.cpp -o <dir>/rasterizer; and again nvcc " + os.environ["STUB_FLAGS"] + " -I <dir>/include")
print(out)
STUB
printf 'import time\ntime.sleep(5)\n' > "$T/root/bin/fake_model.py"
printf 'pass\n' > "$T/root/bin/metal.py"
printf 'pass\n' > "$T/root/bin/analyze.py"
printf '#!/bin/bash\necho ran > "$AOS_IPC_ROOT/aos-ran"\n' > "$T/root/bin/aos"
chmod +x "$T/cuda/bin/"* "$T/root/bin/aos"

failed=0
n=0
# expect <case> <exit> <aos ran: yes|no> <text in results or ->
run_case() {
  local title=$1 want=$2 ran=$3 text=$4 mode=$5
  n=$((n + 1))
  rm -f "$T/root/aos-ran"
  local name=run$n dir=$T/root/reports/ipc-live/runs/run$n
  AOS_IPC_ROOT=$T/root AOS_CUDA_HOME=$T/cuda AOS_GPU_NAME="Test Card" AOS_FAKE_PORT=1 \
    bash "$job" "$name" "$mode" 30 > "$T/out" 2>&1
  local status=$? bad=
  (( status == want )) || bad="exit $status, wanted $want"
  if [[ -e $T/root/aos-ran ]]; then [[ $ran == yes ]] || bad="$bad; aos ran"; else [[ $ran == no ]] || bad="$bad; aos did not run"; fi
  [[ -e $dir/job.ended ]] || bad="$bad; no job.ended"
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

(( failed == 0 )) && echo "all $n passed"
exit $failed
