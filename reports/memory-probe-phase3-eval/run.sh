#!/bin/zsh
# Runs the frozen phase-3 evaluation probes (commit 4b2514c) on the 74510b7
# binary, 9 samples per probe, in three conditions one after another.
# Run from the directory the memfix run started in (/Users/peihongguo/Code/aos):
#   zsh <worktree>/reports/memory-probe-phase3-eval/run.sh <worktree>
# Results go to <worktree>/reports/memory-probe-phase3-eval/.
set -u
WT=$1
D=$WT/reports/memory-probe-phase3-eval
PROBES=$WT/bench/memory/pathtracer-bunny-memfix-phase3-eval-probes.json
BIN=.cache/bin/aos-eval-74510b7
K=$(awk '/^  strata-phg-gpu:/{f=1;next} f&&/^  [a-z]/{exit} f&&/api_key:/{print $2; exit}' ~/.hermes/config.yaml | tr -d "\"'")
mkdir -p $D
for C in memory no-check-rule no-memory; do
  case $C in
    memory) F=() ;;
    no-check-rule) F=(--no-check-rule) ;;
    no-memory) F=(--no-working-memory) ;;
  esac
  date -u +"started %Y-%m-%dT%H:%M:%SZ condition $C, binary 74510b7 ($(shasum -a 256 $BIN | cut -c1-12)), probes $(shasum -a 256 $PROBES | cut -c1-12)" >> $D/run.log
  AOS_LLM_BACKEND=openai-compatible AOS_LLM_BASE_URL=http://phg-gpu.local:8080 \
  AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4 AOS_LLM_API_KEY="$K" \
  AOS_LLM_CONTEXT_WINDOW=65536 AOS_COMPACT_RATIO=0.8 AOS_LLM_TIMEOUT_SECONDS=3600 \
  AOS_LLM_TEMPERATURE=0.6 AOS_LLM_TOP_P=0.95 AOS_LLM_MAX_TOKENS=32768 AOS_LLM_THINKING_BUDGET=16384 \
  $BIN probe-memory "${F[@]}" --workflow .cache/workflows/pathtracer-bunny-memfix.json \
    --probes $PROBES --samples 9 --max-steps 12 \
    --work-dir $D/work-$C --out $D/samples-$C.jsonl \
    reports/pathtracer-bunny-memfix/.aos-state >> $D/run.log 2>&1
  echo "exit $? condition $C" >> $D/run.log
done
date -u +"finished %Y-%m-%dT%H:%M:%SZ" >> $D/run.log
