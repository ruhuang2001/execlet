#!/usr/bin/env bash
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
PYTHON=${PYTHON:-python3}
"$PYTHON" -c 'import sys; assert sys.version_info >= (3,11), "Python 3.11+ required"'
node -e 'const [major,minor]=process.versions.node.split(".").map(Number); if (major<22 || (major===22 && minor<19)) { console.error("Node >=22.19 required by pinned E2B dependencies"); process.exit(1) }'
"$PYTHON" -m venv "$HERE/.venv"
"$HERE/.venv/bin/python" -m pip install -r "$HERE/requirements.txt"
npm --prefix "$HERE" ci
"$HERE/node_modules/.bin/tsc" -p "$HERE/tsconfig.json"
echo 'Reference SDKs installed; no acceptance cases or cloud resources run.'
