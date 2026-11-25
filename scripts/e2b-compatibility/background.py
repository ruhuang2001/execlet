"""Next-profile acceptance: real installed adapters, never a probe replacement."""
from __future__ import annotations

import base64
import hashlib
import http.client
import json
from pathlib import Path
import re
import shlex
import subprocess
import threading
import time

from local import Fault, Local
from runner import HERE, ROOT, save
from scenarios import FIX, a12 as baseline_a12, a16 as baseline_a16, src

INPUT = bytes([0, 255, 13, 10, 128, 7]) + 'input:世界🌍'.encode()
ARTIFACT = INPUT[::-1] + bytes([0]) + b'built' + bytes([255])
PRE = ('PRE:世界🌍\n', 'PRE:err🙂\n')
POST = (''.join(f'out:{i:02}:世界🌍\n' for i in range(12)),
        ''.join(f'err:{i:02}:不同🙂\n' for i in range(12)))


def raw(ctx, method, path, body=None, expected=200):
    """Independent HTTP control, bypassing SDK/proxy; no implicit renewal."""
    connection = http.client.HTTPConnection('127.0.0.1', ctx.local.port, timeout=20)
    try:
        connection.request(method, path, json.dumps(body) if body is not None else None,
                           {'Authorization': 'Bearer ' + ctx.local.api_key, 'Content-Type': 'application/json'})
        response = connection.getresponse()
        payload = response.read()
        try:
            value = json.loads(payload)
        except ValueError:
            value = payload.decode(errors='replace')
        evidence = {'at': time.monotonic(), 'method': method, 'path': path,
                    'status': response.status, 'response': value}
        if isinstance(value, dict):
            evidence['response'] = {k: {'bytes': len(v.encode()), 'sha256': hashlib.sha256(v.encode()).hexdigest()}
                                   if isinstance(v, str) and len(v) > 2048 else v for k, v in value.items()}
        with (ctx.out / 'native.jsonl').open('a') as log:
            log.write(json.dumps(evidence, ensure_ascii=False) + '\n')
        if expected is not None:
            assert response.status == expected, (method, path, response.status, value)
        return response.status, value
    finally:
        connection.close()


def execution(ctx, sid, eid):
    return raw(ctx, 'GET', f'/v1/sandboxes/{sid}/executions/{eid}')[1]


def terminal(ctx, sid, eid, seconds=15):
    assert ctx.wait_until(lambda: execution(ctx, sid, eid)['state'] != 'running', seconds), 'no terminal evidence'
    return execution(ctx, sid, eid)


def traffic(ctx, method=None, suffix=None):
    return [r for r in ctx.local.faults.records if (method is None or r['method'] == method)
            and (suffix is None or r['path'].endswith(suffix))]


def mutations(ctx):
    return [r for r in traffic(ctx) if r['method'] in ('POST', 'PATCH', 'DELETE')]


def wait(ctx, worker=None, handle='h', **options):
    return ctx.call('handle_wait', worker, handle=handle, quiet=True, **options)


def attached(ctx, sid, pid, worker=None, handle='attached', **options):
    return ctx.ok('command_connect', 'attach managed command', worker, id=sid, pid=pid, handle=handle, **options)


def start(ctx, sid, command, worker=None, handle='h', **options):
    return ctx.ok('background', 'background start', worker, id=sid, command=command, handle=handle, **options)


def outcome(reply, code=0):
    if code:
        assert not reply['ok'] and reply['error']['family'] == 'exit', reply
        value = reply['error']
    else:
        assert reply['ok'], reply
        value = reply['value']
    assert value['exit'] == code, value
    return value


def callbacks(reply):
    return tuple(''.join(reply['callbacks'][stream]) for stream in ('stdout', 'stderr'))


def no_new_callbacks(reply):
    data = reply['callbacks']
    return not data.get('new_stdout', data['stdout']) and not data.get('new_stderr', data['stderr'])


class Job:
    """File-gated workload; it implements no adapter or process-management API."""
    def __init__(self, ctx, sid=None, name='job', **config):
        self.ctx = ctx
        self.sid = sid or ctx.create(duration=180)
        self.root = '/home/user/background/' + name
        self.worker = ctx.worker
        ctx.write(self.sid, self.root + '/guest.py', text=(HERE / 'background-guest.py').read_text())
        ctx.write(self.sid, self.root + '/input.bin', bytes=base64.b64encode(INPUT).decode())
        self.command = ('exec python3 ' + shlex.quote(self.root + '/guest.py') + ' '
                        + shlex.quote(json.dumps({'root': self.root, **config})))

    def start(self, worker=None, **kwargs):
        self.worker = worker or self.ctx.worker
        self.info = start(self.ctx, self.sid, self.command, self.worker, **kwargs)
        assert isinstance(self.info.get('execution_id'), str) and self.info['execution_id'], 'missing execution identity'
        return self.info

    def file(self, name, worker=None):
        reply = self.ctx.call('read', worker or self.worker, id=self.sid, path=self.root + '/' + name, quiet=True)
        if reply['ok']:
            return reply['value']
        assert reply['error']['family'] == 'file_not_found', reply
        return None

    def ready(self, worker=None):
        assert self.ctx.wait_until(lambda: self.file('READY', worker) == 'ready', 12), 'guest never READY'
        self.identity = json.loads(self.file('identity.json', worker))
        return self.identity

    def release(self, name='GO', worker=None):
        self.ctx.write(self.sid, self.root + '/' + name, worker or self.worker, text='go')

    def alive(self, identity=None):
        run = subprocess.run([self.ctx.env['CONTRACT_PYTHON'], str(HERE / 'observe.py'),
                              'tiny', 'process', self.sid, self.ctx.env['CONTRACT_OWNER'],
                              json.dumps(identity or self.identity)], env=self.ctx.env, text=True,
                             capture_output=True, timeout=25, check=True)
        evidence = json.loads(run.stdout)
        with (self.ctx.out / 'processes.jsonl').open('a') as log:
            log.write(json.dumps({'at': time.monotonic(), **evidence}) + '\n')
        return evidence['alive']

    def artifact(self, worker=None):
        value = self.ctx.ok('read', 'artifact', worker or self.worker, id=self.sid,
                            path=self.root + '/artifact.bin', options={'format': 'bytes'})
        data = base64.b64decode(value['bytes'])
        assert data == ARTIFACT, hashlib.sha256(data).hexdigest()
        assert self.file('counter', worker) == '1', 'work executed more than once'
        return hashlib.sha256(data).hexdigest()


def fresh(ctx, sid, language=None):
    worker = ctx.spawn(language=language)
    ctx.ok('connect', 'fresh sandbox client', worker, id=sid)
    return worker


def native_sandbox(ctx, slots=2, memory=128):
    _, sb = raw(ctx, 'POST', '/v1/sandboxes', {'image': ctx.local.image,
        'max_concurrent_executions': slots, 'memory_mb': memory,
        'timeout_seconds': 600, 'network': 'none'}, 201)
    with (ctx.out / 'owned.jsonl').open('a') as log:
        log.write(json.dumps({'id': sb['id']}) + '\n')
    ctx.ok('connect', 'native sandbox', id=sb['id'], duration=600)
    return sb['id']


def container(ctx, sid):
    value = json.loads(subprocess.check_output(['docker', 'inspect', 'execlet-' + sid], text=True))[0]
    assert value['Config']['Labels'].get('execlet.owner') == ctx.env['CONTRACT_OWNER']
    return value['Id']


def event_overflow(ctx, job, eid):
    """Force 4097 distinct real output writes using SSE acknowledgements."""
    script = ('import pathlib,sys; p=pathlib.Path(sys.argv[1]);\n'
              'for line in sys.stdin:\n t=p.with_suffix(".tmp");t.write_text(line.strip());t.replace(p)')
    ack = subprocess.Popen(['docker', 'exec', '-i', container(ctx, job.sid), 'python3', '-c',
                             script, job.root + '/ACK'], stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    errors, frames = [], []
    connection = http.client.HTTPConnection('127.0.0.1', ctx.local.port, timeout=30)
    def observe():
        try:
            connection.request('GET', f'/v1/sandboxes/{job.sid}/executions/{eid}/events',
                headers={'Authorization': 'Bearer ' + ctx.local.api_key})
            response = connection.getresponse()
            assert response.status == 200
            kind, data = '', []
            count = 0
            while True:
                line = response.readline().decode()
                assert line, 'stream ended before the event-cap evidence'
                if line.startswith('event:'): kind = line[6:].strip()
                if line.startswith('data:'): data.append(line[5:].strip())
                if line.strip(): continue
                if kind == 'stdout':
                    value = json.loads('\n'.join(data))['data']
                    assert value == f'event:{count:04}\n', (count, value)
                    frames.append(value)
                    ack.stdin.write(str(count) + '\n'); ack.stdin.flush()
                    count += 1
                if kind == 'truncated':
                    assert count == 4096, count
                    ack.stdin.write('4096\n'); ack.stdin.flush()
                    return
                kind, data = '', []
        except BaseException as exc:
            errors.append(repr(exc))
    thread = threading.Thread(target=observe, daemon=True)
    thread.start()
    try:
        job.release()
        thread.join(timeout=90)
        assert not thread.is_alive(), 'event overflow observer watchdog'
        assert not errors, errors
        state = terminal(ctx, job.sid, eid)
        assert state['output_truncated'] and state['stdout'] == ''.join(frames)
        assert len(state['stdout'].encode()) < 1048576, 'tested byte cap rather than event cap'
        assert job.file('AFTER') == 'complete', 'overflow fixture did not finish'
        return {'events': len(frames), 'bytes': len(state['stdout'].encode()), 'execution_id': eid}
    finally:
        connection.close()
        ack.stdin.close()
        try: ack.wait(timeout=5)
        except subprocess.TimeoutExpired: ack.kill(); ack.wait()
        ack.stdout.close(); ack.stderr.close()


def b01(ctx):
    job = Job(ctx)
    info = job.start(wait=0, callbacks=ctx.language == 'typescript')
    identity = job.ready()
    ctx.check('handle precedes release and has real guest PID', info['pid'] == identity['pid']
              and job.file('AFTER') is None and job.file('counter') == '1', {**info, **identity})
    job.release()
    value = outcome(wait(ctx))
    ctx.check('start observes exact output', (value['stdout'], value['stderr']) == tuple(a + b for a, b in zip(PRE, POST)))
    for code, command in ((0, 'true'), (37, 'exit 37')):
        start(ctx, job.sid, command, handle=f'fast-{code}')
        outcome(wait(ctx, handle=f'fast-{code}'), code)
    ctx.check('fast exits return handles with real outcomes', True)
    value = ctx.must_run(job.sid, 'printf foreground', options={'background': False})
    ctx.check('foreground remains a result', value['stdout'] == 'foreground')


def transfer(ctx, target, mode='success'):
    job = Job(ctx, name=f'transfer-{len(ctx.workers)}', exit=37 if mode == 'failure' else 0)
    owner = ctx.worker
    info = job.start(wait=0)
    job.ready()
    def prefix_retained():
        snapshot = execution(ctx, job.sid, info['execution_id'])
        return (snapshot['stdout'], snapshot['stderr']) == PRE
    assert ctx.wait_until(prefix_retained, 5), 'pre-attachment output never reached the daemon'
    ctx.ok('handle_disconnect', 'detach', handle='h')
    if mode == 'late':
        job.release()
        terminal(ctx, job.sid, info['execution_id'])
    old_pid = owner.identity['pid']
    owner.close()
    receiver = fresh(ctx, job.sid, target)
    assert receiver.identity['pid'] != old_pid
    before = ctx.local.faults.mutation_count()
    new = attached(ctx, job.sid, info['pid'], receiver, wait=.3 if mode == 'timeout' else 0,
                   callbacks=target == 'typescript')
    assert new['execution_id'] == info['execution_id'], (new, info)
    assert ctx.local.faults.mutation_count() == before, mutations(ctx)
    if mode == 'timeout':
        reply = wait(ctx, receiver, 'attached')
        assert not reply['ok'] and reply['error']['family'] == 'timeout', reply
        assert job.alive()
        attached(ctx, job.sid, info['pid'], receiver, handle='resumed', wait=0, callbacks=target == 'typescript')
        name = 'resumed'
    else:
        name = 'attached'
    if mode == 'kill':
        assert ctx.ok('handle_kill', 'kill handoff', receiver, handle=name) is True
        assert not job.alive()
        reply = wait(ctx, receiver, name)
        assert not reply['ok'] and reply['error'].get('code') == 'canceled', reply
    else:
        if mode != 'late':
            job.release(worker=receiver)
        reply = wait(ctx, receiver, name)
        value = outcome(reply, 37 if mode == 'failure' else 0)
        expected = ('', '') if mode == 'late' else POST
        assert (value['stdout'], value['stderr']) == expected, value
        assert callbacks(reply) == expected, reply['callbacks']
        job.artifact(receiver)
    ctx.ok('kill', 'explicit handoff cleanup', receiver, id=job.sid)
    assert not ctx.state(job.sid)['alive']
    save(ctx.out / f'handoff-{target}-{mode}.json', {'controller_pid': old_pid,
         'receiver_pid': receiver.identity['pid'], 'reference': {'sandbox_id': job.sid, 'pid': info['pid']},
         'execution_id': info['execution_id'], 'mode': mode, 'result': reply})
    ctx.worker = ctx.spawn()


def b02(ctx):
    transfer(ctx, ctx.language)
    ctx.check('fresh process resolves one surviving command', True)


def deadline(ctx, background=True):
    job = Job(ctx, name='deadline')
    watcher = fresh(ctx, job.sid)
    before = time.monotonic()
    if background:
        info = job.start(wait=1.25)
        job.ready(watcher)
        ctx.ok('handle_disconnect', 'detach timed command')
    else:
        ctx.worker.begin('run', id=job.sid, command=job.command, wait=1.25, callbacks=True)
        job.ready(watcher)
        reply = ctx.worker.finish()
        assert not reply['ok'] and reply['error']['family'] == 'timeout', reply
        info = {'execution_id': reply['error'].get('execution_id')}
    starts = [r for r in traffic(ctx, 'POST', '/executions') if job.root in json.dumps(r.get('body'))]
    assert starts and all(r['body'].get('timeout_ms') == 1250 and 'timeout_seconds' not in r['body'] for r in starts), starts
    assert ctx.wait_until(lambda: not job.alive(), max(.1, before + 4.25 - time.monotonic())), 'deadline did not terminate process'
    assert job.file('AFTER', watcher) is None
    state = terminal(ctx, job.sid, info['execution_id'])
    assert state.get('termination_reason') == 'timeout', state
    ctx.check('precise deadline terminates detached work' if background else 'foreground deadline terminates work',
              True, {'elapsed': time.monotonic() - before, 'execution': state})
    control = Job(ctx, name='zero')
    control.start(wait=0)
    control.ready()
    time.sleep(1.6)
    assert control.alive()
    control.release()
    outcome(wait(ctx))
    ctx.check('zero deadline control completes artifact', bool(control.artifact()))


def b03(ctx):
    deadline(ctx)


def a05(ctx):
    deadline(ctx, background=False)
    sid = ctx.create(duration=60)
    reply = ctx.call('run', id=sid, command='printf callback; sleep .2; echo alive > /tmp/cb',
                     callback_error='sdk', quiet=True)
    ctx.check('callback exception keeps identity', reply.get('value', {}).get('callback_error_preserved') is True, reply)
    ctx.check('callback failure does not cancel work', ctx.wait_until(
        lambda: ctx.call('read', id=sid, path='/tmp/cb', quiet=True).get('value') == 'alive\n', 10))


def b04(ctx):
    job = Job(ctx)
    info = job.start(wait=0)
    job.ready()
    ctx.ok('handle_disconnect', 'detach')
    before = len(mutations(ctx))
    attached(ctx, job.sid, info['pid'], wait=1.25)
    reply = wait(ctx, handle='attached')
    ctx.check('attach timeout is local', not reply['ok'] and reply['error']['family'] == 'timeout' and job.alive(), reply)
    attached(ctx, job.sid, info['pid'], handle='again', wait=0, callbacks=ctx.language == 'typescript')
    job.release()
    value = outcome(wait(ctx, handle='again'))
    ctx.check('reattach neither restarts nor renews', len(mutations(ctx)) == before and job.file('counter') == '1')
    ctx.check('reattach observes completion suffix', (value['stdout'], value['stderr']) == POST)


def b05(ctx):
    job = Job(ctx)
    info = job.start()
    job.ready()
    requests = traffic(ctx, 'POST', '/executions')
    ctx.check('omitted run timeout is 60000 ms', requests[-1]['body'].get('timeout_ms') == 60000, requests[-1])
    ctx.ok('handle_disconnect', 'detach default')
    attached(ctx, job.sid, info['pid'], wait=.5)
    time.sleep(.8)
    reply = wait(ctx, handle='attached')
    ctx.check('wait cannot reset observation clock', not reply['ok'] and reply['error']['family'] == 'timeout'
              and reply['elapsed'] < .35 and job.alive(), reply)
    timed = Job(ctx, name='remote-clock')
    clock = time.monotonic()
    info = timed.start(wait=3)
    timed.ready()
    ctx.ok('handle_disconnect', 'detach remote clock')
    time.sleep(1.5)
    attached(ctx, timed.sid, info['pid'], wait=30)
    ctx.check('reattach cannot extend remote deadline', ctx.wait_until(lambda: not timed.alive(),
              max(.1, clock + 6 - time.monotonic())))
    state = terminal(ctx, timed.sid, info['execution_id'])
    ctx.check('remote expiry remains diagnostic', state.get('termination_reason') == 'timeout', state)


def b06(ctx):
    job = Job(ctx)
    info = job.start(wait=0)
    job.ready()
    attached(ctx, job.sid, info['pid'], wait=0, callbacks=ctx.language == 'typescript')
    before = len(mutations(ctx))
    first = ctx.call('handle_disconnect')
    second = ctx.call('handle_disconnect')
    reply = wait(ctx)
    ctx.check('disconnect is prompt idempotent and local', max(first['elapsed'], second['elapsed']) < 1
              and len(mutations(ctx)) == before and job.alive())
    ctx.check('disconnected wait cannot revive observation', not reply['ok'] and reply['error'].get('code') == 'disconnected', reply)
    job.release()
    outcome(wait(ctx, handle='attached'))
    ctx.check('other observer survives disconnect', bool(job.artifact()))
    live = Job(ctx, name='kill-disconnected')
    live.start(wait=0)
    live.ready()
    ctx.ok('handle_disconnect', 'disconnect kill target')
    ctx.check('disconnected handle can kill original work', ctx.ok('handle_kill', 'kill disconnected') is True and not live.alive())
    if ctx.language == 'typescript':
        for mode in ('disconnect', 'pending'):
            pending = Job(ctx, name=mode, pre_out='', pre_err='', second_gate=True)
            pending.start(wait=0, callback_mode=mode)
            pending.ready()
            pending.release()
            field = 'disconnected' if mode == 'disconnect' else 'active'
            assert ctx.wait_until(lambda: ctx.ok('handle_stats', 'callback state').get(field) is True, 10)
            reply = ctx.call('handle_disconnect')
            assert reply['elapsed'] < 1
            before = ctx.ok('handle_stats', 'callback count')['callbacks']
            ctx.ok('handle_release', 'release user callback')
            pending.release('FINISH')
            terminal(ctx, pending.sid, pending.info['execution_id'])
            after = ctx.ok('handle_stats', 'settled callbacks')
            assert before == after['callbacks'] and not after['unhandled'], after
        ctx.check('disconnect avoids callback deadlocks and new dispatch', True)


def b07(ctx):
    for op in ('handle_kill', 'command_kill'):
        job = Job(ctx, name=op, child=True)
        info = job.start(wait=0)
        job.ready()
        child = json.loads(job.file('child.json'))
        assert job.alive(child)
        params = {'id': job.sid, 'pid': info['pid']} if op == 'command_kill' else {}
        assert ctx.ok(op, 'immediate kill', **params) is True
        assert not job.alive() and not job.alive(child), 'kill acknowledged before group termination'
        assert job.file('TERM') is None, 'graceful cancel used instead of SIGKILL'
        assert ctx.ok(op, 'repeat kill', **params) is False
        reply = wait(ctx)
        assert not reply['ok'] and reply['error'].get('code') == 'canceled', reply
        assert ctx.must_run(job.sid, 'printf alive')['stdout'] == 'alive'
    ctx.check('both kill APIs confirm group death without TERM', True)
    ctx.check('repeat kill is false and sandbox remains usable', True)
    native = Job(ctx, name='native-term', child=True)
    _, ex = raw(ctx, 'POST', f'/v1/sandboxes/{native.sid}/executions',
                {'command': ['/bin/bash', '-l', '-c', native.command]}, 201)
    native.ready()
    raw(ctx, 'POST', f'/v1/sandboxes/{native.sid}/executions/{ex["id"]}/cancel')
    ctx.check('native cancel still sends TERM', native.file('TERM') == 'received')


def b08(ctx):
    run = subprocess.run(['go', 'test', '-json', '-count=1', '-timeout=30s',
        '-tags=e2b_background_acceptance', './internal/dockerbackend',
        '-run', '^TestBackgroundSignalLifetime$'], cwd=ROOT, capture_output=True, text=True, timeout=90)
    (ctx.out / 'backend-lifetime.jsonl').write_text(run.stdout)
    (ctx.out / 'backend-lifetime.stderr.log').write_text(run.stderr)
    ctx.check('backend rejects reused PID at signal dispatch', run.returncode == 0,
        {'fault': 'fake_engine_pid_reuse_at_dispatch', 'fired': 'signal-boundary-reached' in run.stdout,
         'returncode': run.returncode, 'log': 'backend-lifetime.jsonl',
         'scope': 'real backend against modeled Engine; not kernel PID reuse'})
    job = Job(ctx)
    old = job.start(wait=0, handle='old')
    job.ready()
    job.release()
    outcome(wait(ctx, handle='old'))
    replacement = Job(ctx, job.sid, name='replacement')
    current = replacement.start(wait=0)
    replacement.ready()
    assert current['execution_id'] != old['execution_id']
    ctx.check('stale handle cannot kill a new execution',
              ctx.ok('handle_kill', 'stale handle', handle='old') is False and replacement.alive())
    other = ctx.create(duration=60)
    err = ctx.error('command_connect', 'other sandbox', id=other, pid=current['pid'])
    assert err['family'] == 'command_not_found', err
    assert ctx.ok('command_kill', 'missing pid', id=other, pid=current['pid']) is False
    err = ctx.error('command_connect', 'absent pid', id=job.sid, pid=2147483647)
    ctx.check('PID lookup is sandbox scoped and absence explicit', err['family'] == 'command_not_found', err)


def b09(ctx):
    job = Job(ctx, exit=37)
    info = job.start(wait=0)
    job.ready()
    job.release()
    outcome(wait(ctx), 37)
    attached(ctx, job.sid, info['pid'], wait=0)
    first = wait(ctx, handle='attached')
    value = outcome(first, 37)
    before = ctx.local.faults.request_count()
    second = wait(ctx, handle='attached')
    outcome(second, 37)
    ctx.check('completed attach returns real exit without history',
              value['stdout'] == value['stderr'] == '' and first['elapsed'] < 2)
    ctx.check('terminal wait is cached without network or callbacks',
              ctx.local.faults.request_count() == before and no_new_callbacks(second))
    race = Job(ctx, name='attach-race', exit=37)
    info = race.start(wait=0)
    race.ready()
    ctx.ok('handle_disconnect', 'detach before race')
    barrier = Fault('GET', rf'/v1/sandboxes/{race.sid}/commands/{info["pid"]}', 'barrier_after')
    ctx.local.faults.arm('attach-race', barrier)
    ctx.worker.begin('command_connect', id=race.sid, pid=info['pid'], handle='attached',
                     wait=0, callbacks=ctx.language == 'typescript')
    try:
        assert barrier.accepted.wait(10), 'resolve never reached post-snapshot barrier'
        control = fresh(ctx, race.sid)
        race.release(worker=control)
        terminal(ctx, race.sid, info['execution_id'])
    finally:
        barrier.release.set()
    assert ctx.worker.finish()['ok'], 'connect failed after completion race'
    reply = wait(ctx, handle='attached')
    value = outcome(reply, 37)
    ctx.check('completion between resolve and subscribe is not lost', (value['stdout'], value['stderr']) == POST,
              {'fault': 'barrier_after', 'fired': barrier.remaining == 0 and barrier.accepted.is_set()})


def b10(ctx):
    job = Job(ctx)
    info = job.start(wait=0)
    job.ready()
    assert ctx.wait_until(lambda: execution(ctx, job.sid, info['execution_id']).get('stdout') == PRE[0], 5)
    ctx.ok('handle_disconnect', 'detach prefix')
    new = attached(ctx, job.sid, info['pid'], wait=0, callbacks=ctx.language == 'typescript')
    job.release()
    reply = wait(ctx, handle='attached')
    value = outcome(reply)
    ctx.check('fresh attach observes only POST', (value['stdout'], value['stderr']) == POST and callbacks(reply) == POST)
    native = terminal(ctx, job.sid, info['execution_id'])
    ctx.check('native retained logs contain PRE and POST',
              (native['stdout'], native['stderr']) == tuple(a + b for a, b in zip(PRE, POST)))
    records = traffic(ctx, 'GET', '/commands/' + str(info['pid']))
    ctx.check('attach records identity and positive watermark', len(records) == 1
              and records[0]['execution_id'] == new['execution_id'] and records[0]['attach_event_id'] > 0, records)


def b11(ctx):
    for attach in (False, True):
        job = Job(ctx, name='retry-' + str(attach), interval=.08)
        path = rf'/v1/sandboxes/{job.sid}/executions/[^/]+/events'
        if not attach:
            cut, replay = Fault('GET', path, 'cut'), Fault('GET', path, 'replay')
            ctx.local.faults.arm('retry', cut, replay)
        info = job.start(wait=0, callbacks=ctx.language == 'typescript' and not attach)
        job.ready()
        if attach:
            ctx.ok('handle_disconnect', 'detach before retry')
            cut, replay = Fault('GET', path, 'cut'), Fault('GET', path, 'replay')
            ctx.local.faults.arm('retry', cut, replay)
            attached(ctx, job.sid, info['pid'], wait=0, callbacks=ctx.language == 'typescript')
        before = len(traffic(ctx, 'GET', '/commands/' + str(info['pid'])))
        job.release()
        reply = wait(ctx, handle='attached' if attach else 'h')
        value = outcome(reply)
        expected = POST if attach else tuple(a + b for a, b in zip(PRE, POST))
        assert (value['stdout'], value['stderr']) == expected and callbacks(reply) == expected, reply
        assert ctx.wait_until(lambda: bool([r for r in ctx.local.faults.requests('retry', 'GET') if r['fault'] == 'replay']), 5)
        records = ctx.local.faults.requests('retry', 'GET')
        assert cut.remaining == replay.remaining == 0 and any(r.get('partial_frame') for r in records)
        assert any(r['fault'] == 'replay' and r.get('last_event_id') is not None for r in records), records
        assert len(traffic(ctx, 'GET', '/commands/' + str(info['pid']))) == before
        ctx.check(('attach' if attach else 'start') + ' resumes once from delivered event ID', True,
                  {'fault': 'cut+replay', 'fired': True, 'requests': records})
        ctx.ok('kill', 'cleanup retry', id=job.sid)


def b12(ctx):
    job = Job(ctx)
    fault = Fault('POST', rf'/v1/sandboxes/{job.sid}/executions', 'truncate')
    ctx.local.faults.arm('start-loss', fault)
    info = job.start(wait=0)
    job.ready()
    job.release()
    outcome(wait(ctx))
    records = ctx.local.faults.requests('start-loss', 'POST', '/executions')
    ctx.check('accepted loss recovers one immutable invocation', fault.remaining == 0 and len(records) >= 2
              and len({r['body']['idempotency_key'] for r in records}) == 1
              and {r['execution_id'] for r in records} == {info['execution_id']} and job.file('counter') == '1',
              {'fault': 'truncate', 'fired': fault.remaining == 0, 'requests': records})
    uncertain = Job(ctx, name='uncertain')
    control = fresh(ctx, uncertain.sid)
    held = Fault('POST', rf'/v1/sandboxes/{uncertain.sid}/executions', 'hold', remaining=20, delay=3)
    ctx.local.faults.arm('start-held', held)
    ctx.worker.begin('background', id=uncertain.sid, command=uncertain.command, wait=1.25)
    assert held.accepted.wait(10)
    uncertain.ready(control)
    reply = ctx.worker.finish()
    assert not reply['ok'] and reply['error']['family'] == 'timeout' and reply['error'].get('idempotency_key'), reply
    assert ctx.wait_until(lambda: not uncertain.alive(), 4)
    assert uncertain.file('AFTER', control) is None
    assert ctx.wait_until(lambda: bool(ctx.local.faults.requests('start-held', 'POST', '/executions')), 5)
    records = ctx.local.faults.requests('start-held', 'POST', '/executions')
    ctx.check('uncertain start retains key and remote deadline',
              {r['body']['idempotency_key'] for r in records} == {reply['error']['idempotency_key']}
              and all(r['body'].get('timeout_ms') == 1250 for r in records) and uncertain.file('counter', control) == '1',
              {'fault': 'hold', 'fired': held.accepted.is_set(), 'error': reply['error']})


def b13(ctx):
    job = Job(ctx, pre_out='', pre_err='', second_gate=True)
    info = job.start(wait=0, callback_error=ctx.language == 'typescript')
    job.ready()
    job.release()
    if ctx.language == 'typescript':
        assert ctx.wait_until(lambda: any(ctx.ok('handle_stats', 'callback without wait')['callbacks'][stream]
                                         for stream in ('stdout', 'stderr')), 5)
        time.sleep(.05)
    reply = wait(ctx, callback_error=True)
    ctx.check('callback failure preserves original exception', reply.get('value', {}).get('callback_error_preserved') is True, reply)
    before = ctx.local.faults.request_count()
    again = wait(ctx, callback_error=True)
    after = ctx.local.faults.request_count()
    alive = job.alive()
    ctx.check('failed handle cannot retry or cancel', again.get('value', {}).get('callback_error_preserved') is True
              and after == before and no_new_callbacks(again) and alive,
              {'reply': again, 'requests_before': before, 'requests_after': after, 'alive': alive})
    if ctx.language == 'typescript':
        stats = ctx.ok('handle_stats', 'failed callback state')
        ctx.check('callback rejection has no unhandled leak', not stats['unhandled'] and stats['serial'], stats)
    attached(ctx, job.sid, info['pid'], wait=0)
    job.release('FINISH')
    outcome(wait(ctx, handle='attached'))
    ctx.check('new observer can complete original work', bool(job.artifact()))


def b14(ctx):
    for size in (1048576, 1048577):
        sid = ctx.create(duration=120)
        info = start(ctx, sid, f'python3 -c "import sys;sys.stdout.write(chr(120)*{size})"', wait=0)
        reply = wait(ctx)
        if size == 1048576:
            value = outcome(reply)
            ctx.check('exact output boundary succeeds', value['stdout'] == 'x' * size and value['stderr'] == '')
        else:
            assert not reply['ok'] and reply['error'].get('code') == 'output_truncated', reply
            assert reply['error'].get('output_truncated') is True and reply['error'].get('execution_id') == info['execution_id']
            terminal(ctx, sid, info['execution_id'])
            connected = ctx.call('command_connect', id=sid, pid=info['pid'], handle='over', wait=0, quiet=True)
            late = wait(ctx, handle='over') if connected['ok'] else connected
            ctx.check('reattach cannot erase truncation', not late['ok'] and late['error'].get('code') == 'output_truncated', late.get('error'))
        ctx.ok('kill', 'cleanup bounded output', id=sid)
    events = Job(ctx, pre_out='', pre_err='', lines=0, event_steps=4097)
    info = events.start(wait=0)
    events.ready()
    proof = event_overflow(ctx, events, info['execution_id'])
    reply = wait(ctx)
    ctx.check('event overflow stays explicit', not reply['ok']
              and reply['error'].get('code') == 'output_truncated'
              and reply['error'].get('output_truncated') is True, proof)
    ctx.ok('kill', 'cleanup event cap', id=events.sid)
    sid = native_sandbox(ctx)
    active = Job(ctx, sid, name='active-retained')
    active.start(wait=0, handle='active')
    active.ready()
    old = start(ctx, sid, 'true', handle='old', wait=0)
    outcome(wait(ctx, handle='old'))
    for _ in range(201):
        info = start(ctx, sid, 'true', wait=0)
        outcome(wait(ctx))
    status, _ = raw(ctx, 'GET', f'/v1/sandboxes/{sid}/executions/{old["execution_id"]}', expected=None)
    assert status == 404, 'completed handle pinned its execution forever'
    err = ctx.error('command_connect', 'pruned command', id=sid, pid=old['pid'])
    assert err['family'] == 'command_not_found', err
    attached(ctx, sid, active.info['pid'], wait=0)
    assert active.alive()
    assert ctx.ok('handle_kill', 'kill retained active command', handle='attached') is True
    ctx.check('retention prunes completed not active identities', not active.alive())


def b15(ctx):
    job = Job(ctx)
    info = job.start(wait=0)
    job.ready()
    ctx.ok('handle_disconnect', 'detach occupied slot')
    reply = ctx.call('background', id=job.sid, command='echo wrong > /home/user/extra-start', quiet=True)
    ctx.check('detached command retains admission slot', not reply['ok'] and reply['error'].get('code') == 'busy', reply)
    attached(ctx, job.sid, info['pid'], wait=0)
    assert ctx.ok('handle_kill', 'kill occupied slot', handle='attached') is True and not job.alive()
    assert ctx.must_run(job.sid, 'printf released')['stdout'] == 'released'
    ctx.check('attach and kill need no free slot', True)
    sid = native_sandbox(ctx)
    first, second = Job(ctx, sid, name='first'), Job(ctx, sid, name='second')
    first.start(wait=0, handle='first')
    first.ready()
    second.start(wait=0, handle='second')
    second.ready()
    assert ctx.ok('handle_kill', 'kill one sibling', handle='first') is True
    ctx.check('normal kill leaves concurrent sibling alive', not first.alive() and second.alive())
    second.release()
    outcome(wait(ctx, handle='second'))
    second.artifact()
    first, second = Job(ctx, sid, name='fallback-first'), Job(ctx, sid, name='fallback-second')
    first.start(wait=0, handle='first'); first.ready()
    second.start(wait=0, handle='second'); second.ready()
    ctx.local.docker.deny_exec(True)
    try:
        reply = ctx.call('handle_kill', handle='first', quiet=True)
        assert not reply['ok'] and reply['error'].get('execution_id') == first.info['execution_id'], reply
        assert ctx.wait_until(lambda: not ctx.state(sid)['alive'], 15)
        sibling = wait(ctx, handle='second')
        assert not sibling['ok'] and sibling['error'].get('termination_reason') in ('canceled', 'evicted'), sibling
        state = raw(ctx, 'GET', f'/v1/sandboxes/{sid}')[1]
        ctx.check('fallback exposes collateral termination', state['state'] == 'killed'
                  and not first.alive() and not second.alive(),
                  {'fault': 'docker_exec_denied', 'fired': bool(ctx.local.docker.failed_execs),
                   'target': reply['error'], 'sibling': sibling['error'], 'sandbox': state})
    finally:
        ctx.local.docker.deny_exec(False)


def b16(ctx):
    job = Job(ctx)
    info = job.start(wait=0)
    job.ready()
    ctx.ok('timeout', 'shorten sandbox lifetime', id=job.sid, duration=4)
    before = len(mutations(ctx))
    attached(ctx, job.sid, info['pid'], wait=0)
    ctx.ok('handle_disconnect', 'disconnect TTL observer', handle='attached')
    ctx.check('handle operations never renew sandbox TTL', len(mutations(ctx)) == before)
    ctx.check('sandbox expiry ends unlimited command', ctx.wait_until(lambda: not ctx.state(job.sid)['alive'], 10) and not job.alive())
    paused = Job(ctx, name='pause')
    info = paused.start(wait=0)
    paused.ready()
    raw(ctx, 'POST', f'/v1/sandboxes/{paused.sid}/pause')
    reply = ctx.call('command_connect', id=paused.sid, pid=info['pid'], quiet=True)
    state = raw(ctx, 'GET', f'/v1/sandboxes/{paused.sid}')[1]
    ctx.check('command attach never auto-resumes paused sandbox', not reply['ok'] and state['state'] == 'paused', reply)
    raw(ctx, 'POST', f'/v1/sandboxes/{paused.sid}/resume')
    ctx.local.restart()
    reply = ctx.call('command_connect', id=paused.sid, pid=info['pid'], quiet=True)
    ctx.check('restart invalidates identities and cleans owned containers', not reply['ok']
              and reply['error']['family'] == 'sandbox_not_found' and ctx.count() == 0, reply)


def b17(ctx):
    job = Job(ctx)
    info = job.start(wait=0)
    job.ready()
    path = f'/v1/sandboxes/{job.sid}/executions/{info["execution_id"]}/kill'
    fault = Fault('POST', re.escape(path), 'status', remaining=20, status=503)
    ctx.local.faults.arm('kill-denied', fault)
    reply = ctx.call('handle_kill', quiet=True)
    ctx.check('failed kill is uncertainty not success', not reply['ok']
              and reply['error'].get('execution_id') == info['execution_id'] and job.alive(),
              {'fault': 'status', 'fired': fault.remaining < 20, 'reply': reply})
    ctx.local.faults.arm('kill-reply-loss', Fault('POST', re.escape(path), 'truncate', remaining=20))
    reply = ctx.call('handle_kill', quiet=True)
    assert ctx.wait_until(lambda: not job.alive(), 12)
    records = ctx.local.faults.requests('kill-reply-loss', 'POST')
    ctx.check('lost kill acknowledgement preserves identity', not reply['ok']
              and reply['error'].get('execution_id') == info['execution_id']
              and bool(records) and all(r['path'] == path for r in records),
              {'fault': 'truncate', 'fired': any(r.get('truncated_bytes') for r in records), 'reply': reply})
    ctx.local.faults.arm('runtime-kill-failure')
    live = Job(ctx, name='runtime-denied', child=True)
    live.start(wait=0); live.ready()
    child = json.loads(live.file('child.json'))
    before = len(ctx.local.docker.removals())
    ctx.local.docker.deny_exec(True)
    ctx.local.docker.arm(True)
    try:
        reply = ctx.call('handle_kill', quiet=True)
        ctx.check('runtime signal and removal failure remain uncertain', not reply['ok']
                  and reply['error'].get('execution_id') == live.info['execution_id']
                  and live.alive() and live.alive(child),
                  {'fault': 'docker_exec_and_remove_denied',
                   'fired': bool(ctx.local.docker.failed_execs) and len(ctx.local.docker.removals()) > before,
                   'reply': reply})
    finally:
        ctx.local.docker.deny_exec(False)
        ctx.local.docker.arm(False)


def b18(ctx):
    assert ctx.count() == 0, 'old-server check needs an empty owner scope'
    directory = ctx.out / 'old'
    directory.mkdir()
    old = Local(directory, ctx.env['CONTRACT_OWNER'], ctx.env['CONTRACT_OLD_SERVER'], ctx.local.image)
    old.start()
    try:
        worker = ctx.spawn(**old.environment(ctx.language))
        sid = ctx.ok('create', 'old server sandbox', worker, duration=60)['id']
        replies = [ctx.call(op, worker, id=sid, command='echo bad > /home/user/should-not-start', pid=42, quiet=True)
                   for op in ('run', 'background', 'command_connect', 'command_kill')]
        records = [r for r in old.faults.records if r['method'] == 'POST' and r['path'].endswith('/executions')]
        ctx.check('real v0.0.2 rejects corrected commands before mutation',
                  all(not r['ok'] and r['error'].get('code') == 'incompatible_server' for r in replies) and not records, replies)
        original = ctx.local
        try:
            ctx.local = old
            _, ex = raw(ctx, 'POST', f'/v1/sandboxes/{sid}/executions', {'command': ['/bin/sh', '-c', 'printf native']}, 201)
            state = terminal(ctx, sid, ex['id'])
            ctx.check('old native command API still works', state['stdout'] == 'native' and state['exit_code'] == 0)
        finally:
            ctx.local = original
    finally:
        old.close()
    sid = ctx.create(duration=60)
    worker = fresh(ctx, sid)
    fault = Fault('GET', '/v1/capabilities', 'missing_capability', remaining=20)
    ctx.local.faults.arm('missing-capability', fault)
    before = len(mutations(ctx))
    reply = ctx.call('run', worker, id=sid, command='true', quiet=True)
    ctx.check('missing flag fails closed without mutation', not reply['ok']
              and reply['error'].get('code') == 'incompatible_server' and len(mutations(ctx)) == before,
              {'fault': 'missing_capability', 'fired': fault.remaining < 20, 'reply': reply})
    ctx.local.faults.arm('auth')
    key = 'apiKey' if ctx.language == 'typescript' else 'api_key'
    bad = ctx.spawn(CONTRACT_OPTIONS=json.dumps({**json.loads(ctx.env['CONTRACT_OPTIONS']), key: 'wrong-test-key'}))
    err = ctx.error('connect', 'bad credentials', bad, id=sid)
    ctx.check('auth stays distinct from unsupported', err['family'] == 'auth', err)


def b19(ctx):
    sid = ctx.create(duration=120)
    ctx.write(sid, '/home/user/guard', text='unchanged')
    before = len(mutations(ctx))
    pids = (0, -1, True, 1.5, '42') + ((9007199254740992,) if ctx.language == 'typescript' else ())
    for pid in pids:
        for op in ('command_connect', 'command_kill'):
            err = ctx.error(op, 'invalid PID', id=sid, pid=pid)
            assert err['family'] == 'invalid', err
    for options in ({'background': 'true'}, {'stdin': True}, {'stdin': 'text'}, {'retain_output': False},
                    {'unexpected': True}, {'request_timeout': 1}):
        err = ctx.error('background', 'invalid option', id=sid, command='echo changed > /home/user/guard', options=options)
        assert err['family'] == 'invalid', err
    for duration in (-1, 9223372037):
        for op in ('background', 'command_connect'):
            err = ctx.error(op, 'invalid duration', id=sid, command='true', pid=42, wait=duration)
            assert err['family'] == 'invalid', err
    for token in ('nan', 'infinity', '-infinity'):
        for op in ('background', 'command_connect'):
            err = ctx.error(op, 'nonfinite duration', id=sid, command='true', pid=42, nonfinite=token)
            assert err['family'] == 'invalid', err
    if ctx.language == 'python':
        for args in ({'callbacks': True}, {'wait': True}):
            err = ctx.error('background', 'invalid Python option', id=sid, command='true', **args)
            assert err['family'] == 'invalid', err
    else:
        for millis in (1.5, True):
            err = ctx.error('background', 'invalid TS milliseconds', id=sid, command='true', millis=millis)
            assert err['family'] == 'invalid', err
    ctx.check('invalid input is rejected before side effects', len(mutations(ctx)) == before and ctx.read(sid, '/home/user/guard') == 'unchanged')
    _, body = raw(ctx, 'POST', f'/v1/sandboxes/{sid}/executions',
                  {'command': ['true'], 'timeout_ms': 1250, 'timeout_seconds': 2}, 400)
    ctx.check('wire timeout fields are mutually exclusive', body.get('code') == 'invalid_request', body)
    start(ctx, sid, 'true', wait=1.25, options={'stdin': False, 'retain_output': True})
    outcome(wait(ctx))
    ctx.check('supported defaults and fractional seconds normalize', traffic(ctx, 'POST', '/executions')[-1]['body'].get('timeout_ms') == 1250)


def b20(ctx):
    if ctx.language == 'python':
        ctx.ok('handle_exports', 'public CommandHandle')
    else:
        path = ctx.env.get('CONTRACT_BACKGROUND_TYPES')
        evidence = json.loads(Path(path).read_text()) if path else {}
        ctx.check('installed background declarations compile', evidence.get('returncode') == 0, evidence)
    job = Job(ctx, pre_out='', pre_err='')
    job.start(wait=0, callbacks=ctx.language == 'typescript')
    job.ready()
    job.release()
    if ctx.language == 'typescript':
        assert ctx.wait_until(lambda: bool(ctx.ok('handle_stats', 'live fields')['stdout']), 5)
    first = wait(ctx, simultaneous=ctx.language == 'typescript')
    outcome(first)
    before = ctx.local.faults.request_count()
    again = wait(ctx)
    ctx.check('wait shares and caches terminal result', first['value'] == again.get('value')
              and ctx.local.faults.request_count() == before and no_new_callbacks(again))
    ctx.check('callbacks deliver exact serial text', callbacks(first) == POST)
    ctx.ok('handle_disconnect', 'disconnect completed handle')
    ctx.check('cached result survives disconnect', wait(ctx).get('value') == first['value'])
    proofs = []
    for reason in ('oom', 'evicted', 'unknown'):
        sid = native_sandbox(ctx, slots=2 if reason == 'unknown' else 1, memory=48 if reason == 'oom' else 128)
        abnormal = Job(ctx, sid, name=reason, oom=reason == 'oom', exit=137 if reason == 'unknown' else 0)
        abnormal.start(wait=0); abnormal.ready()
        if reason == 'evicted':
            subprocess.run(['docker', 'kill', container(ctx, sid)], check=True, capture_output=True, timeout=20)
        else:
            abnormal.release()
        reply = wait(ctx)
        state = terminal(ctx, sid, abnormal.info['execution_id'])
        assert not reply['ok'] and reply['error']['family'] != 'exit' and reply['error'].get('code') == reason, reply
        assert state['termination_reason'] == reason and state['exit_code'] != 0, state
        assert reply['error'].get('execution_id') == state['id'], reply
        proofs.append({'expected': reason, 'actual': reply['error'], 'native': state})
        ctx.ok('kill', 'cleanup abnormal command', id=sid)
    ctx.check('abnormal outcomes never become ordinary success', True, proofs)


def w04(ctx):
    other = 'typescript' if ctx.language == 'python' else 'python'
    for target in (ctx.language, other):
        for mode in ('success', 'failure', 'timeout', 'kill', 'late'):
            transfer(ctx, target, mode)
        ctx.check(('same' if target == ctx.language else 'cross') + ' language artifact handoff variants', True)


def a12(ctx):
    baseline_a12(ctx, background_invalid=False)


def a14(ctx):
    b12(ctx)
    b11(ctx)


def a16(ctx):
    baseline_a16(ctx)
    b18(ctx)


def w03(ctx):
    sid = ctx.create(duration=120)
    reply = ctx.call('run', id=sid, command=f'python3 -c {src(FIX["counter"])}', callbacks=True)
    ctx.check('long foreground task streams in order',
              reply['value']['stdout'] == ''.join(f'item-{i:02}\n' for i in range(24))
              and ''.join(reply['callbacks']['stdout']) == reply['value']['stdout'] and len(reply['callbacks']['stdout']) >= 4)
    deadline(ctx, background=False)
    ctx.ok('kill', 'explicit cleanup', id=sid)
    ctx.check('foreground workflow explicitly cleans up', not ctx.state(sid)['alive'])


SCENARIOS = {f'B{i:02}': globals()[f'b{i:02}'] for i in range(1, 21)}
SCENARIOS.update(A05=a05, A12=a12, A14=a14, A16=a16, W03=w03, W04=w04)
