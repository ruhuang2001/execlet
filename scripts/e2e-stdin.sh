#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/stdin-e2e}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
OWNER=127.0.0.1:$PORT
export SANDBOX_DOMAIN=http://$OWNER
unset SANDBOX_API_KEY || true
export DOCKER_HOST=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}
if [[ "$DOCKER_HOST" != unix://* ]]; then echo 'stdin-e2e requires a Unix Docker endpoint' >&2; exit 1; fi
(cd "$ROOT" && go build -o "$OUT/sandboxd" ./cmd/sandboxd && go build -o "$OUT/sandboxctl" ./cmd/sandboxctl)
"$OUT/sandboxd" -addr "$OWNER" >"$OUT/sandboxd.log" 2>&1 &
PID=$!
cleanup() {
 local result=$?
 trap - EXIT
 kill "$PID" 2>/dev/null || true
 wait "$PID" 2>/dev/null || true
 docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER" >"$OUT/containers.txt"
 if [ -s "$OUT/containers.txt" ]; then result=1; fi
 while IFS= read -r id; do [ -z "$id" ] || docker rm -f "$id" >/dev/null; done <"$OUT/containers.txt"
 docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER" >"$OUT/containers-after-cleanup.txt"
 if [ -s "$OUT/containers-after-cleanup.txt" ]; then result=1; fi
 printf '{"exit_code":%s,"leaked_before_cleanup":%s,"remaining_containers":%s}\n' "$result" "$(wc -l <"$OUT/containers.txt" | tr -d ' ')" "$(wc -l <"$OUT/containers-after-cleanup.txt" | tr -d ' ')" >"$OUT/result.json"
 exit "$result"
}
trap cleanup EXIT
for _ in $(seq 1 100); do
 if curl -fsS "$SANDBOX_DOMAIN/healthz" >"$OUT/healthz.json" 2>/dev/null; then break; fi
 kill -0 "$PID" 2>/dev/null || { cat "$OUT/sandboxd.log" >&2; exit 1; }
 sleep .1
done
curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null
(cd "$ROOT" && go test -race -tags live ./sandbox-go -run '^TestLiveStdin$' -count=1 -v) 2>&1 | tee "$OUT/go-live.log"
grep -qF -- '--- PASS: TestLiveStdin' "$OUT/go-live.log"
printf 'cli-input' | "$OUT/sandboxctl" run --rm --stdin-file - -- cat >"$OUT/cli.out" 2>"$OUT/cli.err"
test "$(cat "$OUT/cli.out")" = cli-input
git -C "$ROOT" rev-parse HEAD >"$OUT/source-revision.txt"
