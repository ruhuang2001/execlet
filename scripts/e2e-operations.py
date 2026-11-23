"""Real Docker acceptance for policy, conservative recovery, and warm-image latency."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
from http.client import HTTPConnection
import json
import os
from pathlib import Path
import shutil
import socket
import statistics
import subprocess
import sys
import time
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent / 'e2b-compatibility'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--samples', type=int, default=5)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument('--cleanup-only', action='store_true', help='run only default admission during failed removals')
    modes.add_argument('--benchmark-only', action='store_true', help='run only the direct short-command workload')
    parser.add_argument('--guest-agent', choices=['auto','off'], default='auto')
    modes.add_argument('--creation-only', action='store_true', help='measure fresh_container_local_image only')
    args = parser.parse_args()
    if not __debug__:
        parser.error('assertions must be enabled')
    assert args.samples >= 3
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    binary = str(Path(args.server).resolve())
    owner, token = 'operations-' + uuid.uuid4().hex, uuid.uuid4().hex
    env = {**os.environ, 'SANDBOX_API_KEY': token}
    docker = os.environ.get('DOCKER_HOST') or subprocess.check_output(
        ['docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'], text=True).strip()
    assert docker.startswith('unix://')
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    state, policy = out / 'state', out / 'policy.json'
    rules = dict(max_sandboxes=4, total_memory_mb=192, total_nano_cpus=2_000_000_000,
                 total_pids=256, allowed_images=[args.image], allowed_networks=['none'])
    policy.write_text(json.dumps(rules))
    report, processes, logs = [], [], []
    server = None
    proxy = None
    if not (args.benchmark_only or args.creation_only):
        from local import DockerFault
        proxy = DockerFault(out / 'docker.sock')

    def check(name, passed, evidence=None):
        report.append(dict(name=name, passed=bool(passed), evidence=evidence))
        (out / 'report.json').write_text(json.dumps(report, indent=2))
        assert passed, (name, evidence)
        print('PASS', name, flush=True)

    def cli(*command):
        return subprocess.check_output(['docker', *command], text=True, stderr=subprocess.STDOUT).strip()

    def owned():
        return cli('ps', '-aq', '--filter', 'label=tiny-sandbox.owner=' + owner).split()

    def http(path, method='GET', payload=None, auth=True, headers=None):
        h = dict(headers or {})
        if auth:
            h['Authorization'] = 'Bearer ' + token
        if payload is not None:
            h['Content-Type'] = 'application/json'
        conn = HTTPConnection('127.0.0.1', port, timeout=15)
        try:
            conn.request(method, path, None if payload is None else json.dumps(payload), h)
            response = conn.getresponse()
            data = response.read().decode()
            if response.getheader('Content-Type', '').startswith('application/json'):
                data = json.loads(data)
            return response.status, data
        finally:
            conn.close()

    def start(extra=(), success=True, legacy_faults=False):
        nonlocal server
        log = (out / ('server-%d.log' % len(processes))).open('w')
        logs.append(log)
        server = subprocess.Popen([binary, '-addr', '127.0.0.1:' + str(port),
            '-owner', owner, '-docker', proxy.path,
            '-guest-agent', 'off' if legacy_faults else args.guest_agent, *extra],
            env=env, stdout=log, stderr=log)
        processes.append(server)
        if not success:
            check('invalid configuration refuses startup', server.wait(timeout=10) != 0)
            return
        until = time.monotonic() + 15
        while time.monotonic() < until:
            if server.poll() is not None:
                raise RuntimeError('server exited: ' + log.name)
            try:
                if http('/readyz')[0] == 200:
                    return
            except OSError:
                pass
            time.sleep(.05)
        raise TimeoutError('readiness')

    def stop(crash=False):
        if server is not None and server.poll() is None:
            server.kill() if crash else server.terminate()
            server.wait(timeout=30)

    spec = dict(image=args.image, memory_mb=64, pids_limit=64, nano_cpus=500_000_000,
                network='none', timeout_seconds=120, idle_timeout_seconds=0)

    def create(**overrides):
        return http('/v1/sandboxes', 'POST', {**spec, **overrides})

    def lost_create():
        with proxy.lock:
            proxy.drop_create = True
            proxy.lost_create_id = None
        status, _ = create()
        with proxy.lock:
            lost_id = proxy.lost_create_id
            requests = proxy.requests.copy()
        check('lost Docker create response cleans up the verified container ID', status == 502
              and bool(lost_id) and not owned()
              and any(r.startswith('DELETE ') and f'/containers/{lost_id}?' in r for r in requests), lost_id)

    def cleanup_backpressure():
        start()
        lost_create()
        with ThreadPoolExecutor(max_workers=4) as pool:
            starts = list(pool.map(lambda _: create(timeout_seconds=0), range(64)))
            check('default admission fills 64 slots', all(status == 201 for status, _ in starts))
            containers = sorted(owned())
            check('all admitted containers exist in Docker', len(containers) == 64, containers)
            proxy.arm(True)
            deletes = list(pool.map(lambda pair: http('/v1/sandboxes/' + pair[1]['id'], 'DELETE')[0], starts))
            check('Docker refuses every removal', deletes == [502] * 64, deletes)
            metrics = http('/metrics')[1].splitlines()
            check('all cleanup obligations remain visible', 'tiny_sandbox_cleanup_pending 64' in metrics
                  and 'tiny_sandbox_live 0' in metrics)
            refused = list(pool.map(lambda _: create()[0], range(8)))
            check('pending cleanup blocks new admission without policy', refused == [429] * 8, refused)
            check('Docker retains exactly the original containers', sorted(owned()) == containers)
        proxy.arm(False)
        until = time.monotonic() + 90
        while time.monotonic() < until:
            if not owned() and 'tiny_sandbox_cleanup_pending 0' in http('/metrics')[1].splitlines():
                break
            time.sleep(.2)
        check('background retries confirm every original removal', not owned()
              and 'tiny_sandbox_cleanup_pending 0' in http('/metrics')[1].splitlines())
        status, sb = create()
        check('confirmed cleanup restores default admission', status == 201, sb)
        kill(sb['id'])
        stop()

    def short_benchmark():
        from latency_baseline import measure
        start(['-policy', str(policy)])
        data = measure('http://127.0.0.1:' + str(port), token, spec, args.samples)
        check('short-command workload released every container before shutdown', not owned())
        (out / 'short-latency.json').write_text(json.dumps(data, indent=2) + '\n')

    def begin(sid, command, key=''):
        status, value = http('/v1/sandboxes/' + sid + '/executions', 'POST',
                             dict(command=command, idempotency_key=key))
        assert status in (200, 201), (status, value)
        return value

    def wait(sid, eid):
        until = time.monotonic() + 20
        while time.monotonic() < until:
            status, value = http('/v1/sandboxes/' + sid + '/executions/' + eid)
            assert status == 200, (status, value)
            if value['state'] == 'completed':
                return value
            time.sleep(.01)
        raise TimeoutError(eid)

    def run(sid, code):
        return wait(sid, begin(sid, ['python3', '-c', code])['id'])

    def kill(sid):
        assert http('/v1/sandboxes/' + sid, 'DELETE')[0] == 200

    def listing_fallbacks():
        status, sb = create()
        assert status == 201, sb
        sid = sb['id']
        try:
            container, = owned()
            setup = run(sid, "import os; from pathlib import Path; p=Path('/tmp/list-ok'); p.mkdir(); "
                        "(p/'sample').write_bytes(b'abc'); Path('/tmp/list-empty').mkdir(mode=0o111); "
                        "print(os.getuid(),os.getgid(),int(os.access('/tmp/list-empty',os.R_OK)))")
            uid, gid, readable = map(int, setup['stdout'].split())
            assert setup['exit_code'] == 0 and uid != 0 and readable == 0, setup

            def listing(label, path, expected, archive):
                with proxy.lock:
                    offset = len(proxy.requests)
                status, page = http('/v1/sandboxes/' + sid + '/files/list?path=' + path)
                with proxy.lock:
                    used_archive = any(line.startswith('GET ') and '/archive?' in line
                                       for line in proxy.requests[offset:])
                actual = [(e['name'], e['type'], e['size'], e['uid'], e['gid']) for e in page.get('entries', [])]
                check(label, status == 200 and 'entries' in page and actual == expected and used_archive == archive,
                      dict(page=page, docker_archive=used_archive))

            sample = [('sample', 'file', 3, uid, gid)]
            listing('readable directory uses guest metadata', '/tmp/list-ok', sample, False)
            listing('unreadable empty directory uses archive', '/tmp/list-empty', [], True)
            cli('exec', '-u', '0', container, 'sh', '-ec',
                "printf '#!/bin/sh\\nexit 2\\n' >/usr/local/bin/stat; chmod 755 /usr/local/bin/stat")
            assert cli('exec', container, 'sh', '-ec', 'command -v stat') == '/usr/local/bin/stat'
            listing('incompatible guest stat uses archive', '/tmp/list-ok', sample, True)
        finally:
            kill(sid)

    def start_reliability(durable, managed, fault):
        label = 'start %s durable=%s managed=%s' % (fault, durable, managed)
        extra = ['-state-dir', str(out / ('start-state-' + str(managed) + '-' + fault))] if durable else []
        start(extra, legacy_faults=True)
        status, sb = create(max_concurrent_executions=2, timeout_seconds=3)
        assert status == 201, sb
        sid = sb['id']
        sibling = begin(sid, ['python3', '-c', 'import time; time.sleep(60)'])
        container = owned()[0]
        status, other = create()
        assert status == 201, other
        request = dict(command=['python3', '-u', '-c',
            "from pathlib import Path; import time; p=Path('/tmp/reliability-count'); p.write_text(p.read_text()+'x' if p.exists() else 'x'); print('RELIABILITY_READY',flush=True); time.sleep(60)"],
            idempotency_key='lost-start')
        if managed:
            request['timeout_ms'] = 0
        with proxy.lock:
            proxy.start_fault = fault
            proxy.start_faults.clear()
            proxy.release_start.clear()
            proxy.fail_remove = fault == 'drop'
        began = time.monotonic()
        with ThreadPoolExecutor(max_workers=1) as worker:
            pending = worker.submit(http, '/v1/sandboxes/' + sid + '/executions', 'POST', request)
            until = time.monotonic() + 5
            while not proxy.start_faults and time.monotonic() < until:
                time.sleep(.01)
            check(label + ' fault follows real user code', bool(proxy.start_faults), list(proxy.start_faults))
            check(label + ' independent sandbox stays usable',
                  run(other['id'], 'print(73)')['stdout'].strip() == '73')
            status, response = pending.result(timeout=15)
        elapsed = time.monotonic() - began
        check(label + ' bounded uncertainty response', status == 502 and elapsed < 9,
              dict(status=status, response=response, elapsed_seconds=elapsed))
        with proxy.lock:
            proxy.start_fault = None
        repeat, replay = http('/v1/sandboxes/' + sid + '/executions', 'POST', request)
        records = http('/v1/sandboxes/' + sid + '/executions')[1]['executions']
        uncertain = next((ex for ex in records if ex['id'] != sibling['id']), None)
        check(label + ' retained unknown prevents replay', repeat == 409 and len(records) == 2
              and uncertain is not None and uncertain['id'] in response['message']
              and uncertain['termination_reason'] == 'unknown' and uncertain['exit_code'] is None
              and uncertain['output_truncated'], dict(replay=replay, executions=records))
        lost = wait(sid, sibling['id'])
        check(label + ' sibling loss is visible', lost['termination_reason'] == 'evicted', lost)
        check(label + ' sandbox no longer accepts work', http('/v1/sandboxes/' + sid)[1]['state'] == 'killed')
        if fault == 'drop':
            check(label + ' one side effect despite replay',
                  cli('exec', container, 'cat', '/tmp/reliability-count') == 'x')
            check(label + ' failed removal remains visible',
                  http('/v1/sandboxes/' + sid, 'DELETE')[0] == 502
                  and 'tiny_sandbox_cleanup_pending 1' in http('/metrics')[1])
        check(label + ' exactly one Docker start', len(proxy.start_faults) == 1, list(proxy.start_faults))
        proxy.arm(False)
        proxy.release_start.set()
        until = time.monotonic() + 10
        while container in owned() and time.monotonic() < until:
            time.sleep(.1)
        check(label + ' cleanup confirmed independently', container not in owned())
        kill(other['id'])
        stop()

    try:
        check('pinned local image', cli('image', 'inspect', args.image, '--format', '{{.Id}}') == args.image)
        (out / 'environment.json').write_text(json.dumps(dict(image=args.image, control_path=args.guest_agent, supervisor_faults='legacy',
            docker=json.loads(cli('version', '--format', '{{json .}}')),
            python=sys.version, git_head=subprocess.check_output(['git', 'rev-parse', 'HEAD'],
                cwd=Path(__file__).resolve().parent, text=True).strip(),
            server_sha256=hashlib.sha256(Path(binary).read_bytes()).hexdigest()), indent=2))
        if args.creation_only:
            start(['-policy', str(policy)])
            fresh = []
            for _ in range(args.samples):
                began = time.monotonic()
                status, sb = create()
                fresh.append(time.monotonic() - began)
                assert status == 201 and sb['agent_mode'] == ('guest' if args.guest_agent == 'auto' else 'legacy'), sb
                kill(sb['id'])
            result = dict(samples=len(fresh), median_seconds=statistics.median(fresh),
                          p95_seconds=sorted(fresh)[int(len(fresh)*.95)], raw_seconds=fresh)
            (out/'baseline.json').write_text(json.dumps(dict(fresh_container_local_image=result,
                   image_pull_measured=False, agent_mode=sb['agent_mode']), indent=2)+'\n')
            check('fresh container baseline and cleanup', not owned(), result)
            return
        if args.benchmark_only:
            short_benchmark()
            return
        cleanup_backpressure()
        if args.cleanup_only:
            return
        for durable in [False, True]:
            for managed in [False, True]:
                for fault in ['drop', 'stall']:
                    start_reliability(durable, managed, fault)
        start(['-policy', str(policy)], legacy_faults=True)
        lost_create()
        listing_fallbacks()
        stop()
        start(['-policy', str(policy)])
        status, sb = create()
        assert status == 201, sb
        check('requested control path is active', sb['agent_mode'] == ('guest' if args.guest_agent == 'auto' else 'legacy'), sb)
        long_command = ['python3', '-u', '-c', "import time; print('before',flush=True); time.sleep(6); print('after')"]
        initial = begin(sb['id'], long_command, 'long-stream')
        final = wait(sb['id'], initial['id'])
        replay = begin(sb['id'], long_command, 'long-stream')
        check('normal stream outlives handshake and initiating request', final['stdout'] == 'before\nafter\n'
              and final['exit_code'] == 0 and replay['id'] == initial['id'], final)
        kill(sb['id'])
        check('metrics authentication', http('/metrics', auth=False)[0] == 401)
        check('capability authentication', http('/v1/capabilities', auth=False)[0] == 401)
        check('unapproved image denied before create', create(image='unapproved:latest')[0] == 400 and not owned())
        check('unapproved bridge denied before create', create(network='bridge')[0] == 400 and not owned())
        with ThreadPoolExecutor(max_workers=6) as workers:
            responses = list(workers.map(lambda _: create(), range(6)))
        accepted = [v['id'] for status, v in responses if status == 201]
        check('atomic aggregate resource admission', len(accepted) == 3 and
              sum(s == 429 for s, _ in responses) == 3 and len(owned()) == 3, responses)
        sid = accepted[0]
        limits = run(sid, "from pathlib import Path; print('|'.join(Path('/sys/fs/cgroup/'+n).read_text().strip() for n in ['memory.max','memory.swap.max','pids.max','cpu.max'])); print(','.join(sorted(p.name for p in Path('/sys/class/net').iterdir())))")
        check('kernel limits and isolated namespace', limits['exit_code'] == 0 and
              limits['stdout'].splitlines() == ['67108864|0|64|50000 100000', 'lo'], limits)
        peer = cli('run', '-d', '--label', 'tiny-sandbox.owner=' + owner, '--network', 'bridge',
                   args.image, 'python3', '-m', 'http.server', '8765', '--bind', '0.0.0.0')
        peer_ip = cli('inspect', peer, '--format', '{{.NetworkSettings.Networks.bridge.IPAddress}}')
        connect = "import socket; s=socket.create_connection((%r,8765),1); print('connected'); s.close()" % peer_ip
        allowed = subprocess.run(['docker', 'run', '--rm', '--label', 'tiny-sandbox.owner=' + owner,
            '--network', 'bridge', args.image, 'python3', '-c', connect], capture_output=True, text=True)
        check('bridge positive control to private peer', allowed.returncode == 0 and allowed.stdout.strip() == 'connected', allowed.stderr)
        for target in [peer_ip, '169.254.169.254', '10.0.0.1']:
            denied = run(sid, "import socket; s=socket.socket(); s.settimeout(.2); print(s.connect_ex((%r,8765)))" % target)
            check('restricted route denied ' + target, denied['exit_code'] == 0 and int(denied['stdout']) != 0, denied)
        cli('rm', '-f', peer)
        for sid in accepted:
            kill(sid)
        status, value = create()
        check('cleanup releases budget', status == 201, value)
        kill(value['id'])
        metrics = http('/metrics')[1]
        (out / 'policy-metrics.txt').write_text(metrics)
        check('rejection metric without identifier labels', 'outcome="rate_limited"' in metrics
              and 'tiny_sandbox_reserved_memory_bytes 0' in metrics and token not in metrics
              and all(s not in metrics for s in accepted))
        stop()
        start(['-policy', str(policy)], legacy_faults=True)
        status, sb = create(max_concurrent_executions=2)
        assert status == 201, sb
        sid = sb['id']
        sibling = begin(sid, ['python3', '-c', 'import time; time.sleep(60)'])
        command = dict(command=['python3', '-u', '-c',
            "from pathlib import Path; import time; p=Path('/tmp/handshake-count'); p.write_text(p.read_text()+'1' if p.exists() else '1'); print('READY',flush=True); time.sleep(60)"],
            timeout_ms=8000, idempotency_key='identity-fault')
        proxy.fail_identity = True
        began = time.monotonic()
        status, failed = http('/v1/sandboxes/' + sid + '/executions', 'POST', command)
        proxy.fail_identity = False
        check('PID handshake fault actually fired', bool(proxy.failed_identities), proxy.failed_identities)
        check('PID failure reports uncertainty without destroying siblings', status == 502 and
              http('/v1/sandboxes/' + sid)[1]['state'] == 'running' and
              http('/v1/sandboxes/' + sid + '/executions/' + sibling['id'])[1]['state'] == 'running', failed)
        records = http('/v1/sandboxes/' + sid + '/executions')[1]['executions']
        uncertain = next(ex for ex in records if ex['id'] != sibling['id'])
        replay_status, replay = http('/v1/sandboxes/' + sid + '/executions', 'POST', command)
        check('unconfirmed start retains key and execution identity', replay_status == 502 and
              uncertain['id'] in replay['message'] and uncertain['id'] in failed['message'], replay)
        final = wait(sid, uncertain['id'])
        check('unobserved command keeps original remote deadline', final['termination_reason'] == 'timeout'
              and time.monotonic() - began < 12 and final['stdout'] == 'READY\n', final)
        check('uncertain start executed only once', run(sid, "print(open('/tmp/handshake-count').read())")['stdout'].strip() == '1')
        kill(sid)
        status, held = create(memory_mb=192)
        assert status == 201, held
        proxy.arm(True)
        check('failed removal keeps reservation', http('/v1/sandboxes/' + held['id'], 'DELETE')[0] == 502
              and create()[0] == 429 and len(owned()) == 1)
        check('cleanup failure visible in metrics', 'tiny_sandbox_cleanup_pending 1' in http('/metrics')[1])
        proxy.arm(False)
        until = time.monotonic() + 10
        while owned() and time.monotonic() < until:
            time.sleep(.1)
        check('cleanup retry confirms removal', not owned())
        status, released = create(memory_mb=192)
        check('confirmed retry releases reservation', status == 201, released)
        kill(released['id'])
        stop()
        bad = out / 'unsupported-policy.json'
        bad.write_text(json.dumps({**rules, 'allowed_networks': ['filtered']}))
        start(['-policy', str(bad)], success=False)
        recovery = ['-state-dir', str(state), '-policy', str(policy)]
        start(recovery)
        status, sandbox = create()
        assert status == 201, sandbox
        sid = sandbox['id']
        command = ['python3', '-c', "from pathlib import Path; p=Path('/tmp/count'); n=int(p.read_text())+1 if p.exists() else 1; p.write_text(str(n)); print(n)"]
        first = begin(sid, command, 'exactly-once')
        check('original execution recorded', wait(sid, first['id'])['stdout'].strip() == '1')
        stop(crash=True)
        start(recovery)
        replay = begin(sid, command, 'exactly-once')
        check('restart preserves identity output and key', replay['id'] == first['id'] and replay['stdout'].strip() == '1', replay)
        check('restart does not rerun completed command', run(sid, "print(open('/tmp/count').read())")['stdout'].strip() == '1')
        status, events = http('/v1/sandboxes/' + sid + '/executions/' + first['id'] + '/events')
        check('completed stream survives restart', status == 200 and 'event: complete' in events and 'event: stdout' in events)
        duplicate = subprocess.run([binary, '-addr', '127.0.0.1:0', '-owner', owner,
            '-state-dir', str(state), '-docker', docker[7:]], env=env, capture_output=True, timeout=10)
        check('single writer lock', duplicate.returncode != 0 and http('/v1/sandboxes/' + sid)[1]['state'] == 'running')
        status, active_sb = create()
        assert status == 201, active_sb
        active = active_sb['id']
        ex = begin(active, ['python3', '-u', '-c', "import time; print('started',flush=True); time.sleep(90)"], 'interrupted')
        for _ in range(100):
            snapshot = http('/v1/sandboxes/' + active + '/executions/' + ex['id'])[1]
            if snapshot['stdout']:
                break
            time.sleep(.01)
        check('interrupted task actually ran', snapshot['stdout'].strip() == 'started')
        stop(crash=True)
        start(recovery)
        unknown = wait(active, ex['id'])
        check('interrupted work unknown not success', unknown['termination_reason'] == 'unknown'
              and unknown['exit_code'] is None and unknown['output_truncated'], unknown)
        check('interrupted sandbox removed without replay', http('/v1/sandboxes/' + active)[1]['state'] == 'killed' and len(owned()) == 1)
        check('lost cursor explicitly rejected', http('/v1/sandboxes/' + active + '/executions/' + ex['id']
              + '/events', headers={'Last-Event-ID': '1'})[0] == 409)
        check('independent idle sandbox survives', http('/v1/sandboxes/' + sid)[1]['state'] == 'running')
        stop()
        start(recovery)
        check('graceful stop preserves idle container', http('/v1/sandboxes/' + sid)[1]['state'] == 'running')
        stop()
        for record in [state / (sid + '-' + first['id'] + '.json'), state / 'metadata.json']:
            saved, before = record.read_bytes(), owned()
            record.unlink()
            start(recovery, success=False)
            check('missing record refuses before Docker: ' + record.name, owned() == before and not record.exists())
            record.write_bytes(saved)
            record.chmod(0o600)
        record = state / (sid + '-' + first['id'] + '.json')
        saved, before = record.read_bytes(), owned()
        corrupted = json.loads(saved)
        corrupted['events'][-1]['Execution']['exit_code'] = 37
        record.write_text(json.dumps(corrupted))
        start(recovery, success=False)
        check('conflicting persisted outcomes rejected before Docker', owned() == before)
        record.write_bytes(saved)
        start(recovery)
        check('repair preserves key', begin(sid, command, 'exactly-once')['id'] == first['id'])
        pending = begin(sid, ['python3', '-u', '-c',
            "from pathlib import Path; import time; print('storage-ready',flush=True);\nwhile not Path('/tmp/storage-go').exists(): time.sleep(.01)\nprint('storage-finished')"], 'storage-outage')
        until = time.monotonic() + 5
        while time.monotonic() < until:
            if http('/v1/sandboxes/' + sid + '/executions/' + pending['id'])[1]['stdout']:
                break
            time.sleep(.01)
        container = owned()[0]
        offline = out / 'state-offline'
        state.rename(offline)
        try:
            cli('exec', container, 'touch', '/tmp/storage-go')
            uncertain = wait(sid, pending['id'])
            check('storage outage never acknowledges terminal success', uncertain['exit_code'] is None
                  and uncertain['termination_reason'] == 'unknown', uncertain)
            before = owned()
            check('storage outage closes admission and readiness', http('/readyz')[0] == 503
                  and create()[0] == 503 and owned() == before)
            stop(crash=True)
        finally:
            offline.rename(state)
        start(recovery)
        uncertain = wait(sid, pending['id'])
        check('storage repair does not replay uncommitted completion', uncertain['termination_reason'] == 'unknown'
              and uncertain['exit_code'] is None and not owned(), uncertain)
        stop()
        start(['-policy', str(policy)])
        fresh, reused, first_output = [], [], []
        for _ in range(args.samples):
            t = time.monotonic()
            status, sb = create()
            assert status == 201, sb
            fresh.append(time.monotonic() - t)
            sid = sb['id']
            for __ in range(3):
                t = time.monotonic()
                ex = begin(sid, ['python3', '-u', '-c', "import time; print('first',flush=True); time.sleep(.05); print('last')"])
                conn = HTTPConnection('127.0.0.1', port, timeout=15)
                try:
                    conn.request('GET', '/v1/sandboxes/' + sid + '/executions/' + ex['id'] + '/events',
                                 headers={'Authorization': 'Bearer ' + token})
                    response = conn.getresponse()
                    assert response.status == 200
                    found, completed = False, False
                    for line in response:
                        if not found and line.startswith(b'event: stdout'):
                            first_output.append(time.monotonic() - t)
                            found = True
                        completed |= line.startswith(b'event: complete')
                    assert found and completed
                finally:
                    conn.close()
                reused.append(time.monotonic() - t)
            kill(sid)
        def stats(values):
            return dict(samples=len(values), median_seconds=statistics.median(values),
                        p95_seconds=sorted(values)[min(len(values)-1, int(len(values)*.95))], raw_seconds=values)
        baseline = dict(fresh_container_local_image=stats(fresh), reused_container_command=stats(reused),
                        first_stdout_from_start_request=stats(first_output), image_pull_measured=False)
        (out / 'baseline.json').write_text(json.dumps(baseline, indent=2))
        check('repeatable latency baseline', len(first_output) == args.samples * 3, baseline)
        metrics = http('/metrics')[1]
        (out / 'metrics.txt').write_text(metrics)
        check('latency and cleanup metrics', all(name in metrics for name in [
            'tiny_sandbox_create_seconds_bucket', 'tiny_sandbox_first_output_seconds_count',
            'tiny_sandbox_execution_start_seconds_sum', 'tiny_sandbox_cleanup_pending 0']))
        check('Prometheus family types declared once', all(metrics.count('# TYPE ' + name + ' ' + kind + '\n') == 1
              for name, kind in [('tiny_sandbox_create_seconds', 'histogram'),
                                 ('tiny_sandbox_http_seconds', 'histogram'),
                                 ('tiny_sandbox_cleanup_failures_total', 'counter'),
                                 ('tiny_sandbox_live', 'gauge')]))
        stop()
        short_benchmark()
    finally:
        for proc in processes:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=5)
        for log in logs:
            log.close()
        if proxy:
            proxy.close()
        remaining = owned()
        if remaining:
            cli('rm', '-f', *remaining)
        after = owned()
        (out / 'cleanup.json').write_text(json.dumps(dict(owner=owner, remaining=after)))
        assert not after
        shutil.rmtree(state, ignore_errors=True)
        for private_state in out.glob('start-state-*'):
            shutil.rmtree(private_state)


if __name__ == '__main__':
    main()
