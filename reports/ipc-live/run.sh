#!/bin/zsh
# Usage: run.sh <aos binary> <run name> [aos run flags]. The model endpoint comes from the
# environment. Everything the run writes is under reports/ipc-live/runs/<name>, git-ignored.
set -e
aos=$1; name=$2; shift 2
root=${0:A:h:h:h}
if [[ -z "$AOS_LLM_CONTEXT_WINDOW" ]]; then
  echo "AOS_LLM_CONTEXT_WINDOW is not set: the default policy would apply, with no handoff and 2,000-byte mail batches" >&2
  exit 2
fi
workflow=$(python3 -I $root/reports/ipc-live/render.py $name)
dir=${workflow:h}
set +e
$aos run --workflow $workflow --state-dir $dir/state \
  --output $dir/metal-workspace/final-summary.txt --report $dir/report.html "$@" \
  "Build and optimize a Metal compute-shader rasterizer for the Stanford bunny, against the CPU experiment." > $dir/run.log 2>&1
echo "aos run exited $?; state in $dir/state, log in $dir/run.log"
