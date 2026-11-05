#!/bin/zsh
# The scripted preflight. Usage: run.sh <aos binary> <run name> [port]. No real model, and no
# network beyond this machine: the fake endpoint drives the variant through the real aos run,
# whose run tool compiles and executes a small Metal kernel inside the OS sandbox.
aos=$1; name=$2; port=${3:-18940}
here=${0:A:h}; root=${here:h:h:h}
python3 -I $here/fake_model.py $port $root &
model=$!
sleep 1
AOS_LLM_BASE_URL=http://127.0.0.1:$port/v1 AOS_LLM_MODEL=scripted AOS_LLM_CONTEXT_WINDOW=100000 \
  $here/../run.sh $aos $name --time-budget 60m
kill $model
dir=$root/reports/ipc-live/runs/$name
python3 -I $here/metal.py $dir/state
python3 -I $here/../analyze.py $dir/state
