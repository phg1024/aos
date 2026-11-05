#!/bin/zsh
# Submits the one job of a window. Usage: submit.sh <submission id> <run name> <preflight|live> [seconds]
# Everything about the cluster comes from the environment, and it refuses without entrypoint
# resources, since a driver lands on the head otherwise. No key goes in the entrypoint or metadata.
set -e
id=${1:?submission id}; name=${2:?run name}; mode=${3:?preflight or live}; limit=${4:-5400}
: ${AOS_RAY:?the ray command}; : ${AOS_RAY_ADDRESS:?}; : ${AOS_RAY_RESOURCES:?entrypoint resources are required, or the driver lands on the head}
: ${AOS_JOB_HOME:?the home directory on the machine}; : ${AOS_GPU_NAME:?}
cpus=${AOS_JOB_CPUS:-12}
if (( cpus > 12 )); then echo "at most 12 CPUs" >&2; exit 2; fi
exec $AOS_RAY job submit --address $AOS_RAY_ADDRESS --submission-id $id --no-wait \
  --metadata-json "{\"name\":\"$name\",\"owner\":\"aos\"}" \
  --entrypoint-num-cpus $cpus --entrypoint-resources $AOS_RAY_RESOURCES \
  -- env AOS_GPU_NAME="$AOS_GPU_NAME" AOS_JOB_CPUS=$cpus AOS_LLM_BASE_URL=${AOS_LLM_BASE_URL:-} AOS_LLM_MODEL=${AOS_LLM_MODEL:-} \
  AOS_LLM_CONTEXT_WINDOW=${AOS_LLM_CONTEXT_WINDOW:-} AOS_LLM_MAX_TOKENS=${AOS_LLM_MAX_TOKENS:-12000} \
  $AOS_JOB_HOME/aos-ipc-live/bin/job.sh $name $mode $limit
