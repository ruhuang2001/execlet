#!/usr/bin/env bash
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
PROVIDER=${1:?provider: e2b or tiny}
OUT=${2:?fresh evidence directory}
shift 2
PYTHON=${PYTHON:-$HERE/.venv/bin/python}
if [ ! -x "$PYTHON" ]; then
  echo "run setup.sh first, or set PYTHON to an interpreter with e2b==2.64.0" >&2
  exit 1
fi
exec "$PYTHON" "$HERE/runner.py" run --provider "$PROVIDER" --out "$OUT" "$@"
