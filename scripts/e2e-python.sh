#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/python-e2e}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
PYTHON=${PYTHON:-python3}
command -v "$PYTHON" >/dev/null
docker info >"$OUT/docker-info.txt" 2>&1
(cd "$ROOT" && go build -o "$OUT/execletd" ./cmd/execletd) >"$OUT/build.log" 2>&1
if [ "${SDK_INSTALLED:-0}" != "1" ]; then
  rm -f "$OUT"/wheels/execlet-*.whl
  PYTHON="$PYTHON" bash "$ROOT/scripts/python-tools.sh" install-sdk "$OUT/venv" "$OUT/wheels" >"$OUT/install.log" 2>&1
  PYTHON="$OUT/venv/bin/python"
fi
"$PYTHON" -u "$ROOT/sandbox-python/e2e/run.py" --server "$OUT/execletd" --artifacts "$OUT"
