#!/usr/bin/env bash
# Runs the TB2 baseline's trials in schedule.json order, one Harbor job each,
# against the Strata on phg-gpu through relay.py (which must be running on
# 127.0.0.1:18080; see relay-start.sh). Around each trial it checks the
# backend and the daily window, and after it, collect.py appends the record
# and applies the stop rule. Trials already recorded are skipped, so the
# script can be restarted after a pause. Nothing is ever rerun.
#
#   reports/terminal-bench/baseline-tb2/run.sh             # the 96 trials
#   reports/terminal-bench/baseline-tb2/run.sh --smoke     # the two smoke trials
#
# The AOS binaries are built once into .cache/harbor-dist-v0.1.10 (PLAN.md);
# this script does not rebuild them.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
HARBOR="${HARBOR:-$(command -v harbor || echo "$HOME/.local/bin/harbor")}"
JOBS="$ROOT/reports/terminal-bench/jobs"
COLLECT=(python3 -I "$HERE/collect.py")
mkdir -p "$HERE/logs"
# No key reaches Harbor, the agents or the containers: the relay adds it.
unset ANTHROPIC_API_KEY OPENAI_API_KEY AOS_LLM_API_KEY HOSTED_VLLM_API_KEY AOS_TIME_BUDGET

trials() {
  if [[ "${1:-}" == "--smoke" ]]; then
    task=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["smoke_task"])' "$HERE/drawn.json")
    echo "0 0 $task terminus-2 tb2-base-smoke-$task-terminus-2"
    echo "-1 0 $task aos tb2-base-smoke-$task-aos"
  else
    python3 -I -c 'import json,sys
for t in json.load(open(sys.argv[1]))["trials"]:
    print(t["seq"], t["round"], t["task"], t["arm"], t["job"])' "$HERE/schedule.json"
  fi
}

# Seconds to wait so that the trial cannot still be running in 08:55-09:10
# Pacific: its worst case is the agent timeout plus the verifier timeout plus
# 10 minutes.
window_wait() {
  python3 -I - "$HERE/survey.json" "$1" <<'PY'
import json, sys
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo
task = next(t for t in json.load(open(sys.argv[1])) if t["task"] == sys.argv[2])
worst = timedelta(seconds=task["agent_timeout_s"] + task["verifier_timeout_s"] + 600)
now = datetime.now(ZoneInfo("America/Los_Angeles"))
for day in (now.date(), now.date() + timedelta(days=1), now.date() + timedelta(days=2)):
    lo = datetime(day.year, day.month, day.day, 8, 55, tzinfo=now.tzinfo)
    hi = datetime(day.year, day.month, day.day, 9, 10, tzinfo=now.tzinfo)
    if now < hi and now + worst > lo:
        print(int((hi - now).total_seconds()) + 1)
        break
else:
    print(0)
PY
}

harbor_args() {  # arm -> the agent-specific arguments, one per line
  if [[ "$1" == aos ]]; then
    printf '%s\n' --agent aos_agent:AOSAgent --ak time_budget=strict
  else
    python3 -I - "$HERE/terminus2-settings.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
print("--agent"); print("terminus-2"); print("--model"); print(s["model"])
for k, v in s["agent_kwargs"].items():
    print("--ak"); print(f"{k}={json.dumps(v) if isinstance(v, (dict, list)) else v}")
PY
  fi
}

trials "${1:-}" | while read -r seq round task arm job; do
  if "${COLLECT[@]}" done "$seq"; then
    continue
  fi
  wait_s=$(window_wait "$task")
  if (( wait_s > 0 )); then
    echo "$(date '+%F %T %Z') seq $seq: waiting ${wait_s}s for the 08:55-09:10 window"
    sleep "$wait_s"
  fi
  before="$("${COLLECT[@]}" backend)" || { echo "stop: backend check failed before seq $seq: $before"; exit 3; }
  echo "$(date '+%F %T %Z') seq $seq round $round $task $arm"
  args=()
  while IFS= read -r a; do args+=("$a"); done < <(harbor_args "$arm")
  (
    if [[ "$arm" == aos ]]; then
      export AOS_HARBOR_DIST="$ROOT/.cache/harbor-dist-v0.1.10"
      export PYTHONPATH="$ROOT/bench/harbor"
      export AOS_LLM_BACKEND=openai-compatible AOS_LLM_BASE_URL=http://127.0.0.1:18080
      export AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4 AOS_LLM_API_KEY=
    fi
    "$HARBOR" run \
      --dataset terminal-bench@2.0 \
      "${args[@]}" \
      --n-concurrent 1 \
      --allow-agent-host host.docker.internal \
      --jobs-dir "$JOBS" \
      --job-name "$job" \
      -i "$task" \
      --yes < /dev/null > "$HERE/logs/$job.log" 2>&1
  ) || echo "harbor exit $? for $job"
  after="$("${COLLECT[@]}" backend)" || after="{\"error\": \"no answer\"}"
  "${COLLECT[@]}" record "$seq" "$round" "$task" "$arm" "$JOBS/$job" "$before" "$after" \
    || { echo "stop: rule met after seq $seq"; exit 4; }
done
