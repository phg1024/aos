#!/bin/bash
# The preflight's whole path on the machine, with nothing on the card. Usage, on the machine: AOS_GPU_NAME=<card> test_path.sh.
# Real here: job.sh, render.py, the workflow, aos, its Landlock sandbox, the scripted model, g++, /usr/bin/time, the CPU
# reference and analyze.py. Stubs: nvcc, whose "binary" is a script printing the program's line, and nvidia-smi inside the
# job. The one real nvidia-smi call is the query for the card's UUID, as the sandbox's grant names it; a grant holds the device
# files by path and opens none. So no CUDA context opens. It runs outside Ray, in a directory of its own, which it removes.
# With unread as its argument it plays live3's pid-000009: no handle goes with the delegation, the verifier's read is refused, it
# tries to exit 0, and the run must not pass. The clean path attaches at the action level, beside the delegate, as live4's
# coordinator did; with inner as its argument it attaches inside the delegate, as pf5 did.
set -u
: "${AOS_GPU_NAME:?}"
path=${1:-clean}
live=$HOME/aos-ipc-live
uuid=$(nvidia-smi --query-gpu=uuid,name --format=csv,noheader | grep -F "$AOS_GPU_NAME" | cut -d, -f1)
[[ $uuid == GPU-* ]] || { echo "no such card: $AOS_GPU_NAME"; exit 2; }
T=$(mktemp -d "$live/test-path.XXXXXX")
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/workflows" "$T/reports" "$T/cuda/bin"
cp "$live/bin/aos" "$live/bin/job.sh" "$live/bin/render.py" "$live/bin/analyze.py" "$live/bin/fake_model.py" "$live/bin/metal.py" "$T/bin/"
cp "$live/workflows/rasterizer-bunny-cuda-evidence-ipc.json" "$T/workflows/"
[[ $path != unread ]] || sed -i 's/^    unread = False$/    unread = True/' "$T/bin/fake_model.py"
[[ $path != inner ]] || sed -i 's/^    inner = False$/    inner = True/' "$T/bin/fake_model.py"
ln -s "$live/reports/rasterizer-bunny" "$T/reports/rasterizer-bunny"
ln -s "$live/skills" "$T/skills"
cat > "$T/cuda/bin/nvcc" <<'STUB'
#!/bin/bash
out=
ptx=0
while (( $# )); do
  case $1 in
    --version) echo "Cuda compilation tools, stub"; exit 0 ;;
    -o) out=$2; shift ;;
    -ptx) ptx=1 ;;
  esac
  shift
done
if (( ptx )); then printf '.version 9.3\n.target sm_75\n' > "$out"; exit 0; fi
printf '#!/bin/bash\necho kernel_ok=1\necho render_ms_best=2.640 render_ms_avg=3.070 gpu_ms_best=2.310 threads_used=1\n' > "$out"
chmod +x "$out"
STUB
cat > "$T/cuda/bin/nvidia-smi" <<STUB
#!/bin/bash
case "\$*" in
  *uuid,name*) echo "$uuid, $AOS_GPU_NAME" ;;
  *compute_cap*) echo "8.6" ;;
  *memory.free*) echo "100 MiB" ;;
  *query-compute-apps*) ;;
  *query-gpu*) printf 'name, uuid, memory.used [MiB], memory.total [MiB], utilization.gpu [%%], temperature.gpu\nStub Card, $uuid, 225 MiB, 10240 MiB, 40 %%, 50\n' ;;
  *) echo "| CUDA Version: 13.2 |" ;;
esac
STUB
chmod +x "$T/cuda/bin/"*

before=$(nvidia-smi --query-compute-apps=pid --format=csv,noheader | sort)
env -i HOME="$HOME" AOS_IPC_ROOT="$T" AOS_CUDA_HOME="$T/cuda" AOS_GPU_NAME="$AOS_GPU_NAME" AOS_FAKE_PORT=18958 /bin/bash "$T/bin/job.sh" rehearsal preflight 600 > "$T/job.out" 2>&1
status=$?
dir=$T/reports/ipc-live/runs/rehearsal
echo "job exit: $status"
sed -n '/== Counts/,$p' "$T/job.out"
failed=0
need() { if grep -qF -- "$1" "$T/job.out"; then echo "ok   $1"; else echo "FAIL $1"; failed=1; fi; }
if [[ $path == unread ]]; then
  need "UNVERIFIED:"
  need "declared raw_record by benchmarker: "
  need "exercised 1      a body refused as too large"
  if grep -qE '^exercised [0-9]+ +an exit 0 refused for unread evidence' "$T/job.out"; then echo "ok   an exit 0 refused"; else echo "FAIL an exit 0 refused"; failed=1; fi
  need "a verdict withheld as unverified"
  if grep -qE '^declared raw_record by benchmarker: art-[0-9]+: unread$' "$T/job.out"; then echo "ok   the record unread"; else echo "FAIL the record unread"; failed=1; fi
  if [[ $status == 0 ]]; then echo "FAIL the job passed"; failed=1; else echo "ok   the job did not pass: exit $status"; fi
  if grep -q '^coordinator told: attached=none$' "$dir/fake-model.log"; then echo "ok   the coordinator was told none was attached"; else echo "FAIL the coordinator was not told none was attached"; failed=1; fi
else
  need "CLEAN: the judged verifier read the latest record"
  need "exits refused for evidence: 0"
  need "exercised 1      a body refused as too large"
  for label in "an artifact published" "the verifier reading evidence to its end" "a handle attached at a delegation"; do
    if grep -qE "^exercised [0-9]+ +$label" "$T/job.out"; then echo "ok   $label"; else echo "FAIL $label"; failed=1; fi
  done
  # What live3's three verifiers all met: with the handle at the delegation, no read is refused and nobody waits.
  need "NOT EXERCISED    a read denied"
  need "NOT EXERCISED    the verifier pausing with a wait"
  if grep -qE 'attached at delegation by coordinator \(pid-[0-9]+\) to verifier \(pid-[0-9]+\).*: read [2-9] pages, to the end' "$T/job.out"; then echo "ok   the record took more than one page, read to the end"; else echo "FAIL the record's reading"; failed=1; fi
  if grep -q 'UNCLEAN\|DISAGREE' "$T/job.out"; then echo "FAIL an unclean pass"; failed=1; fi
  # The default puts the handles at the action level, as live4's coordinator did; inner puts them inside the delegate.
  if grep -qE '^coordinator told: attached=art-[0-9]+' "$dir/fake-model.log"; then echo "ok   the coordinator was told what was attached ($path)"; else echo "FAIL the coordinator was not told what was attached ($path)"; failed=1; fi
  need "an attachment in a delegate reply not granted: 0"
  if [[ $status != 0 ]]; then sed 's/^/job: /' "$T/job.out" | tail -30; tail -5 "$dir/run.log"; cat "$dir/fake-model.log" | tail -20; failed=1; fi
fi
budget=$(python3 -I -c 'import json, sys; print(next(e["data"].get("budget_seconds") for e in json.load(open(sys.argv[1])) if e["type"] == "run.started"))' "$dir/state/kernel/events.json")
if [[ $budget == 600 ]]; then echo "ok   the job's limit is the run's budget: 600"; else echo "FAIL the run's budget is $budget, the job's limit 600"; failed=1; fi
echo "record bytes: $(wc -c < "$dir/cuda-bench/raw_record.txt")"
echo "verifier's report: $(cat "$dir/cuda-verification/cuda_verification.txt")"
echo "planted in the source: $([[ -e $dir/cuda-workspace/cuda/should-be-refused ]] && echo yes || echo no)"
echo "compute apps unchanged by this script: $([[ $(nvidia-smi --query-compute-apps=pid --format=csv,noheader | sort) == "$before" ]] && echo yes || echo no)"
echo "job.ended: $(tr '\n' ' ' < "$dir/job.ended")"
(( failed == 0 )) && echo "all passed"
exit $failed
