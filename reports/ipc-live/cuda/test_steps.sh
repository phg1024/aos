#!/bin/bash
# job.sh's real steps on the machine with the card, up to compiling the kernel check. Usage, on the machine:
# AOS_GPU_NAME=<card> test_steps.sh. Real here: job.sh, render.py, the workflow, the CPU reference, nvidia-smi's
# queries and nvcc. Mode dry stops before the compiled check runs, and aos, the scripted model, metal.py and
# analyze.py are stubs, so no CUDA context opens on the card. It runs outside Ray, in a directory of its own.
set -u
: "${AOS_GPU_NAME:?}"
live=$HOME/aos-ipc-live
T=$(mktemp -d "$live/test-steps.XXXXXX")
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/workflows" "$T/reports"
cp "$live/bin/job.sh" "$live/bin/render.py" "$T/bin/"
cp "$live/workflows/rasterizer-bunny-cuda-ipc.json" "$T/workflows/"
ln -s "$live/reports/rasterizer-bunny" "$T/reports/rasterizer-bunny"
printf 'import time\ntime.sleep(5)\n' > "$T/bin/fake_model.py"
printf 'pass\n' > "$T/bin/metal.py"
printf 'pass\n' > "$T/bin/analyze.py"
printf '#!/bin/bash\necho ran > "$AOS_IPC_ROOT/aos-ran"\n' > "$T/bin/aos"
chmod +x "$T/bin/aos"

apps() { nvidia-smi --query-compute-apps=pid --format=csv,noheader | sort; }
step() {
  env -i HOME="$HOME" AOS_IPC_ROOT="$T" AOS_GPU_NAME="$AOS_GPU_NAME" AOS_FAKE_PORT=18959 /bin/bash "$T/bin/job.sh" steps dry 60 2>&1
}
before=$(apps)
step
echo "--- exit $?"
dir=$T/reports/ipc-live/runs/steps
echo "aos stub reached: $([[ -e $T/aos-ran ]] && echo yes || echo no)"
echo "workflow is JSON: $(python3 -c 'import json, sys; json.load(open(sys.argv[1])); print("yes")' "$dir/workflow.json")"
echo "placeholders left: $(grep -c '__[A-Z_]*__' "$dir/workflow.json")"
echo "check binaries: $(cd "$dir/kernel-check" && ls -d check-* | tr '\n' ' ')"
echo "run logs, which would mean a binary ran: $(cd "$dir/kernel-check" && ls run-*.log 2>/dev/null | wc -l)"
echo "job.ended: $(tr '\n' ' ' < "$dir/job.ended")"
first=$(cat "$dir/job.ended")
rm -f "$T/aos-ran"
sleep 1
echo "--- the same name again"
step
echo "--- exit $?"
echo "aos stub reached: $([[ -e $T/aos-ran ]] && echo yes || echo no)"
echo "job.ended unchanged: $([[ $(cat "$dir/job.ended") == "$first" ]] && echo yes || echo no)"
echo "compute apps unchanged by this script: $([[ $(apps) == "$before" ]] && echo yes || echo no)"
