#!/bin/zsh
# Runs the frozen A1 follow-up (PLAN.md): 27 handoffs that write the
# indexes, the stop rule (gate.py), then, if it passes, the 11 own-memory
# probes in arm C (RecallOff) and arm A1 (a1 on, with draw 1, 2 and 3's
# indexes, 3 samples each).
# Run from the directory the live run started in (/Users/peihongguo/Code/aos):
#   zsh <worktree>/reports/memory-probe-recall-fix/run.sh <worktree> <commit>
# Results go to <worktree>/reports/memory-probe-recall-fix/.
set -u
WT=$1
COMMIT=$2
D=$WT/reports/memory-probe-recall-fix
PROBES=$WT/bench/memory/pathtracer-bunny-phase3-own-memory-probes.json
BIN=.cache/bin/aos-eval-$COMMIT
STATE=reports/pathtracer-bunny-phase3/.aos-state
WF=.cache/workflows/pathtracer-bunny-phase3.json
CHECKPOINTS=art-000410,art-000503,art-000553,art-000642,art-001605,art-001657,art-001762,art-003059,art-003129
K=$(awk '/^  strata-phg-gpu:/{f=1;next} f&&/^  [a-z]/{exit} f&&/api_key:/{print $2; exit}' ~/.hermes/config.yaml | tr -d "\"'")
export AOS_LLM_BACKEND=openai-compatible AOS_LLM_BASE_URL=http://phg-gpu.local:8080 \
  AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4 AOS_LLM_API_KEY="$K" \
  AOS_LLM_CONTEXT_WINDOW=65536 AOS_COMPACT_RATIO=0.8 AOS_LLM_TIMEOUT_SECONDS=3600 \
  AOS_LLM_TEMPERATURE=0.6 AOS_LLM_TOP_P=0.95 AOS_LLM_MAX_TOKENS=32768 AOS_LLM_THINKING_BUDGET=16384
mkdir -p $D
stamp() { date -u +"$1 %Y-%m-%dT%H:%M:%SZ $2, binary $COMMIT ($(shasum -a 256 $BIN | cut -c1-12)), probes $(shasum -a 256 $PROBES | cut -c1-12)" >> $D/run.log }

# Step 1: handoffs, three draws per segment.
stamp started "handoffs"
$BIN probe-memory --handoff $CHECKPOINTS --recall a1 --samples 3 --max-steps 12 --workflow $WF \
  --work-dir $D/work-handoff --out $D/handoffs.jsonl $STATE >> $D/run.log 2>&1
echo "exit $? handoffs" >> $D/run.log
# One re-run of each failed draw, appended with its draw number.
python3 -I -c '
import json, sys
for line in open(sys.argv[1]):
    h = json.loads(line)
    if h.get("error"):
        print(h["checkpoint"], h["sample"])
' $D/handoffs.jsonl | while read CP N; do
  stamp started "handoff re-run $CP draw $N"
  $BIN probe-memory --handoff $CP --recall a1 --samples 1 --max-steps 12 --workflow $WF \
    --work-dir $D/work-handoff-$CP-d$N --out $D/retry.jsonl $STATE >> $D/run.log 2>&1
  echo "exit $? handoff re-run $CP draw $N" >> $D/run.log
  python3 -I -c '
import json, sys
with open(sys.argv[1], "a") as out:
    for line in open(sys.argv[2]):
        h = json.loads(line); h["sample"] = int(sys.argv[3]); h["retry"] = True
        out.write(json.dumps(h) + "\n")
' $D/handoffs.jsonl $D/retry.jsonl $N
  rm -f $D/retry.jsonl
done
(cd $D && python3 -I make_indexes.py) >> $D/run.log 2>&1

# Stop rule: the probe phase runs only if gate.py passes.
if (cd $D && python3 -I gate.py) > $D/gate.log 2>&1; then
  date -u +"gate %Y-%m-%dT%H:%M:%SZ: $(cat $D/gate.log)" >> $D/run.log
else
  date -u +"gate %Y-%m-%dT%H:%M:%SZ: $(cat $D/gate.log); stopped before the probe phase" >> $D/run.log
  exit 0
fi

# Step 2: probes. C with every aid off, then A1 with each draw's indexes.
stamp started "condition C"
$BIN probe-memory --workflow $WF --probes $PROBES --samples 9 --max-steps 12 \
  --work-dir $D/work-C --out $D/samples-C.jsonl $STATE >> $D/run.log 2>&1
echo "exit $? condition C" >> $D/run.log
for N in 1 2 3; do
  stamp started "condition A1 draw $N"
  $BIN probe-memory --workflow $WF --probes $PROBES --samples 3 --max-steps 12 \
    --recall a1 --agent-indexes $D/indexes-d$N.json \
    --work-dir $D/work-A1-d$N --out $D/samples-A1-d$N.jsonl $STATE >> $D/run.log 2>&1
  echo "exit $? condition A1 draw $N" >> $D/run.log
done
date -u +"finished %Y-%m-%dT%H:%M:%SZ" >> $D/run.log
