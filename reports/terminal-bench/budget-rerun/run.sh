#!/usr/bin/env bash
# Runs the budget rerun's trials in schedule.json order, one Harbor job each,
# against the Strata on phg-gpu through relay.py (which must be running on
# 127.0.0.1:18080; see PLAN.md, A0). Before each trial it checks the backend.
# After each trial, collect.py appends the trial's record to results.jsonl
# and applies the stop rule. Trials already in results.jsonl are skipped, so
# the script can be restarted after a stop. Nothing is ever rerun.
#
#   reports/terminal-bench/budget-rerun/run.sh            # the 72 trials
#   reports/terminal-bench/budget-rerun/run.sh --smoke    # the smoke trial only
#
# The binaries are built once into .cache/harbor-dist-budget (PLAN.md, A1);
# this script does not rebuild them.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
export AOS_HARBOR_DIST="$ROOT/.cache/harbor-dist-budget"
export PYTHONPATH="$ROOT/bench/harbor"
export AOS_LLM_BACKEND=openai-compatible
export AOS_LLM_BASE_URL=http://127.0.0.1:18080
export AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4
export AOS_LLM_API_KEY=
unset AOS_TIME_BUDGET
HARBOR="${HARBOR:-$(command -v harbor || echo "$HOME/.local/bin/harbor")}"
JOBS="$ROOT/reports/terminal-bench/jobs"
COLLECT=(python3 -I "$HERE/collect.py")
mkdir -p "$HERE/logs"

trials() {
  if [[ "${1:-}" == "--smoke" ]]; then
    echo "0 0 fix-git strict tb2-budget-smoke-fix-git-strict"
  else
    python3 -I -c 'import json,sys
for t in json.load(open(sys.argv[1]))["trials"]:
    print(t["seq"], t["round"], t["task"], t["arm"], t["job"])' "$HERE/schedule.json"
  fi
}

trials "${1:-}" | while read -r seq round task arm job; do
  if "${COLLECT[@]}" done "$seq"; then
    continue
  fi
  # Backend check before the trial: stop on a change or no answer.
  before="$("${COLLECT[@]}" backend)" || { echo "stop: backend check failed before seq $seq"; exit 3; }
  echo "$(date -u +%FT%TZ) seq $seq round $round $task $arm"
  "$HARBOR" run \
    --dataset terminal-bench@2.0 \
    --agent aos_agent:AOSAgent \
    --ak "time_budget=$arm" \
    --n-concurrent 1 \
    --allow-agent-host host.docker.internal \
    --jobs-dir "$JOBS" \
    --job-name "$job" \
    -i "$task" \
    --yes < /dev/null > "$HERE/logs/$job.log" 2>&1 || echo "harbor exit $? for $job"
  after="$("${COLLECT[@]}" backend)" || after='{"error": "no answer"}'
  "${COLLECT[@]}" record "$seq" "$round" "$task" "$arm" "$JOBS/$job" "$before" "$after" \
    || { echo "stop: rule met after seq $seq"; exit 4; }
done
