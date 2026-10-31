#!/bin/zsh
# Applies gate.py to the running run.sh (PLAN.md, "Stop rule"): waits for
# the handoff phase's indexes.log, then on FAIL stops run.sh and any probe
# process it started, and notes it in run.log.
#   zsh gate_watch.sh <worktree> <run.sh pid>
set -u
D=$1/reports/memory-probe-recall
RUN=$2
until [ -f $D/indexes.log ]; do
  kill -0 $RUN 2>/dev/null || exit 0
  sleep 2
done
sleep 1
if (cd $D && python3 -I gate.py) > $D/gate.log 2>&1; then
  date -u +"gate %Y-%m-%dT%H:%M:%SZ: $(cat $D/gate.log)" >> $D/run.log
else
  kill $RUN 2>/dev/null
  pkill -f "aos-eval-d66bd30 probe-memory --workflow" 2>/dev/null
  date -u +"gate %Y-%m-%dT%H:%M:%SZ: $(cat $D/gate.log); stopped run.sh before the probe phase" >> $D/run.log
fi
