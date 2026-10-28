#!/usr/bin/env bash
# One line per trial of a Harbor job: tool calls so far, AOS exit, reward.
#   bench/harbor/progress.sh                  # latest job
#   bench/harbor/progress.sh pilot-strata-8   # a named job
#   watch -n 10 bench/harbor/progress.sh      # live view

set -euo pipefail
JOBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/reports/terminal-bench/jobs"
job="${1:-$(ls -t "$JOBS_DIR" | head -1)}"
cd "$JOBS_DIR/$job"
echo "job: $job"
for trial in */; do
  trial="${trial%/}"
  log="$trial/agent/aos.log"
  if [[ -f "$log" ]]; then
    content=$(cat "$log")
  else
    # Logs sync back when a trial ends; read a running trial from its container.
    container="$(echo "$trial" | tr '[:upper:]' '[:lower:]')__env-main-1"
    content=$(docker exec "$container" cat /logs/agent/aos.log 2>/dev/null || true)
  fi
  calls=$(grep -c "TOOL_CALL" <<<"$content" || true)
  exit_code=$(grep -o 'aos_exit=[0-9]*' <<<"$content" | tail -1 || true)
  reward=$(cat "$trial/verifier/reward.txt" 2>/dev/null || true)
  printf '%-28s calls=%-4s %-12s reward=%s\n' "${trial%%__*}" "${calls:-0}" "${exit_code:-running}" "${reward:--}"
done
