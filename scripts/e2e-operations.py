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
from local import DockerFault


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--samples', type=int, default=5)
    args = parser.parse_args()
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

    def start(extra=(), success=True):
        nonlocal server
        log = (out / ('server-%d.log' % len(processes))).open('w')
        logs.append(log)
        server = subprocess.Popen([binary, '-addr', '127.0.0.1:' + str(port),
            '-owner', owner, '-docker', proxy.path, *extra], env=env, stdout=log, stderr=log)
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

    def create(**overrides):
        request = dict(image=args.image, memory_mb=64, pids_limit=64, nano_cpus=500_000_000,
                       network='none', timeout_seconds=120, idle_timeout_seconds=0)
        request.update(overrides)
        return http('/v1/sandboxes', 'POST', request)

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

    try:
        check('pinned local image', cli('image', 'inspect', args.image, '--format', '{{.Id}}') == args.image)
        (out / 'environment.json').write_text(json.dumps(dict(image=args.image,
            docker=json.loads(cli('version', '--format', '{{json .}}')),
            server_sha256=hashlib.sha256(Path(binary).read_bytes()).hexdigest()), indent=2))
        start(['-policy', str(policy)])
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
        proxy.close()
        remaining = owned()
        if remaining:
            cli('rm', '-f', *remaining)
        after = owned()
        (out / 'cleanup.json').write_text(json.dumps(dict(owner=owner, remaining=after)))
        assert not after
        shutil.rmtree(state, ignore_errors=True)


if __name__ == '__main__':
    main()
