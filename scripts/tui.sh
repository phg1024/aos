#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$ROOT_DIR/.cache/go-build}"
export GOTMPDIR="${GOTMPDIR:-$ROOT_DIR/.cache/tmp}"
mkdir -p "$GOCACHE" "$GOTMPDIR"

BACKEND="${AOS_LLM_BACKEND:-ollama}"
if [[ $# -gt 0 && "$1" != -* ]]; then
  BACKEND="$1"
  shift
fi

case "$BACKEND" in
  ollama)
    export AOS_LLM_BACKEND="ollama"
    export AOS_LLM_BASE_URL="${AOS_LLM_BASE_URL:-http://lenovo-px.local:11434}"
    export AOS_LLM_MODEL="${AOS_LLM_MODEL:-gemma4:31b}"
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
  *)
    echo "unsupported backend: $BACKEND" >&2
    echo "usage: $0 [ollama|vllm] [extra aos-tui args...]" >&2
    exit 1
    ;;
esac

echo "backend: $AOS_LLM_BACKEND"
echo "base_url: $AOS_LLM_BASE_URL"
echo "model: $AOS_LLM_MODEL"

cd "$ROOT_DIR"

exec go run ./cmd/aos-tui --workdir "$ROOT_DIR" "$@"
