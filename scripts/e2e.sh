#!/usr/bin/env bash
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/artifacts/e2e}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)

PORT=${E2E_PORT:-18081}
export SANDBOX_DOMAIN="http://127.0.0.1:$PORT"
OWNER="127.0.0.1:$PORT"
unset SANDBOX_API_KEY || true

BIN=$ROOT/bin
COVER_DIR=$OUT/coverage
SERVER_LOG=$OUT/sandboxd.log
RESULTS=$OUT/results.txt
: >"$RESULTS"
failures=0
server_pid=""
stream_pid=""

cleanup() {
  [ -n "$stream_pid" ] && kill "$stream_pid" 2>/dev/null
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null
    wait "$server_pid" 2>/dev/null
  fi
  ids=$(docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER")
  [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
}
trap cleanup EXIT

pass() { printf 'ok   %s\n' "$1" | tee -a "$RESULTS"; }
fail() {
  printf 'FAIL %s: %s (got %s, want %s)\n' "$1" "$2" "$3" "$4" | tee -a "$RESULTS"
  failures=$((failures + 1))
}
check() {
  if [ "$3" = "$4" ]; then pass "$1"; else fail "$1" "$2" "$3" "$4"; fi
}
contains() {
  if grep -qF -- "$4" "$3"; then pass "$1"; else fail "$1" "$2" "no '$4'" "present"; fi
}
lacks() {
  if grep -qF -- "$4" "$3"; then fail "$1" "$2" "found '$4'" "absent"; else pass "$1"; fi
}

note() { printf '\n== %s ==\n' "$1" | tee -a "$RESULTS"; }


note "build"
if [ "${E2E_COVERAGE:-0}" = "1" ]; then
  mkdir -p "$COVER_DIR"
  BIN=$OUT/bin
  mkdir -p "$BIN"
  {
    (cd "$ROOT" && go build -cover -covermode=atomic -coverpkg=./... -o "$BIN/sandboxd" ./cmd/sandboxd) &&
    (cd "$ROOT" && go build -cover -covermode=atomic -coverpkg=./... -o "$BIN/sandboxctl" ./cmd/sandboxctl)
  } >"$OUT/build.txt" 2>&1 || {
    echo "instrumented build failed; see $OUT/build.txt" | tee -a "$RESULTS"
    exit 1
  }
  export GOCOVERDIR="$COVER_DIR"
  pass "instrumented binaries built"
else
  (cd "$ROOT" && make build) >"$OUT/build.txt" 2>&1 || {
    echo "build failed; see $OUT/build.txt" | tee -a "$RESULTS"
    exit 1
  }
  pass "binaries built"
fi


note "server startup"
"$BIN/sandboxd" -addr "127.0.0.1:$PORT" >"$SERVER_LOG" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "$SANDBOX_DOMAIN/healthz" >"$OUT/healthz.json" 2>&1
check "healthz" "health endpoint answers" "$(grep -c '"status":"ok"' "$OUT/healthz.json")" "1"

python3 -B "$ROOT/scripts/e2e-cli-cleanup.py" "$BIN/sandboxctl" "$SANDBOX_DOMAIN" "$OUT/cli-cleanup" >"$OUT/cli-cleanup.log" 2>&1
check "cli-cleanup" "command outcome and cleanup diagnostics; see $OUT/cli-cleanup.log" "$?" "0"

(cd "$ROOT" && go test -race -tags docker ./internal/dockerbackend -run '^TestRealCreateNameConflict$' -count=1 -v) >"$OUT/container-name-conflict.log" 2>&1
check "container-name-conflict" "verified fixture ownership and replacement; see $OUT/container-name-conflict.log" "$?" "0"
contains "container-name-conflict ran" "test ran without skipping" "$OUT/container-name-conflict.log" "--- PASS: TestRealCreateNameConflict"

python3 "$ROOT/scripts/e2e-env/run.py" "$SANDBOX_DOMAIN" "$OUT/env-contract.json" >"$OUT/env-contract.log" 2>&1
check "env-contract" "bounds, replay identity, secrecy and isolation; see $OUT/env-contract.log" "$?" "0"

(cd "$ROOT" && go test -race -tags live ./sandbox-go -run '^TestLiveEnvRecoverySnapshotsInvocation$' -count=1 -v) >"$OUT/go-env-recovery.log" 2>&1
check "go-env-recovery" "caller mutation preserves accepted invocation; see $OUT/go-env-recovery.log" "$?" "0"
contains "go-env-recovery ran" "test ran without skipping" "$OUT/go-env-recovery.log" "--- PASS: TestLiveEnvRecoverySnapshotsInvocation"
python3 -B "$ROOT/scripts/e2e-concurrency/test_runner.py" -v >"$OUT/concurrency-runner.log" 2>&1
check "concurrency-runner" "direct HTTP and failure cleanup; see $OUT/concurrency-runner.log" "$?" "0"
python3 -E "$ROOT/scripts/e2e-concurrency/run.py" "$SANDBOX_DOMAIN" "$OUT/concurrency.json" >"$OUT/concurrency.log" 2>&1
check "concurrency" "admission, lifecycle, retention and outcomes; see $OUT/concurrency.log" "$?" "0"

(cd "$ROOT" && go test -race -tags live ./sandbox-go -run '^TestLiveConcurrency$' -count=1 -v) >"$OUT/go-concurrency.log" 2>&1
check "go-concurrency" "SDK overlap, cancel and unknown outcome; see $OUT/go-concurrency.log" "$?" "0"
contains "go-concurrency ran" "test ran without skipping" "$OUT/go-concurrency.log" "--- PASS: TestLiveConcurrency"

"$BIN/sandboxctl" run --max-concurrent-executions 9 -- true >"$OUT/concurrency-invalid.out" 2>"$OUT/concurrency-invalid.err"
check "concurrency-cli-invalid" "invalid slot limit is a usage error" "$?" "2"
"$BIN/sandboxctl" run --max-concurrent-executions 2 -- true >"$OUT/concurrency-cli.out" 2>"$OUT/concurrency-cli.err"
check "concurrency-cli-create" "CLI accepts concurrency limit" "$?" "0"
concurrency_sb=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/concurrency-cli.err" | head -1)
"$BIN/sandboxctl" inspect "$concurrency_sb" >"$OUT/concurrency-cli-inspect.txt" 2>&1
contains "concurrency-cli-inspect" "CLI reports concurrency limit" "$OUT/concurrency-cli-inspect.txt" "max_concurrent_executions: 2"
"$BIN/sandboxctl" kill "$concurrency_sb" >/dev/null 2>&1


note "doctor"
"$BIN/sandboxctl" doctor >"$OUT/doctor.txt" 2>"$OUT/doctor.err"
code=$?
check "doctor exit" "doctor succeeds against a local server" "$code" "0"
contains "doctor config" "config line printed" "$OUT/doctor.txt" "config: ok"
contains "doctor health" "health line printed" "$OUT/doctor.txt" "health: ok"
contains "doctor auth" "auth line printed" "$OUT/doctor.txt" "auth: ok"
contains "doctor docker" "docker line printed" "$OUT/doctor.txt" "docker: ok"
contains "doctor sandbox" "sandbox line printed" "$OUT/doctor.txt" "sandbox: ok"
contains "doctor ready" "prints Ready" "$OUT/doctor.txt" "Ready"


note "run: success"
"$BIN/sandboxctl" run -- sh -c 'echo out; echo err >&2' >"$OUT/run.out" 2>"$OUT/run.err"
code=$?
check "run exit" "a clean command exits 0" "$code" "0"
check "run stdout" "stdout carries only command output" "$(cat "$OUT/run.out")" "out"
contains "run sandbox line" "created sandbox is announced" "$OUT/run.err" "sandbox: sbx_"
contains "run created hint" "remove hint is printed" "$OUT/run.err" "remove with: sandboxctl kill sbx_"
contains "run execution line" "execution id is printed" "$OUT/run.err" "execution: exe_"
contains "run status line" "final status line" "$OUT/run.err" "completed (exited), exit 0"
sandbox_id=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/run.err" | head -1)

note "run: exit codes"
for pair in "1:1" "42:42" "125:125" "126:1" "127:1"; do
  want=${pair##*:}
  given=${pair%%:*}
  "$BIN/sandboxctl" run -- sh -c "exit $given" >/dev/null 2>"$OUT/exit-$given.err"
  code=$?
  check "run exit $given" "command exit code mapping" "$code" "$want"
done

note "run: nonzero status line"
contains "run nonzero status" "nonzero exit is reported" "$OUT/exit-42.err" "exit 42"


note "pause and resume"
"$BIN/sandboxctl" pause "$sandbox_id" >"$OUT/pause.txt" 2>&1
check "pause exit" "pause succeeds" "$?" "0"
contains "pause state" "sandbox is paused" "$OUT/pause.txt" "paused"
"$BIN/sandboxctl" resume "$sandbox_id" >"$OUT/resume.txt" 2>&1
check "resume exit" "resume succeeds" "$?" "0"
contains "resume state" "sandbox is running" "$OUT/resume.txt" "running"


note "logs, inspect, list"
"$BIN/sandboxctl" run --sandbox "$sandbox_id" -- echo second >"$OUT/second.out" 2>"$OUT/second.err"
check "run existing" "run on an existing sandbox" "$?" "0"
contains "run existing line" "existing sandbox line has no created hint" "$OUT/second.err" "sandbox: $sandbox_id"
lacks "run existing no hint" "no remove hint for an existing sandbox" "$OUT/second.err" "(created"

exec_id=$(sed -n 's/^execution: \(exe_[a-z0-9]*\).*/\1/p' "$OUT/second.err" | head -1)
"$BIN/sandboxctl" logs "$sandbox_id" "$exec_id" >"$OUT/logs.out" 2>"$OUT/logs.err"
check "logs exit" "logs succeeds" "$?" "0"
check "logs stdout" "logs prints retained stdout" "$(cat "$OUT/logs.out")" "second"

"$BIN/sandboxctl" inspect "$sandbox_id" >"$OUT/inspect.txt" 2>&1
check "inspect exit" "inspect succeeds" "$?" "0"
contains "inspect id" "inspect prints the id" "$OUT/inspect.txt" "id: $sandbox_id"

"$BIN/sandboxctl" list >"$OUT/list.txt" 2>&1
check "list exit" "list succeeds" "$?" "0"
contains "list line" "list prints ID STATE IMAGE CREATED" "$OUT/list.txt" "$sandbox_id running"


note "attach"
"$BIN/sandboxctl" attach "$sandbox_id" "$exec_id" >"$OUT/attach.out" 2>"$OUT/attach.err"
check "attach exit" "attach to a finished execution" "$?" "0"
check "attach stdout" "attach replays retained output" "$(cat "$OUT/attach.out")" "second"


note "ctrl+c"
"$BIN/sandboxctl" run -- sleep 60 >"$OUT/cancel.out" 2>"$OUT/cancel.err" &
cli_pid=$!
sleep 2
kill -INT "$cli_pid"
wait "$cli_pid"
code=$?
check "ctrl+c exit" "Ctrl+C returns 130" "$code" "130"
contains "ctrl+c status" "final status line is printed after Ctrl+C" "$OUT/cancel.err" "(canceled)"


note "oom"
"$BIN/sandboxctl" run --image python:3.12-alpine --memory 64 -- \
  python3 -c 'x = bytearray(256*1024*1024)' >"$OUT/oom.out" 2>"$OUT/oom.err"
code=$?
check "oom exit" "OOM exits 1" "$code" "1"
contains "oom status" "OOM status line" "$OUT/oom.err" "(oom)"
contains "oom hint" "OOM hint" "$OUT/oom.err" "hint: increase --memory"


note "timeout"
"$BIN/sandboxctl" run --timeout 5 -- sleep 30 >"$OUT/timeout.out" 2>"$OUT/timeout.err"
code=$?
check "timeout exit" "timeout exits 1" "$code" "1"
contains "timeout reason" "timeout status line" "$OUT/timeout.err" "(timeout)"
contains "timeout hint" "timeout hint" "$OUT/timeout.err" "hint: increase --timeout"

"$BIN/sandboxctl" run --timeout 0 --exec-timeout 3 -- sleep 30 >"$OUT/exec-timeout.out" 2>"$OUT/exec-timeout.err"
code=$?
check "exec-timeout exit" "an execution timeout exits 1" "$code" "1"
contains "exec-timeout reason" "execution timeout status line" "$OUT/exec-timeout.err" "(timeout)"
exec_timeout_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/exec-timeout.err" | head -1)
"$BIN/sandboxctl" inspect "$exec_timeout_sandbox" >"$OUT/exec-timeout-inspect.txt" 2>&1
check "exec-timeout survives" "the sandbox is still running after the command timed out" "$?" "0"
contains "exec-timeout state" "the sandbox was not killed" "$OUT/exec-timeout-inspect.txt" "state: running"
"$BIN/sandboxctl" kill "$exec_timeout_sandbox" >/dev/null 2>&1

"$BIN/sandboxctl" run --env GREETING=hi --env 'PAIR=a=b' --cwd /tmp -- \
  sh -c 'printf "%s|%s|%s" "$GREETING" "$PAIR" "$(pwd)"' \
  >"$OUT/env-cwd.out" 2>"$OUT/env-cwd.err"
check "env-cwd exit" "the probe command succeeds" "$?" "0"
check "env-cwd applied" "the execution sees its environment and directory" \
  "$(cat "$OUT/env-cwd.out")" "hi|a=b|/tmp"

env_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/env-cwd.err" | head -1)
"$BIN/sandboxctl" run --sandbox "$env_sandbox" -- \
  sh -c 'printf "%s" "${GREETING-<unset>}"' >"$OUT/env-second.out" 2>"$OUT/env-second.err"
check "env-second exit" "the second execution succeeds" "$?" "0"
check "env-second isolated" "a later execution does not inherit the environment" \
  "$(cat "$OUT/env-second.out")" "<unset>"
"$BIN/sandboxctl" kill "$env_sandbox" >/dev/null 2>&1

"$BIN/sandboxctl" run --rm --env 'MY-VAR=hyphen' --env 'CONFIG.KEY=dot' \
  --env 'SHLVL=42' -- env >"$OUT/env-names.out" 2>"$OUT/env-names.err"
check "env-names exit" "the probe command succeeds" "$?" "0"
contains "env-names hyphen" "a name with a hyphen reaches the process" "$OUT/env-names.out" "MY-VAR=hyphen"
contains "env-names dot" "a name with a dot reaches the process" "$OUT/env-names.out" "CONFIG.KEY=dot"
contains "env-names shlvl" "a shell-managed name keeps the requested value" "$OUT/env-names.out" "SHLVL=42"

"$BIN/sandboxctl" run -- sh -c 'mkdir -p /work /data/project && ln -s /data/project /work/link' \
  >"$OUT/cwd-phys-fixture.out" 2>"$OUT/cwd-phys-fixture.err"
check "cwd-phys fixture" "the fixture command succeeds" "$?" "0"
cwd_phys_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/cwd-phys-fixture.err" | head -1)
"$BIN/sandboxctl" run --sandbox "$cwd_phys_sandbox" --cwd /work/link/.. -- pwd \
  >"$OUT/cwd-phys.out" 2>"$OUT/cwd-phys.err"
check "cwd-phys exit" "a path through a symlink succeeds" "$?" "0"
check "cwd-phys resolved" "a symlink followed by .. resolves physically" \
  "$(cat "$OUT/cwd-phys.out")" "/data"
"$BIN/sandboxctl" run --sandbox "$cwd_phys_sandbox" --cwd /tmp/nope/.. -- pwd \
  >"$OUT/cwd-missing-upto.out" 2>"$OUT/cwd-missing-upto.err"
check "cwd-missing-upto exit" "a missing component followed by .. fails" "$?" "1"
contains "cwd-missing-upto marker" "the failure names the unusable directory" \
  "$OUT/cwd-missing-upto.err" "sandbox: cwd is not a usable directory: /tmp/nope/.."
check "cwd-missing-upto stdout" "nothing ran" "$(cat "$OUT/cwd-missing-upto.out")" ""
"$BIN/sandboxctl" kill "$cwd_phys_sandbox" >/dev/null 2>&1

"$BIN/sandboxctl" run --rm --timeout 0 --cwd /definitely/not/here -- true \
  >"$OUT/bad-cwd.out" 2>"$OUT/bad-cwd.err"
code=$?
check "bad-cwd exit" "a bad directory fails the invocation with status 1" "$code" "1"
check "bad-cwd empty stdout" "the marker is not program output" "$(cat "$OUT/bad-cwd.out")" ""
contains "bad-cwd marker" "stderr names the unusable directory" \
  "$OUT/bad-cwd.err" "sandbox: cwd is not a usable directory: /definitely/not/here"
contains "bad-cwd status" "the failure is distinguishable from a missing command" \
  "$OUT/bad-cwd.err" "(exited) with exit code 126"
bad_cwd_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/bad-cwd.err" | head -1)
[ -n "$bad_cwd_sandbox" ] && "$BIN/sandboxctl" kill "$bad_cwd_sandbox" >/dev/null 2>&1

"$BIN/sandboxctl" run --rm --cwd relative/dir -- true >"$OUT/relative-cwd.out" 2>"$OUT/relative-cwd.err"
check "relative-cwd exit" "a relative directory is rejected" "$?" "1"
contains "relative-cwd message" "the rejection names the requirement" \
  "$OUT/relative-cwd.err" "must be an absolute path"


note "hardening"
"$BIN/sandboxctl" run --image python:3.12-alpine -- \
  sh -c 'if [ -r /sys/fs/cgroup/pids.max ]; then cat /sys/fs/cgroup/pids.max;
         elif [ -r /sys/fs/cgroup/pids/pids.max ]; then cat /sys/fs/cgroup/pids/pids.max;
         else echo no-pids-controller; fi;
         grep -E "^NoNewPrivs:" /proc/self/status' \
  >"$OUT/hardening.out" 2>"$OUT/hardening.err"
check "hardening exit" "the probe command succeeds" "$?" "0"
contains "hardening pids" "pids limit reaches the cgroup" "$OUT/hardening.out" "1024"
contains "hardening nonewprivs" "no-new-privileges is set" "$OUT/hardening.out" "NoNewPrivs:	1"

"$BIN/sandboxctl" run --image python:3.12-alpine --pids-limit 128 -- \
  cat /sys/fs/cgroup/pids.max >"$OUT/pids-request.out" 2>"$OUT/pids-request.err"
check "pids-request exit" "the probe command succeeds" "$?" "0"
check "pids-request applied" "a requested pids limit reaches the cgroup" \
  "$(cat "$OUT/pids-request.out")" "128"

"$BIN/sandboxctl" run --image python:3.12-alpine --network none -- \
  sh -c 'awk -F: "NR>2 {gsub(/ /,\"\",\$1); print \$1}" /proc/net/dev' \
  >"$OUT/network-none.out" 2>"$OUT/network-none.err"
check "network-none exit" "the network probe succeeds" "$?" "0"
check "network-none blocked" "--network none leaves only loopback" \
  "$(tr '\n' ' ' <"$OUT/network-none.out" | tr -s ' ' | sed 's/ $//')" "lo"

"$BIN/sandboxctl" run --image python:3.12-alpine -- \
  sh -c 'awk -F: "NR>2 {gsub(/ /,\"\",\$1); print \$1}" /proc/net/dev' \
  >"$OUT/network-bridge.out" 2>"$OUT/network-bridge.err"
check "network-bridge exit" "the network probe succeeds" "$?" "0"
if grep -qv '^lo$' "$OUT/network-bridge.out"; then
  pass "network-bridge open"
else
  fail "network-bridge open" "the default network has no non-loopback interface" \
    "$(tr '\n' ' ' <"$OUT/network-bridge.out")" "eth0 or similar"
fi

if grep -qx '1024' "$OUT/hardening.out"; then
  "$BIN/sandboxctl" run --image python:3.12-alpine -- python3 -c '
import os, time
kids = []
try:
    for _ in range(5000):
        pid = os.fork()
        if pid == 0:
            time.sleep(3)
            os._exit(0)
        kids.append(pid)
except OSError:
    pass
print(len(kids))
' >"$OUT/fork.out" 2>"$OUT/fork.err"
  code=$?
  forks=$(tr -dc '0-9' <"$OUT/fork.out")
  check "fork exit" "a fork bomb does not kill the command" "$code" "0"
  if [ -n "$forks" ] && [ "$forks" -lt 2000 ]; then
    pass "fork bounded"
  else
    fail "fork bounded" "fork bomb was not bounded by the pids limit" "${forks:-none}" "<2000"
  fi

  "$BIN/sandboxctl" run --image python:3.12-alpine -- \
    sh -c 'for i in $(seq 1 20); do (true) & done; wait; echo slots-ok' \
    >"$OUT/after-fork.out" 2>"$OUT/after-fork.err"
  check "after-fork exit" "the sandbox still forks after the fork bomb" "$?" "0"
  check "after-fork reaped" "orphaned children were reaped, slots released" \
    "$(cat "$OUT/after-fork.out")" "slots-ok"
else
  fail "fork setup" "the pids limit was not observed, the destructive probe is skipped" \
    "$(head -1 "$OUT/hardening.out")" "1024"
fi

"$BIN/sandboxctl" run --image python:3.12-alpine -- python3 -c '
import socket
try:
    socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)
    print("raw-ok")
except PermissionError:
    print("raw-denied")
' >"$OUT/capraw.out" 2>"$OUT/capraw.err"
check "cap_net_raw exit" "the capability probe succeeds" "$?" "0"
check "cap_net_raw dropped" "CAP_NET_RAW is dropped" "$(cat "$OUT/capraw.out")" "raw-denied"


note "404 hint"
"$BIN/sandboxctl" inspect sbx_does_not_exist >"$OUT/notfound.out" 2>"$OUT/notfound.err"
check "404 exit" "a missing sandbox exits 1" "$?" "1"
contains "404 hint" "404 hint" "$OUT/notfound.err" "hint: check the sandbox and execution IDs"

note "busy hint"
"$BIN/sandboxctl" run -- sleep 20 >"$OUT/busy-holder.out" 2>"$OUT/busy-holder.err" &
holder_pid=$!
sleep 2
holder_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/busy-holder.err" | head -1)
"$BIN/sandboxctl" run --sandbox "$holder_sandbox" -- true >"$OUT/busy.out" 2>"$OUT/busy.err"
check "busy exit" "a second execution exits 1" "$?" "1"
contains "busy hint" "busy hint" "$OUT/busy.err" "hint: wait for the running execution or cancel it"
kill -INT "$holder_pid" 2>/dev/null
wait "$holder_pid" 2>/dev/null

note "evicted hint"
"$BIN/sandboxctl" run -- sleep 30 >"$OUT/evict.out" 2>"$OUT/evict.err" &
evict_pid=$!
for _ in $(seq 1 100); do
  grep -q '^execution: exe_' "$OUT/evict.err" 2>/dev/null && break
  sleep 0.1
done
evict_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/evict.err" | head -1)
evict_container=$(docker ps -q --filter "name=^tiny-sandbox-${evict_sandbox}$" | head -1)
if [ -n "$evict_container" ]; then
  docker kill "$evict_container" >/dev/null 2>&1
  wait "$evict_pid"
  code=$?
  check "evicted exit" "an externally killed container exits 1" "$code" "1"
  contains "evicted status" "evicted status line" "$OUT/evict.err" "(evicted)"
  contains "evicted hint" "evicted hint" "$OUT/evict.err" "hint: the container was stopped outside sandboxd"
else
  fail "evicted setup" "container tiny-sandbox-$evict_sandbox was not found" "none" "one"
  kill "$evict_pid" 2>/dev/null
fi


note "auth"
"$BIN/sandboxctl" run --timeout 0 -- sh -c 'true' >"$OUT/sweep.out" 2>"$OUT/sweep.err"
sweep_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/sweep.err" | head -1)
"$BIN/sandboxctl" run --sandbox "$sweep_sandbox" -- sleep 120 >"$OUT/sweep-stream.out" 2>"$OUT/sweep-stream.err" &
stream_pid=$!
sleep 1
started=$SECONDS
kill "$server_pid" 2>/dev/null
wait "$server_pid" 2>/dev/null
elapsed=$((SECONDS - started))
for _ in $(seq 1 50); do
  kill -0 "$stream_pid" 2>/dev/null || break
  sleep 0.1
done
if kill -0 "$stream_pid" 2>/dev/null; then
  fail "shutdown stream" "the open stream ended with the server" "still running" "ended"
  kill "$stream_pid" 2>/dev/null
else
  pass "shutdown stream"
fi
leftover=$(docker ps -aq --filter "label=tiny-sandbox.owner=$OWNER" | wc -l | tr -d ' ')
check "shutdown sweep" "SIGTERM removes this process's containers" "$leftover" "0"
if [ "$elapsed" -lt 5 ]; then
  pass "shutdown prompt"
else
  fail "shutdown prompt" "drain does not wait out its deadline" "${elapsed}s" "<5s"
fi
SANDBOX_API_KEY=e2e-secret "$BIN/sandboxd" -addr "127.0.0.1:$PORT" >"$SERVER_LOG.auth" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
"$BIN/sandboxctl" list >"$OUT/auth.out" 2>"$OUT/auth.err"
check "auth exit" "missing key exits 1" "$?" "1"
contains "auth hint" "auth hint" "$OUT/auth.err" "hint: set a valid SANDBOX_API_KEY or --api-key"
SANDBOX_API_KEY=e2e-secret "$BIN/sandboxctl" list >"$OUT/auth-ok.out" 2>&1
check "auth ok" "a valid key works" "$?" "0"


note "cleanup"
kill "$server_pid" 2>/dev/null
wait "$server_pid" 2>/dev/null
server_pid=""
docker ps -a --filter "label=tiny-sandbox.owner=$OWNER" --format '{{.ID}} {{.Names}}' >"$OUT/containers.txt"
check "containers" "no sandbox containers remain" "$(wc -l <"$OUT/containers.txt" | tr -d ' ')" "0"


note "crash recovery"
"$BIN/sandboxd" -addr "127.0.0.1:$PORT" >"$SERVER_LOG.crash" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
"$BIN/sandboxctl" run --timeout 0 -- sh -c 'true' >"$OUT/crash.out" 2>"$OUT/crash.err"
check "crash sandbox exits" "a sandbox ran before the crash" "$?" "0"
crash_sandbox=$(sed -n 's/^sandbox: \(sbx_[a-z0-9]*\).*/\1/p' "$OUT/crash.err" | head -1)
crash_container="tiny-sandbox-$crash_sandbox"
docker ps -a --filter "name=^${crash_container}$" --format '{{.ID}}' >"$OUT/crash-container.txt"

kill -9 "$server_pid" 2>/dev/null
wait "$server_pid" 2>/dev/null
server_pid=""
orphan=$(docker ps -a --filter "name=^${crash_container}$" --format '{{.ID}}' | wc -l | tr -d ' ')
check "crash leaves orphan" "SIGKILL skips the shutdown sweep" "$orphan" "1"

"$BIN/sandboxd" -addr "127.0.0.1:$PORT" >"$SERVER_LOG.restart" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "$SANDBOX_DOMAIN/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
reclaimed=$(docker ps -a --filter "name=^${crash_container}$" --format '{{.ID}}' | wc -l | tr -d ' ')
check "startup sweep" "the next start reclaims the orphan by owner label" "$reclaimed" "0"
contains "startup sweep log" "startup cleanup is reported" "$SERVER_LOG.restart" '"msg":"startup cleanup completed","removed":1'

if [ "${E2E_COVERAGE:-0}" = "1" ]; then
  note "file API coverage scenarios"
  if FILES_COVER_DIR="$COVER_DIR" bash "$ROOT/scripts/e2e-files.sh" "$OUT/files"; then
    pass "file API coverage scenarios"
  else
    fail "file API coverage scenarios" "instrumented file E2E completes" "failed" "passed"
  fi
fi


echo | tee -a "$RESULTS"
if [ "$failures" -eq 0 ]; then
  echo "PASS: all end-to-end checks passed" | tee -a "$RESULTS"
  exit 0
fi
echo "FAIL: $failures check(s) failed" | tee -a "$RESULTS"
exit 1
