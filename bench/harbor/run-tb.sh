#!/usr/bin/env bash
# Run Terminal-Bench tasks with AOS through Harbor.
#
#   bench/harbor/run-tb.sh -i fix-git -i prove-plus-comm   # selected TB2 tasks
#   bench/harbor/run-tb.sh -l 10                           # first 10 tasks
#   TB_DATASET=terminal-bench/terminal-bench@4.0.0 bench/harbor/run-tb.sh -i music-harmony
#   bench/harbor/run-tb.sh --self-test                     # adapter unit tests
#
# Extra arguments go to `harbor run`. The model endpoint comes from AOS_LLM_*
# (defaults: the local Strata server; see FORWARDED_ENV in aos_agent.py).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HARBOR_DIR="$ROOT_DIR/bench/harbor"
export AOS_HARBOR_DIST="${AOS_HARBOR_DIST:-$ROOT_DIR/.cache/harbor-dist}"
export PYTHONPATH="$HARBOR_DIR${PYTHONPATH:+:$PYTHONPATH}"
HARBOR="${HARBOR:-$(command -v harbor || echo "$HOME/.local/bin/harbor")}"

if [[ "${1:-}" == "--self-test" ]]; then
  exec "$(dirname "$(readlink -f "$HARBOR")")/python" -m unittest discover -s "$HARBOR_DIR" -p 'test_*.py' -v
fi

# Static Linux builds for whichever architecture a task image uses.
export GOCACHE="${GOCACHE:-$ROOT_DIR/.cache/go-build}"
export GOTMPDIR="${GOTMPDIR:-$ROOT_DIR/.cache/tmp}"
mkdir -p "$GOCACHE" "$GOTMPDIR" "$AOS_HARBOR_DIST"
for arch in amd64 arm64; do
  (cd "$ROOT_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
    -o "$AOS_HARBOR_DIST/aos-linux-$arch" ./cmd/aos)
done

echo "model: ${AOS_LLM_MODEL:-qwen3.8-flash-next-unsloth-ud-q4_k_xl} at ${AOS_LLM_BASE_URL:-http://127.0.0.1:8080}"

# One trial at a time: a local inference server serves a single request slot.
exec "$HARBOR" run \
  --dataset "${TB_DATASET:-terminal-bench@2.0}" \
  --agent aos_agent:AOSAgent \
  --n-concurrent 1 \
  --allow-agent-host host.docker.internal \
  --jobs-dir "$ROOT_DIR/reports/terminal-bench/jobs" \
  --yes \
  "$@"
