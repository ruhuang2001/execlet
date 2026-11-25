#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PYTHON=${PYTHON:-python3}
python_exe=$("$PYTHON" -c 'import sys; print(sys.executable)')

create_env() {
  if command -v uv >/dev/null; then
    uv venv --python "$python_exe" --allow-existing "$1"
  else
    "$python_exe" -m venv "$1"
  fi
}

install() {
  local interpreter=$1
  shift
  if command -v uv >/dev/null; then
    uv pip install --python "$interpreter" "$@"
  else
    "$interpreter" -m pip install "$@"
  fi
}

case "$1" in
  requirements)
    create_env "$2"
    install "$2/bin/python" -r "$3"
    ;;
  install-sdk)
    create_env "$2"
    mkdir -p "$3"
    if command -v uv >/dev/null; then
      uv build --python "$2/bin/python" --wheel --out-dir "$3" "$ROOT/sandbox-python"
    else
      "$2/bin/python" -m pip wheel --no-deps --wheel-dir "$3" "$ROOT/sandbox-python"
    fi
    "$2/bin/python" "$ROOT/scripts/check-sdk-wheel.py" "$3"/execlet-*.whl
    install "$2/bin/python" --force-reinstall --no-deps "$3"/execlet-*.whl
    ;;
  dist)
    if command -v uv >/dev/null; then
      uv build --python "$python_exe" --out-dir "$2" "$ROOT/sandbox-python"
    else
      "$python_exe" -m build --outdir "$2" "$ROOT/sandbox-python"
    fi
    ;;
  *) exit 2 ;;
esac
