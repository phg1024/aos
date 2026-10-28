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
    if [[ -z "${AOS_LLM_API_KEY:-}" && -f "$HOME/.hermes/config.yaml" ]]; then
      AOS_LLM_API_KEY="$(awk '/^  strata-phg-gpu:/{f=1;next} f&&/api_key:/{print $2; exit}' "$HOME/.hermes/config.yaml" | tr -d "\"'")"
    fi
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-}"
    if [[ -z "$AOS_LLM_API_KEY" ]]; then
      echo "strata-phg: set AOS_LLM_API_KEY or add strata-phg-gpu to ~/.hermes/config.yaml" >&2
      exit 1
    fi
    strata_sampling_defaults
    ;;
  *)
    echo "unsupported backend: $BACKEND" >&2
    echo "usage: $0 [ollama|vllm|strata|strata-phg] [extra aos-tui args...]" >&2
    exit 1
    ;;
esac

echo "backend: $AOS_LLM_BACKEND"
echo "base_url: $AOS_LLM_BASE_URL"
echo "model: $AOS_LLM_MODEL"

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

cd "$ROOT_DIR"

exec go run ./cmd/aos-tui --workdir "$ROOT_DIR" ${args[@]+"${args[@]}"}
