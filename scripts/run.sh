#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$ROOT_DIR/.cache/go-build}"
export GOTMPDIR="${GOTMPDIR:-$ROOT_DIR/.cache/tmp}"
export AOS_TRACE="${AOS_TRACE:-1}"
mkdir -p "$GOCACHE" "$GOTMPDIR"

BACKEND="${AOS_LLM_BACKEND:-${1:-ollama}}"

case "$BACKEND" in
  ollama)
    export AOS_LLM_BACKEND="ollama"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://lenovo-px.local:11434}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-gemma4:26b}"
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-}"
    export AOS_LLM_TIMEOUT_SECONDS="${AOS_LLM_TIMEOUT_SECONDS:-180}"
    ;;
  vllm)
    export AOS_LLM_BACKEND="vllm"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://lenovo-px.local:8001}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-Qwen/Qwen3.5-27B}"
    export AOS_LLM_API_KEY="${AOS_LLM_API_KEY:-dummy}"
    export AOS_LLM_TIMEOUT_SECONDS="${AOS_LLM_TIMEOUT_SECONDS:-180}"
    ;;
  *)
    echo "unsupported backend: $BACKEND" >&2
    echo "usage: $0 [ollama|vllm] [extra aos args...]" >&2
    exit 1
    ;;
esac

if [[ $# -gt 0 && "$1" == "$BACKEND" ]]; then
  shift
fi

echo "backend: $AOS_LLM_BACKEND"
echo "base_url: $AOS_LLM_BASE_URL"
echo "model: $AOS_LLM_MODEL"

cd "$ROOT_DIR"

exec go run ./cmd/aos run \
  --trace \
  --workflow ./workflows/rasterizer-optimized-tail.json \
  --output ./reports/rasterizer-team-demo/final-summary.txt \
  "$@"
