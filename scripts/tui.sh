#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$ROOT_DIR/.cache/go-build}"
export GOTMPDIR="${GOTMPDIR:-$ROOT_DIR/.cache/tmp}"
# Skip VCS stamping so builds do not depend on which git is on PATH.
export GOFLAGS="${GOFLAGS:--buildvcs=false}"
mkdir -p "$GOCACHE" "$GOTMPDIR"

BACKEND="${AOS_LLM_BACKEND:-ollama}"
if [[ $# -gt 0 && "$1" != -* ]]; then
  BACKEND="$1"
  shift
fi

# hermes_api_key PROVIDER prints that provider's api_key from ~/.hermes/config.yaml.
hermes_api_key() {
  [[ -f "$HOME/.hermes/config.yaml" ]] || return 0
  awk -v section="  $1:" '$0 == section {f=1; next} f && /^  [a-z]/ {exit} f && /api_key:/ {print $2; exit}' \
    "$HOME/.hermes/config.yaml" | tr -d "\"'"
}

# Qwen thinking-mode sampling and a 256k window, shared by the Strata presets.
strata_sampling_defaults() {
  export AOS_LLM_TIMEOUT_SECONDS="${AOS_LLM_TIMEOUT_SECONDS:-3600}"
  export AOS_LLM_CONTEXT_WINDOW="${AOS_LLM_CONTEXT_WINDOW:-262144}"
  export AOS_LLM_TEMPERATURE="${AOS_LLM_TEMPERATURE:-0.6}"
  export AOS_LLM_TOP_P="${AOS_LLM_TOP_P:-0.95}"
  export AOS_LLM_MAX_TOKENS="${AOS_LLM_MAX_TOKENS:-32768}"
  export AOS_LLM_THINKING_BUDGET="${AOS_LLM_THINKING_BUDGET:-16384}"
}

case "$BACKEND" in
  ollama)
    export AOS_LLM_BACKEND="ollama"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://localhost:11434}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-qwen3.6:35b-a3b-mtp-q4_K_M}"
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-}"
    export AOS_LLM_TIMEOUT_SECONDS="${AOS_LLM_TIMEOUT_SECONDS:-600}"
    ;;
  vllm)
    export AOS_LLM_BACKEND="vllm"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://lenovo-px.local:8000}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-QuantTrio/Qwen3.5-35B-A3B-AWQ}"
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-dummy}"
    export AOS_LLM_TIMEOUT_SECONDS="${AOS_LLM_TIMEOUT_SECONDS:-600}"
    ;;
  strata)
    # Local Strata on this Mac (Metal); it runs without an API key.
    export AOS_LLM_BACKEND="openai-compatible"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://127.0.0.1:8080}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-qwen3.8-flash-next-unsloth-ud-q4_k_xl}"
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-}"
    strata_sampling_defaults
    ;;
  strata-phg)
    export AOS_LLM_BACKEND="openai-compatible"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://phg-gpu.local:8080}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-qwen3.8-flash-next-nvfp4-nvfp4}"
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-$(hermes_api_key strata-phg-gpu)}"
    if [[ -z "$AOS_LLM_API_KEY" ]]; then
      echo "strata-phg: set AOS_LLM_API_KEY or add strata-phg-gpu to ~/.hermes/config.yaml" >&2
      exit 1
    fi
    strata_sampling_defaults
    ;;
  strata-phg-vllm)
    # Strata on phg-vllm (Intel B70). It also serves an image model and swaps
    # models on demand, so the first request may wait for the chat model to load.
    export AOS_LLM_BACKEND="openai-compatible"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://phg-vllm.local:8081}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-qwen3.8-flash-next-iq3_s}"
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-$(hermes_api_key strata-phg-vllm)}"
    if [[ -z "$AOS_LLM_API_KEY" ]]; then
      echo "strata-phg-vllm: set AOS_LLM_API_KEY or add strata-phg-vllm to ~/.hermes/config.yaml" >&2
      exit 1
    fi
    strata_sampling_defaults
    ;;
  *)
    echo "unsupported backend: $BACKEND" >&2
    echo "usage: $0 [ollama|vllm|strata|strata-phg|strata-phg-vllm] [extra aos-tui args...]" >&2
    exit 1
    ;;
esac

# /model offers the profiles in AOS_MODELS_FILE: the user's own file when
# there is one, else the presets above. The backend launched here is the
# profile of the same name. Keys stay in named variables, read from
# ~/.hermes/config.yaml when not set; the TUI drops them from its
# environment once read, so the agent's commands never see them.
export AOS_MODEL_PROFILE="$BACKEND"
if [[ -z "${AOS_MODELS_FILE:-}" ]]; then
  user_models="${XDG_CONFIG_HOME:-$HOME/.config}/aos/models.json"
  if [[ -f "$user_models" ]]; then
    export AOS_MODELS_FILE="$user_models"
  else
    export AOS_MODELS_FILE="$ROOT_DIR/scripts/models.json"
  fi
fi
export_key() {
  local name="$1" provider="$2" value
  [[ -n "${!name:-}" ]] && return 0
  value="$(hermes_api_key "$provider")"
  [[ -n "$value" ]] && export "$name=$value"
  return 0
}
export_key AOS_KEY_STRATA_PHG strata-phg-gpu
export_key AOS_KEY_STRATA_PHG_VLLM strata-phg-vllm
[[ -n "${AOS_KEY_VLLM:-}" ]] || export AOS_KEY_VLLM=dummy

echo "backend: $AOS_LLM_BACKEND"
echo "base_url: $AOS_LLM_BASE_URL"
echo "model: $AOS_LLM_MODEL"
echo "model profiles: $AOS_MODELS_FILE"

# Checked-in workflows use absolute paths from the machine they were written
# on; run a copy rebased onto this checkout when they differ.
WORKFLOW_ORIGIN_ROOT="${AOS_WORKFLOW_ORIGIN_ROOT:-/Users/claw/Code/aos}"
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[$i]}" in
    --workflow | -workflow)
      (( i + 1 < ${#args[@]} )) || break
      i=$((i + 1))
      workflow="${args[$i]}"
      ;;
    --workflow=* | -workflow=*)
      workflow="${args[$i]#*=}"
      ;;
    *)
      continue
      ;;
  esac
  if [[ "$WORKFLOW_ORIGIN_ROOT" != "$ROOT_DIR" && -f "$workflow" ]] && grep -q "$WORKFLOW_ORIGIN_ROOT" "$workflow"; then
    rebased="$ROOT_DIR/.cache/workflows/$(basename "$workflow")"
    mkdir -p "$(dirname "$rebased")"
    sed "s#$WORKFLOW_ORIGIN_ROOT#$ROOT_DIR#g" "$workflow" > "$rebased"
    echo "workflow: $workflow (paths rebased to $ROOT_DIR)"
    if [[ "${args[$i]}" == *=* ]]; then
      args[$i]="--workflow=$rebased"
    else
      args[$i]="$rebased"
    fi
  fi
done

# Keep each session in a findable directory, so it can be inspected after
# the TUI exits (aos inspect <dir>). --state-dir in the arguments wins;
# AOS_TUI_SESSIONS_DIR moves where new sessions go.
state_dir_given=false
for arg in ${args[@]+"${args[@]}"}; do
  case "$arg" in
    --state-dir | -state-dir | --state-dir=* | -state-dir=*) state_dir_given=true ;;
  esac
done
if [[ "$state_dir_given" == false ]]; then
  sessions_dir="${AOS_TUI_SESSIONS_DIR:-$ROOT_DIR/.aos-sessions}"
  state_dir="$sessions_dir/$(date +%Y%m%d-%H%M%S)-$BACKEND"
  mkdir -p "$sessions_dir"
  args+=(--state-dir "$state_dir")
  echo "state_dir: $state_dir"
  echo "  inspect later: go run ./cmd/aos inspect $state_dir"
fi

cd "$ROOT_DIR"

exec go run ./cmd/aos-tui --workdir "$ROOT_DIR" ${args[@]+"${args[@]}"}
