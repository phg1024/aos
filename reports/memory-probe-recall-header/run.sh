#!/bin/zsh
# Runs the frozen A1 header re-test (PLAN.md): 27 handoffs on the frozen
# runtime that write the indexes (gate G1 failed for the follow-up's
# draws), gate G0 (gate.py), gates G2 and G3 (headercheck, coverage.py),
# then, if all pass, the new probes (9 answers per arm) and the 11 frozen
# probes (9 per arm): arm C (RecallOff), then arm A1 with each draw's
# indexes. The backend-error stop rule is applied after each probe run.
# Run from the directory the live run started in (/Users/peihongguo/Code/aos):
#   zsh <worktree>/reports/memory-probe-recall-header/run.sh <worktree> <commit>
# where <commit> is the plan commit the binaries were built from. The
# header's agent-index budget is pinned to 6144 bytes in every run.
set -u
WT=$1
COMMIT=$2
D=$WT/reports/memory-probe-recall-header
NEW=$D/probes-new.json
OLD=$WT/bench/memory/pathtracer-bunny-phase3-own-memory-probes.json
BIN=.cache/bin/aos-eval-$COMMIT
HEADERCHECK=.cache/bin/aos-headercheck-$COMMIT
STATE=reports/pathtracer-bunny-phase3/.aos-state
WF=.cache/workflows/pathtracer-bunny-phase3.json
BUDGET=6144
CHECKPOINTS=art-000410,art-000503,art-000553,art-000642,art-001605,art-001657,art-001762,art-003059,art-003129
K=$(awk '/^  strata-phg-gpu:/{f=1;next} f&&/^  [a-z]/{exit} f&&/api_key:/{print $2; exit}' ~/.hermes/config.yaml | tr -d "\"'")
export AOS_LLM_BACKEND=openai-compatible AOS_LLM_BASE_URL=http://phg-gpu.local:8080 \
  AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4 AOS_LLM_API_KEY="$K" \
  AOS_LLM_CONTEXT_WINDOW=65536 AOS_COMPACT_RATIO=0.8 AOS_LLM_TIMEOUT_SECONDS=3600 \
  AOS_LLM_TEMPERATURE=0.6 AOS_LLM_TOP_P=0.95 AOS_LLM_MAX_TOKENS=32768 AOS_LLM_THINKING_BUDGET=16384
stamp() { date -u +"$1 %Y-%m-%dT%H:%M:%SZ $2, binary $COMMIT ($(shasum -a 256 $BIN | cut -c1-12)), new $(shasum -a 256 $NEW | cut -c1-12), frozen $(shasum -a 256 $OLD | cut -c1-12)" >> $D/run.log }
stopped() { date -u +"stopped %Y-%m-%dT%H:%M:%SZ: $1" >> $D/run.log; exit 1 }

# Step 1: handoffs, three draws per segment, as in the follow-up.
stamp started "handoffs"
$BIN probe-memory --handoff $CHECKPOINTS --recall a1 --header-index-bytes $BUDGET --samples 3 --max-steps 12 --workflow $WF \
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
  $BIN probe-memory --handoff $CP --recall a1 --header-index-bytes $BUDGET --samples 1 --max-steps 12 --workflow $WF \
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
python3 -I $D/handoff_summary.py >> $D/run.log 2>&1

# Gates before any probe: G0 (enough indexes), G2 and G3 (the header).
if (cd $D && python3 -I gate.py) > $D/gate.log 2>&1; then
  date -u +"G0 %Y-%m-%dT%H:%M:%SZ: $(cat $D/gate.log)" >> $D/run.log
else
  stopped "G0 $(cat $D/gate.log); before the probe phase"
fi
$HEADERCHECK -budget $BUDGET $STATE $D/indexes-d1.json $D/indexes-d2.json $D/indexes-d3.json > $D/header.json 2>> $D/run.log \
  || stopped "headercheck failed; before the probe phase"
if (cd $D && python3 -I coverage.py . header.json) > $D/coverage.log 2>&1; then
  date -u +"gates %Y-%m-%dT%H:%M:%SZ: $(grep -E '^G[23]' $D/coverage.log | tr '\n' ' ')" >> $D/run.log
else
  stopped "$(grep -E '^G[23]' $D/coverage.log | tr '\n' ' '); before the probe phase"
fi

# Step 2: probes. Stop rule: more than 10% of a run's answers ending in an error.
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
  $BIN probe-memory --workflow $WF --probes $FILE --samples $N --max-steps 12 --header-index-bytes $BUDGET "$@" \
    --work-dir $D/work-$NAME --out $D/samples-$NAME.jsonl $STATE >> $D/run.log 2>&1
  echo "exit $? $NAME" >> $D/run.log
  errors_ok $D/samples-$NAME.jsonl || stopped "over 10% of $NAME answers ended in an error"
}

probe C-new $NEW 9
probe C-frozen $OLD 9
for N in 1 2 3; do
  probe A1-d$N-new $NEW 3 --recall a1 --agent-indexes $D/indexes-d$N.json
  probe A1-d$N-frozen $OLD 3 --recall a1 --agent-indexes $D/indexes-d$N.json
done
date -u +"finished %Y-%m-%dT%H:%M:%SZ" >> $D/run.log
