#!/usr/bin/env bash
# Three replay-smoke trials for aos code (ca78ad9), one at a time, through the relay.
set -uo pipefail
ROOT=/Users/peihongguo/Code/aos/.claude/worktrees/competent-heyrovsky-566e83
SRC=$ROOT/.cache/replay-ca78ad9
export AOS_HARBOR_DIST=$SRC/.cache/harbor-dist PYTHONPATH=$SRC/bench/harbor
export AOS_LLM_BACKEND=openai-compatible AOS_LLM_BASE_URL=http://127.0.0.1:18080 AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4 AOS_LLM_API_KEY=
unset AOS_TIME_BUDGET
HARBOR="$(command -v harbor || echo "$HOME/.local/bin/harbor")"
for spec in "fix-git strict" "vulnerable-secret strict" "cobol-modernization off"; do
  set -- $spec
  job="tb2-replay-$1-$2"
  echo "$(date -u +%FT%TZ) start $job status=$(curl -s -m 10 http://127.0.0.1:18080/v1/status | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["started"], d["activity"]["requests"])')"
  "$HARBOR" run --dataset terminal-bench@2.0 --agent aos_agent:AOSAgent --ak "time_budget=$2" \
    --n-concurrent 1 --allow-agent-host host.docker.internal --jobs-dir "$ROOT/reports/terminal-bench/jobs" \
    --job-name "$job" -i "$1" --yes < /dev/null > "$SRC/$job.log" 2>&1 || echo "harbor exit $? for $job"
  echo "$(date -u +%FT%TZ) end $job status=$(curl -s -m 10 http://127.0.0.1:18080/v1/status | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["started"], d["activity"]["requests"])') relay_posts=$(grep -c 'POST /v1/chat' $SRC/relay.log)"
done
