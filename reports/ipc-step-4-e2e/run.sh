#!/bin/zsh
# Usage: run.sh <aos binary> <new state dir> [port]. Run from the repository root.
aos=$1; state=$2; port=${3:-18934}
python3 -I reports/ipc-step-4-e2e/fake_model.py $port &
model=$!
sleep 1
AOS_LLM_BASE_URL=http://127.0.0.1:$port/v1 AOS_LLM_MODEL=scripted $aos run \
  --workflow reports/ipc-step-4-e2e/workflow.json --state-dir $state --output $state.out "run the scenario" > $state.log 2>&1
echo "aos run exited $?"
kill $model
python3 -I reports/ipc-step-4-e2e/check.py $state
