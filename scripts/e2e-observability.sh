#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/artifacts/observability-e2e
if [ "$#" -gt 0 ]; then OUT=$1; fi
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
cd "$ROOT"
go build -o "$OUT/execletd" ./cmd/execletd
python3 scripts/e2e-observability.py "$OUT"
