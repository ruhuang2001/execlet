#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/files-e2e}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
cd "$ROOT"
PORT=${E2E_FILES_PORT:-$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')}
OWNER="files-e2e-$PORT-$$"
SERVER_PID=''
cleanup() {
 local code=$?
 trap - EXIT
 if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; fi
 docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER" >"$OUT/containers-before-cleanup.txt"
 while IFS= read -r id; do [ -z "$id" ] || docker rm -f "$id" >>"$OUT/cleanup.log" 2>&1 || true; done <"$OUT/containers-before-cleanup.txt"
 docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER" >"$OUT/containers-after-cleanup.txt"
 [ ! -s "$OUT/containers-after-cleanup.txt" ] || code=1
 printf '{"owner":"%s","exit_code":%s,"remaining":%s}\n' "$OWNER" "$code" "$(wc -l <"$OUT/containers-after-cleanup.txt" | tr -d ' ')" >"$OUT/cleanup.json"
 exit "$code"
}
trap cleanup EXIT
DOCKER_ENDPOINT=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}
if [[ "$DOCKER_ENDPOINT" != unix://* ]]; then echo "files-e2e requires a unix Docker endpoint" >&2; exit 1; fi
export DOCKER_HOST="$DOCKER_ENDPOINT"
export SANDBOX_DOMAIN="http://127.0.0.1:$PORT" SANDBOX_API_KEY="file-e2e-$OWNER" E2E_ARTIFACT_DIR="$OUT"
export PYTHONPATH="$ROOT/sandbox-python/src${PYTHONPATH:+:$PYTHONPATH}"
docker info >"$OUT/docker-info.txt" 2>&1
"${PYTHON:-python3}" scripts/file-e2e/prepare_image.py "$OUT"
BUILD_FLAGS=(-buildvcs=true)
if [ "${E2E_COVERAGE:-0}" = "1" ]; then
 COVER_DIR=${FILES_COVER_DIR:-$OUT/coverage}
 mkdir -p "$COVER_DIR"
 export GOCOVERDIR=$(cd "$COVER_DIR" && pwd)
 BUILD_FLAGS+=(-cover -covermode=atomic -coverpkg=./...)
fi
{
 go build "${BUILD_FLAGS[@]}" -o "$OUT/sandboxd" ./cmd/sandboxd
 go build "${BUILD_FLAGS[@]}" -o "$OUT/sandboxctl" ./cmd/sandboxctl
 go build "${BUILD_FLAGS[@]}" -o "$OUT/go-files" ./scripts/file-e2e
 go build "${BUILD_FLAGS[@]}" -o "$OUT/memory-files" ./scripts/file-e2e/memory
} >"$OUT/build.log" 2>&1
(cd sandbox-ts && npm ci && npm run build && npm run check) >"$OUT/typescript-build.log" 2>&1
"$OUT/sandboxd" -addr "127.0.0.1:$PORT" -owner "$OWNER" >"$OUT/sandboxd.log" 2>&1 &
SERVER_PID=$!
READY=0
for _ in $(seq 1 100); do
 if curl -fsS "$SANDBOX_DOMAIN/healthz" >"$OUT/healthz.json" 2>/dev/null; then READY=1; break; fi
 kill -0 "$SERVER_PID" 2>/dev/null || break
 sleep 0.1
done
if [ "$READY" -ne 1 ]; then cat "$OUT/sandboxd.log" >&2; exit 1; fi
"${PYTHON:-python3}" scripts/file-e2e/run.py 2>&1 | tee "$OUT/runner.log"
"$OUT/memory-files" "$OUT" >"$OUT/memory-results.json" 2>"$OUT/memory-server.log"
git rev-parse HEAD >"$OUT/source-revision.txt"
git show -m --format=fuller --binary HEAD >"$OUT/source.patch"
git diff >"$OUT/worktree.patch"
