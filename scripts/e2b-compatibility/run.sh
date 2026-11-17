#!/usr/bin/env bash
# One acceptance run. Cloud needs --allow-cloud and E2B_API_KEY.
#
#   bash scripts/e2b-compatibility/run.sh tiny artifacts/e2b-tiny \
#     --image sha256:<digest>
#   E2B_API_KEY=... bash scripts/e2b-compatibility/run.sh e2b artifacts/e2b-ref \
#     --allow-cloud --languages python
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
