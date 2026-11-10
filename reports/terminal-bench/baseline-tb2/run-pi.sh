#!/usr/bin/env bash
# Runs the pi arm's trials (PLAN-pi.md, Option A) in schedule-pi.json order,
# one Harbor job each, against the baseline's Strata through relay.py (which
# must be running on 127.0.0.1:18080 with --log relay-pi.log; see
# relay-start-pi.sh). Around each trial it checks the backend and the daily
# window, saves the backend strings, and collect_pi.py appends the record and
# applies the stop rule. Recorded trials are skipped, so the script can be
# restarted after a pause. Nothing is ever rerun.
#
#   reports/terminal-bench/baseline-tb2/run-pi.sh            # the 48 trials
#   reports/terminal-bench/baseline-tb2/run-pi.sh --smoke    # the smoke trial
#
# The pi release must be in .cache/pi-v1.5.0 (PLAN-pi.md, "Build and files");
# the adapter checks its hash on every install.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
HARBOR="${HARBOR:-$(command -v harbor || echo "$HOME/.local/bin/harbor")}"
JOBS="$ROOT/reports/terminal-bench/jobs"
COLLECT=(python3 -I "$HERE/collect_pi.py")
mkdir -p "$HERE/logs"
# No key reaches Harbor, the agent or the container: the relay adds it.
unset ANTHROPIC_API_KEY OPENAI_API_KEY AOS_LLM_API_KEY HOSTED_VLLM_API_KEY AOS_TIME_BUDGET

trials() {
  if [[ "${1:-}" == "--smoke" ]]; then
    task=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["smoke_task"])' "$HERE/drawn.json")
    echo "0 0 $task pi tb2-pi-smoke-$task-pi"
  else
    python3 -I -c 'import json,sys
for t in json.load(open(sys.argv[1]))["trials"]:
    print(t["seq"], t["round"], t["task"], t["arm"], t["job"])' "$HERE/schedule-pi.json"
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
  (
    export PYTHONPATH="$ROOT/bench/harbor"
    export AOS_LLM_BASE_URL=http://127.0.0.1:18080 AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4
    export AOS_LLM_CONTEXT_WINDOW=262144 AOS_LLM_TEMPERATURE=0.6 AOS_LLM_TOP_P=0.95
    export AOS_LLM_MAX_TOKENS=8192 AOS_LLM_THINKING_BUDGET=2048
    export PI_HARBOR_CACHE="$ROOT/.cache/pi-v1.5.0"
    "$HARBOR" run \
      --dataset terminal-bench@2.0 \
      --agent pi_agent:PiAgent \
      --n-concurrent 1 \
      --allow-agent-host host.docker.internal \
      --jobs-dir "$JOBS" \
      --job-name "$job" \
      -i "$task" \
      --yes < /dev/null > "$HERE/logs/$job.log" 2>&1
  ) || echo "harbor exit $? for $job"
  after="$("${COLLECT[@]}" backend)" || after="{\"error\": \"no answer\"}"
  printf '{"seq": %s, "before": %s, "after": %s}\n' "$seq" "$before" "$after" >> "$HERE/logs/backend-pi.jsonl"
  "${COLLECT[@]}" record "$seq" "$round" "$task" "$JOBS/$job" "$before" "$after" \
    || { echo "stop: rule met after seq $seq"; exit 4; }
done
