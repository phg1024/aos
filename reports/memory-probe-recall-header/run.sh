#!/bin/zsh
# Runs the frozen A1 header re-test (PLAN.md): the new probes (9 answers
# per arm) and the 11 frozen probes (9 per arm), arm C (RecallOff) then
# arm A1 with each of the follow-up's three index draws. No handoffs: the
# indexes are reused from ../memory-probe-recall-fix/. Gates G1-G3 are
# checked before this script is started (PLAN.md); the backend-error stop
# rule is applied after each run.
# Run from the directory the live run started in (/Users/peihongguo/Code/aos):
#   zsh <worktree>/reports/memory-probe-recall-header/run.sh <worktree> <commit>
set -u
WT=$1
COMMIT=$2
D=$WT/reports/memory-probe-recall-header
INDEXES=$WT/reports/memory-probe-recall-fix
NEW=$D/probes-new.json
OLD=$WT/bench/memory/pathtracer-bunny-phase3-own-memory-probes.json
BIN=.cache/bin/aos-eval-$COMMIT
STATE=reports/pathtracer-bunny-phase3/.aos-state
WF=.cache/workflows/pathtracer-bunny-phase3.json
K=$(awk '/^  strata-phg-gpu:/{f=1;next} f&&/^  [a-z]/{exit} f&&/api_key:/{print $2; exit}' ~/.hermes/config.yaml | tr -d "\"'")
export AOS_LLM_BACKEND=openai-compatible AOS_LLM_BASE_URL=http://phg-gpu.local:8080 \
  AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4 AOS_LLM_API_KEY="$K" \
  AOS_LLM_CONTEXT_WINDOW=65536 AOS_COMPACT_RATIO=0.8 AOS_LLM_TIMEOUT_SECONDS=3600 \
  AOS_LLM_TEMPERATURE=0.6 AOS_LLM_TOP_P=0.95 AOS_LLM_MAX_TOKENS=32768 AOS_LLM_THINKING_BUDGET=16384
stamp() { date -u +"$1 %Y-%m-%dT%H:%M:%SZ $2, binary $COMMIT ($(shasum -a 256 $BIN | cut -c1-12)), new $(shasum -a 256 $NEW | cut -c1-12), frozen $(shasum -a 256 $OLD | cut -c1-12)" >> $D/run.log }
# Stop rule: more than 10% of a run's answers ending in an error.
errors_ok() {
  python3 -I -c '
import json, sys
s = [json.loads(l) for l in open(sys.argv[1])]
bad = sum(1 for x in s if x.get("error"))
print(f"{sys.argv[1].rsplit(\"/\", 1)[1]}: {len(s)} answers, {bad} with an error")
sys.exit(0 if len(s) and bad <= 0.1 * len(s) else 1)
' $1 >> $D/run.log 2>&1
}
probe() {  # name, probes file, samples, extra flags...
  local NAME=$1 FILE=$2 N=$3; shift 3
  stamp started "$NAME"
  $BIN probe-memory --workflow $WF --probes $FILE --samples $N --max-steps 12 "$@" \
    --work-dir $D/work-$NAME --out $D/samples-$NAME.jsonl $STATE >> $D/run.log 2>&1
  echo "exit $? $NAME" >> $D/run.log
  if ! errors_ok $D/samples-$NAME.jsonl; then
    date -u +"stopped %Y-%m-%dT%H:%M:%SZ: over 10%% of $NAME answers ended in an error" >> $D/run.log
    exit 1
  fi
}

probe C-new $NEW 9
probe C-frozen $OLD 9
for N in 1 2 3; do
  probe A1-d$N-new $NEW 3 --recall a1 --agent-indexes $INDEXES/indexes-d$N.json
  probe A1-d$N-frozen $OLD 3 --recall a1 --agent-indexes $INDEXES/indexes-d$N.json
done
date -u +"finished %Y-%m-%dT%H:%M:%SZ" >> $D/run.log
