#!/bin/zsh
# Runs the frozen guidance test (PLAN.md): gates G1-G3 without a model
# call, then both arms, A1 (guidance off) and A1+G (guidance on), as two
# concurrent sequences over the same probe subsets (probes-d<draw>-n<n>.json,
# from cells.py), each with its draw's indexes from the header re-test
# (d9e653e). The backend-error stop rule is applied after each run.
# Run from the directory the live run started in (/Users/peihongguo/Code/aos):
#   zsh <worktree>/reports/memory-probe-recall-guidance/run.sh <worktree> <commit>
# where <commit> is the plan commit the binaries were built from:
# .cache/bin/aos-eval-<commit> and .cache/bin/aos-headercheck-<commit>.
set -u
WT=$1
COMMIT=$2
D=$WT/reports/memory-probe-recall-guidance
BIN=.cache/bin/aos-eval-$COMMIT
HEADERCHECK=.cache/bin/aos-headercheck-$COMMIT
STATE=reports/pathtracer-bunny-phase3/.aos-state
WF=.cache/workflows/pathtracer-bunny-phase3.json
BUDGET=6144
# The guidance switch (283a92e): recall aid a1p, the partial-index note.
RECALL_OFF=a1
RECALL_ON=a1,a1p
K=$(awk '/^  strata-phg-gpu:/{f=1;next} f&&/^  [a-z]/{exit} f&&/api_key:/{print $2; exit}' ~/.hermes/config.yaml | tr -d "\"'")
export AOS_LLM_BACKEND=openai-compatible AOS_LLM_BASE_URL=http://phg-gpu.local:8080 \
  AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4 AOS_LLM_API_KEY="$K" \
  AOS_LLM_CONTEXT_WINDOW=65536 AOS_COMPACT_RATIO=0.8 AOS_LLM_TIMEOUT_SECONDS=3600 \
  AOS_LLM_TEMPERATURE=0.6 AOS_LLM_TOP_P=0.95 AOS_LLM_MAX_TOKENS=32768 AOS_LLM_THINKING_BUDGET=16384
stamp() { date -u +"$1 %Y-%m-%dT%H:%M:%SZ $2, binary $COMMIT ($(shasum -a 256 $BIN | cut -c1-12))" >> $D/run.log }
stopped() { date -u +"stopped %Y-%m-%dT%H:%M:%SZ: $1" >> $D/run.log; exit 1 }

# Gates, no model call. G1: guidance-off headers byte-identical to
# 6c55bdb's (reference-6c55bdb/), and the same lines shown and hidden per
# draw and checkpoint. G2: guidance on shows the same lines, and its header
# differs from the guidance-off one only by the guidance text (gate.py).
IDX=($D/indexes-d1.json $D/indexes-d2.json $D/indexes-d3.json)
mkdir -p $D/headers-off $D/headers-on
$HEADERCHECK -budget $BUDGET -recall $RECALL_OFF -dump $D/headers-off $STATE $IDX > $D/headers-off/header.json 2>> $D/run.log \
  || stopped "headercheck (guidance off) failed"
$HEADERCHECK -budget $BUDGET -recall $RECALL_ON -dump $D/headers-on $STATE $IDX > $D/headers-on/header.json 2>> $D/run.log \
  || stopped "headercheck (guidance on) failed"
if (cd $D && python3 -I gate.py) > $D/gate.log 2>&1; then
  date -u +"gates %Y-%m-%dT%H:%M:%SZ: $(head -1 $D/gate.log)" >> $D/run.log
else
  stopped "$(cat $D/gate.log); before any model call"
fi

# Stop rule: more than 10% of a run's answers ending in an error.
errors_ok() {
  python3 -I -c '
import json, sys
s = [json.loads(l) for l in open(sys.argv[1])]
bad = sum(1 for x in s if x.get("error"))
name = sys.argv[1].rsplit("/", 1)[1]
print(f"{name}: {len(s)} answers, {bad} with an error")
sys.exit(0 if len(s) and bad <= 0.1 * len(s) else 1)
' $1 >> $D/run.log 2>&1
}
arm() {  # name, recall aids
  local ARM=$1 RECALL=$2
  local START=$SECONDS
  date -u +"started $ARM %Y-%m-%dT%H:%M:%SZ" >> $D/run.log
  for F in $D/probes-d*-n*.json; do
    local B=${F:t:r}                      # probes-d2-n6
    local DRAW=${${B#probes-d}%%-*} N=${B##*-n}
    local NAME=$ARM-d$DRAW-n$N
    stamp started "$NAME"
    $BIN probe-memory --workflow $WF --probes $F --samples $N --max-steps 12 --header-index-bytes $BUDGET \
      --recall $RECALL --agent-indexes $D/indexes-d$DRAW.json \
      --work-dir $D/work-$NAME --out $D/samples-$NAME.jsonl $STATE >> $D/run-$ARM.log 2>&1
    echo "exit $? $NAME" >> $D/run.log
    errors_ok $D/samples-$NAME.jsonl || { touch $D/STOP; stopped "over 10%% of $NAME answers ended in an error, or none were written"; }
    [[ -e $D/STOP ]] && stopped "$ARM: the other arm stopped"
  done
  date -u +"finished $ARM %Y-%m-%dT%H:%M:%SZ, wall clock $(( SECONDS - START ))s" >> $D/run.log
}

rm -f $D/STOP
arm A1 $RECALL_OFF &
arm A1G $RECALL_ON &
wait
date -u +"finished %Y-%m-%dT%H:%M:%SZ" >> $D/run.log
