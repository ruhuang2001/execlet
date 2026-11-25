#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/stdin-e2e}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
unset SANDBOX_API_KEY || true
export DOCKER_HOST=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}
if [[ "$DOCKER_HOST" != unix://* ]]; then echo 'stdin-e2e requires a Unix Docker endpoint' >&2; exit 1; fi
(cd "$ROOT" && go build -o "$OUT/execletd" ./cmd/execletd && go build -o "$OUT/execletctl" ./cmd/execletctl)
PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
OWNER=127.0.0.1:$PORT
export SANDBOX_DOMAIN=http://$OWNER
"$OUT/execletd" -addr "$OWNER" >"$OUT/execletd.log" 2>&1 &
PID=$!
cleanup() {
 local process_result=$?
 local result=$process_result
 trap - EXIT
 kill "$PID" 2>/dev/null || true
 wait "$PID" 2>/dev/null || true
 if ! docker ps -aq --filter "label=execlet.owner=$OWNER" >"$OUT/containers.txt"; then result=1; fi
 if [ -s "$OUT/containers.txt" ]; then result=1; fi
 : >"$OUT/cleanup-errors.txt"
 while IFS= read -r id; do
   if [ -n "$id" ] && ! docker rm -f "$id" >>"$OUT/cleanup.log" 2>>"$OUT/cleanup-errors.txt"; then result=1; fi
 done <"$OUT/containers.txt"
 if ! docker ps -aq --filter "label=execlet.owner=$OWNER" >"$OUT/containers-after-cleanup.txt"; then result=1; fi
 if [ -s "$OUT/containers-after-cleanup.txt" ]; then result=1; fi
 printf '{"exit_code":%s,"process_exit_code":%s,"leaked_before_cleanup":%s,"remaining_containers":%s}\n' "$result" "$process_result" "$(wc -l <"$OUT/containers.txt" | tr -d ' ')" "$(wc -l <"$OUT/containers-after-cleanup.txt" | tr -d ' ')" >"$OUT/result.json"
 exit "$result"
}
trap cleanup EXIT
for _ in $(seq 1 100); do
 if curl -fsS "$SANDBOX_DOMAIN/healthz" >"$OUT/healthz.json" 2>/dev/null; then break; fi
 kill -0 "$PID" 2>/dev/null || { cat "$OUT/execletd.log" >&2; exit 1; }
 sleep .1
done
curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null
(cd "$ROOT" && go test -race -tags live ./sandbox-go -run '^TestLiveStdin$' -count=1 -v) 2>&1 | tee "$OUT/go-live.log"
grep -qF -- '--- PASS: TestLiveStdin' "$OUT/go-live.log"
python3 scripts/stdin-e2e/run.py "$OUT" 2>&1 | tee "$OUT/raw-http-cli.log"
grep -qF 'PASS raw HTTP and CLI stdin gate:' "$OUT/raw-http-cli.log"
git -C "$ROOT" rev-parse HEAD >"$OUT/source-revision.txt"
git -C "$ROOT" diff --binary >"$OUT/source.patch"
