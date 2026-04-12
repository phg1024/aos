#!/bin/sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
export GOCACHE="$ROOT_DIR/.cache/go-build"
export GOTMPDIR="$ROOT_DIR/.cache/tmp"

mkdir -p "$GOCACHE" "$GOTMPDIR"
gofmt -w "$ROOT_DIR/cmd" "$ROOT_DIR/internal"
go test ./...
