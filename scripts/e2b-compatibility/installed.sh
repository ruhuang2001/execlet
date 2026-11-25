#!/usr/bin/env bash
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
OUT=${1:?fresh evidence directory (keep short for Unix sockets)}
IMAGE=${2:?installed compatibility image digest}
shift 2
PYTHON=${PYTHON:-python3}
PROFILE=baseline
previous=
for argument in "$@"; do
  if [ "$previous" = --profile ]; then PROFILE=$argument; fi
  case "$argument" in --profile=*) PROFILE=${argument#--profile=} ;; esac
  previous=$argument
done
if [ -e "$OUT" ]; then echo "evidence directory already exists: $OUT" >&2; exit 1; fi
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
unset PYTHONPATH CONTRACT_MODULE CONTRACT_TS_MODULE CONTRACT_TS_WORKER

"$HERE/node_modules/.bin/tsc" -p "$HERE/tsconfig.json"
mkdir -p "$OUT/wheels" "$OUT/npm" "$OUT/consumer"
"$PYTHON" -m venv "$OUT/venv"
"$OUT/venv/bin/python" -m pip wheel --no-deps --wheel-dir "$OUT/wheels" "$ROOT/sandbox-python" >"$OUT/install-python.log" 2>&1
"$OUT/venv/bin/python" "$ROOT/scripts/check-sdk-wheel.py" "$OUT"/wheels/execlet-*.whl >>"$OUT/install-python.log" 2>&1
"$OUT/venv/bin/python" -m pip install --no-deps "$OUT"/wheels/execlet-*.whl >>"$OUT/install-python.log" 2>&1
(cd "$ROOT/sandbox-ts" && npm pack --pack-destination "$OUT/npm" && npm run check) >"$OUT/install-typescript.log" 2>&1
npm install --prefix "$OUT/consumer" --ignore-scripts --no-audit --no-fund "$OUT"/npm/execlet-sdk-*.tgz >>"$OUT/install-typescript.log" 2>&1
sed "s|'../src/e2b.js'|'@execlet/sdk/e2b'|" "$ROOT/sandbox-ts/checks/e2b-api.ts" >"$OUT/consumer/contract.mts"
"$ROOT/sandbox-ts/node_modules/.bin/tsc" --noEmit --strict --exactOptionalPropertyTypes \
  --module NodeNext --moduleResolution NodeNext --target ES2022 --lib ES2022,DOM,DOM.Iterable \
  "$OUT/consumer/contract.mts" >"$OUT/installed-types.log" 2>&1
cp "$HERE/dist/suite.js" "$OUT/consumer/suite.mjs"
export CONTRACT_TS_WORKER="$OUT/consumer/suite.mjs"
if [ "$PROFILE" = background ]; then
  "$OUT/venv/bin/python" -B -m unittest discover -s "$ROOT/sandbox-python/tests" -v >"$OUT/installed-python-tests.log" 2>&1
  cp "$ROOT"/sandbox-ts/tests/*.test.mjs "$OUT/consumer/"
  (cd "$OUT/consumer" && node --test ./*.test.mjs) >"$OUT/installed-typescript-tests.log" 2>&1
  sed "s|from 'e2b'|from '@execlet/sdk/e2b'|" "$HERE/background-types.ts" >"$OUT/consumer/background.mts"
  type_status=0
  "$ROOT/sandbox-ts/node_modules/.bin/tsc" --noEmit --strict --exactOptionalPropertyTypes \
    --module NodeNext --moduleResolution NodeNext --target ES2022 --lib ES2022,DOM,DOM.Iterable \
    "$OUT/consumer/background.mts" >"$OUT/background-types.log" 2>&1 || type_status=$?
  printf '{"returncode":%s,"log":"background-types.log"}\n' "$type_status" >"$OUT/background-types.json"
  export CONTRACT_BACKGROUND_TYPES="$OUT/background-types.json"
fi
"$OUT/venv/bin/python" -c 'import importlib.util, execlet.e2b; assert importlib.util.find_spec("e2b") is None; print(execlet.e2b.__file__)' >"$OUT/python-import.txt"
(cd "$OUT/consumer" && node --input-type=module -e '
  import { Sandbox } from "@execlet/sdk/e2b";
  try { import.meta.resolve("e2b"); throw new Error("E2B runtime must be absent"); }
  catch (e) { if (e.code !== "ERR_MODULE_NOT_FOUND") throw e; }
  if (typeof Sandbox.create !== "function") throw new Error("missing adapter");
  console.log(import.meta.resolve("@execlet/sdk/e2b"));
') >"$OUT/typescript-import.txt"
docker image inspect "$IMAGE" >"$OUT/image.json"
docker run --rm --network none "$IMAGE" /bin/bash -l -c 'id; pwd; printf "HOME=%s\n" "$HOME"; bash --version; python3 --version; dpkg-query -W' >"$OUT/guest.txt"
run_status=0
"$OUT/venv/bin/python" "$HERE/runner.py" run --provider tiny --out "$OUT/run" \
  --python "$OUT/venv/bin/python" --image "$IMAGE" "$@" || run_status=$?
compare_status=0
"$OUT/venv/bin/python" "$HERE/runner.py" compare "$OUT/run/report.json" --profile "$PROFILE" \
  | tee "$OUT/compare.log" || compare_status=$?
if [ "$run_status" -ne 0 ]; then exit "$run_status"; fi
exit "$compare_status"
