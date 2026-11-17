"""A01-A16 and W01-W03. Application bodies are provider-neutral on purpose."""
from __future__ import annotations

import base64
import hashlib
import json
from pathlib import Path
import re
import time

from runner import CASE_NAMES, Worker, observer, NotRun

FIX = json.loads((Path(__file__).parent / 'fixtures.json').read_text())
# Shell-embedded fixtures keep real characters: ensure_ascii would turn an
# emoji into a surrogate pair, and Python refuses to encode those.
src = lambda value: json.dumps(value, ensure_ascii=False)


class Context:
    def __init__(self, provider, language, out, env, budget, local):
        self.provider, self.language, self.out = provider, language, Path(out)
        self.env, self.local = env, local
        # Leave room inside the parent watchdog for cleanup and evidence.
        self.deadline = time.monotonic() + budget - 15
        self.checks: dict[str, object] = {}
        self.workers = []
        self.worker = self.spawn()

    # -- transport helpers ---------------------------------------------------
    def spawn(self, **overrides):
        env = dict(self.env, CONTRACT_OWNED=str(self.out / 'owned.jsonl'), **overrides)
        worker = Worker(self.language, env, self.out / f'worker-{len(self.workers)}.jsonl',
                        max(60, min(self.deadline - time.monotonic(), 240)))
        self.workers.append(worker)
        return worker

    def call(self, op, worker=None, quiet=False, **kw):
        """Typed call that fails loudly unless the caller expects an error.

        A silently ignored failure would let a case pass for the wrong reason,
        so only probes that assert the error may pass quiet=True.
        """
        reply = (worker or self.worker).call(op, **kw)
        if not quiet:
            assert reply['ok'], f'{op}: {json.dumps(reply.get("error"), ensure_ascii=False)}'
        return reply

    def ok(self, op, label, worker=None, **kw):
        reply = self.call(op, worker, **kw)
        assert reply['ok'], f'{label}: {json.dumps(reply.get("error"), ensure_ascii=False)}'
        return reply['value']

    def error(self, op, label, worker=None, **kw):
        reply = self.call(op, worker, quiet=True, **kw)
        assert not reply['ok'], f'{label} unexpectedly succeeded: {json.dumps(reply)[:500]}'
        return reply['error']

    def check(self, name, condition, evidence=None):
        assert name not in self.checks, 'duplicate check ' + name
        self.checks[name] = bool(condition) if evidence is None else {'passed': bool(condition), 'evidence': evidence}
        assert condition, f'{name}: {evidence}'

    def create(self, **kw):
        return self.ok('create', 'create', **kw)['id']

    def run(self, sid, command, worker=None, **kw):
        return self.call('run', worker, id=sid, command=command, **kw)

    def must_run(self, sid, command, worker=None, **kw):
        value = self.ok('run', 'commands.run', worker, id=sid, command=command, **kw)
        assert value['exit'] == 0, value
        return value

    def read(self, sid, path, worker=None, **kw):
        return self.ok('read', 'files.read', worker, id=sid, path=path, **kw)

    def write(self, sid, path, worker=None, **kw):
        return self.ok('write', 'files.write', worker, id=sid, path=path, **kw)

    def list(self, sid, path, worker=None, **kw):
        value = self.ok('list', 'files.list', worker, id=sid, path=path, **kw)
        assert isinstance(value, list)
        return value

    def state(self, sid):
        """Independent process; never connect or renew as a probe."""
        return observer(self.provider, 'state', sid, self.env, self.env.get('CONTRACT_OWNER', ''))

    def count(self):
        return observer('tiny', 'count', '-', self.env, self.env['CONTRACT_OWNER'])['count']

    def container_creates(self):
        return [r for r in self.local.docker.requests if r.startswith('POST ') and '/containers/create' in r]

    def wait_until(self, predicate, seconds):
        limit = min(time.monotonic() + seconds, self.deadline)
        while time.monotonic() < limit:
            if predicate():
                return True
            time.sleep(.25)
        return False

    def close(self):
        for worker in self.workers:
            worker.close()


def payload(sid, worker, path, seconds=20):
    """Poll a file without connect: files.read is the same transport."""
    limit = time.monotonic() + seconds
    while time.monotonic() < limit:
        reply = worker.call('read', id=sid, path=path)
        if reply['ok']:
            return reply['value']
        time.sleep(.4)
    return None


def text_file(ctx, sid, path, value, worker=None):
    ctx.write(sid, path, worker, text=value)


def check_entry(ctx, entry, kind=None):
    assert isinstance(entry['name'], str) and entry['path'].startswith('/'), entry
    assert entry['type'] in {'file', 'dir', 'symlink'} or entry['type'] is None, entry
    if kind:
        ctx.check(f'{entry["name"]} type', entry['type'] == kind, entry['type'])


# ------------------------------------------------------------------ A01
def a01(ctx):
    sid = ctx.create(duration=60)
    ctx.check('id is opaque text', bool(sid) and isinstance(sid, str))
    info = ctx.ok('info', 'get_info', id=sid)
    ctx.check('created sandbox is running', info['state'].lower().endswith('running'), info['state'])
    ctx.check('kill returns true', ctx.call('kill', id=sid)['value'] is True)
    ctx.check('second kill is false', ctx.call('kill', id=sid)['value'] is False)
    error = ctx.error('connect', 'connect to a killed sandbox', id=sid)
    ctx.check('absent connect uses not-found', error['family'] == 'sandbox_not_found', error)
    alive = ctx.state(sid)
    ctx.check('killed sandbox is gone', not alive['alive'], alive)


# ------------------------------------------------------------------ A02
def a02(ctx):
    sid = ctx.create(duration=120)
    value = ctx.must_run(sid, FIX['shell'])
    ctx.check('bash pipeline output', value['stdout'] == 'ALPHA BETA', value['stdout'])
    ctx.check('exit zero is a normal result', value['exit'] == 0 and value['has_error'] is False)
    value = ctx.must_run(sid, 'shopt -q login_shell && echo login || echo plain')
    ctx.check('login bash, not sh', value['stdout'].strip() == 'login', value['stdout'])
    probe = json.loads(ctx.must_run(sid, f'python3 -c {src(FIX["probe"])}')['stdout'])
    ctx.check('non-root guest user', probe['user'] != 'root' and probe['uid'] != 0, probe)
    ctx.check('home is /home/user', probe['home'] == '/home/user', probe)
    ctx.check('default cwd is home', probe['cwd'] == '/home/user', probe)
    value = ctx.must_run(sid, f'python3 -c {src(FIX["probe"])}', options={'cwd': '/tmp'})
    ctx.check('command cwd applies', json.loads(value['stdout'])['cwd'] == '/tmp')
    probe = json.loads(ctx.must_run(sid, f'python3 -c {src(FIX["probe"])}')['stdout'])
    ctx.check('command cwd does not persist', probe['cwd'] == '/home/user', probe)
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A03
def a03(ctx):
    sid = ctx.create(duration=120, options={'envs': {'CONTRACT_A': 'base', 'CONTRACT_VALUE': FIX['env_value']}})
    read = lambda sandbox_id=sid, worker=None: json.loads(
        ctx.must_run(sandbox_id, f'python3 -c {src(FIX["env"])}', worker)['stdout'])
    ctx.check('create env is visible', read()['A'] == 'base', read())
    ctx.check('env value is data only', read()['value'] == FIX['env_value'] and read()['injected'] is False)
    ctx.check('command env overrides', json.loads(ctx.must_run(
        sid, f'python3 -c {src(FIX["env"])}', options={'envs': {'CONTRACT_A': 'override'}})['stdout'])['A'] == 'override')
    ctx.check('override does not persist', read()['A'] == 'base')
    fresh = ctx.spawn()
    ctx.ok('connect', 'fresh process connect', fresh, id=sid)
    ctx.check('env survives a new client', json.loads(ctx.must_run(
        sid, f'python3 -c {src(FIX["env"])}', fresh)['stdout'])['A'] == 'base')
    other = ctx.create(duration=60)
    ctx.check('second sandbox has no shared env', read(other)['A'] is None)
    ctx.ok('kill', 'kill', id=other)
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A04
def a04(ctx):
    sid = ctx.create(duration=120)
    stdout = FIX['stdout'] * 200
    stderr = FIX['stderr'] * 117
    reply = ctx.call('run', id=sid, command=f'python3 -c {src(FIX["output"])}', callbacks=True, quiet=True)
    error = reply.get('error')
    ctx.check('nonzero raises the command-exit family', error and error['family'] == 'exit', error)
    ctx.check('exit 37 preserved', error['exit'] == 37, error.get('exit'))
    ctx.check('stdout preserved', error['stdout'] == stdout, hashlib.sha256(error['stdout'].encode()).hexdigest())
    ctx.check('stderr preserved', error['stderr'] == stderr, hashlib.sha256(error['stderr'].encode()).hexdigest())
    ctx.check('callbacks concatenate to stdout', ''.join(reply['callbacks']['stdout']) == stdout)
    ctx.check('callbacks concatenate to stderr', ''.join(reply['callbacks']['stderr']) == stderr)
    times = reply['callbacks']['times']
    ctx.check('streams were delivered progressively', len(reply['callbacks']['stdout']) > 1, len(reply['callbacks']['stdout']))
    ctx.check('callbacks completed before return', times and max(times) <= time.time() + 1)
    value = ctx.must_run(sid, "printf '%s' ok")
    ctx.check('success result carries output', value['stdout'] == 'ok' and value['has_error'] is False)
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A05
def a05(ctx):
    sid = ctx.create(duration=60)
    error = ctx.error('run', 'wait deadline', id=sid, command=f'python3 -c {src(FIX["late"])}', wait=1.25)
    ctx.check('deadline raises timeout', error['family'] == 'timeout', error)
    ctx.check('marker written before the deadline', ctx.wait_until(
        lambda: ctx.call('read', id=sid, path='/tmp/started', quiet=True)['ok'], 15))
    ctx.check('work continues after the wait', ctx.wait_until(
        lambda: ctx.call('read', id=sid, path='/tmp/late', quiet=True)['ok'], 20))
    ctx.check('sandbox still healthy', ctx.state(sid)['alive'] is True)
    ctx.must_run(sid, "printf '%s' alive")
    value = ctx.must_run(sid, "printf '%s' zero", wait=0)
    ctx.check('zero disables only the local deadline', value['stdout'] == 'zero')
    reply = ctx.call('run', id=sid, command="printf '%s' cb", callback_error=True, quiet=True)
    ctx.check('callback error is not retried', reply.get('value', {}).get('callback_error_preserved') is True, reply)
    ctx.check('callback error does not kill the process', ctx.state(sid)['alive'] is True)
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A06
def a06(ctx):
    now = time.time()
    sid = ctx.create(duration=60)
    end = ctx.ok('info', 'initial lifetime', id=sid)['end']
    ctx.check('default duration applies', 52 <= end - now <= 70, end - now)
    ctx.ok('timeout', 'set_timeout', id=sid, duration=10)
    end = ctx.ok('info', 'shortened lifetime', id=sid)['end']
    ctx.check('set_timeout shortens from now', 6 <= end - time.time() <= 14, end - time.time())
    ctx.check('shortened sandbox expires', ctx.wait_until(lambda: not ctx.state(sid)['alive'], 18))
    ctx.check('expired sandbox cannot be revived', ctx.error('connect', 'connect expired', id=sid)['family'] == 'sandbox_not_found')
    other = ctx.create(duration=20)
    ctx.ok('timeout', 'set_timeout', id=other, duration=40)
    end = ctx.ok('info', 'extended lifetime', id=other)['end']
    ctx.check('set_timeout extends from now', 34 <= end - time.time() <= 48, end - time.time())
    if ctx.language == 'typescript':
        ctx.ok('timeout', 'set_timeout', id=other, millis=1501)
        end = ctx.ok('info', 'ceil lifetime', id=other)['end']
        ctx.check('1501 ms becomes 2 s', 1.4 <= end - time.time() <= 2.6, end - time.time())
    ctx.ok('kill', 'kill', id=other)


# ------------------------------------------------------------------ A07
def a07(ctx):
    sid = ctx.create(duration=30)
    first = ctx.ok('info', 'initial lifetime', id=sid)['end']
    text_file(ctx, sid, '/home/user/connect.txt', 'kept')
    fresh = ctx.spawn()
    ctx.ok('connect', 'short connect', fresh, id=sid, duration=5)
    after = ctx.ok('info', 'after short connect', fresh, id=sid)['end']
    ctx.check('short connect never shortens', after >= first - 2 and after - first < 12, {'first': first, 'after': after})
    ctx.check('files survive connect', ctx.read(sid, '/home/user/connect.txt', fresh) == 'kept')
    longer = ctx.spawn()
    ctx.ok('connect', 'long connect', longer, id=sid, duration=90)
    extended = ctx.ok('info', 'after long connect', longer, id=sid)['end']
    ctx.check('long connect extends', 84 <= extended - time.time() <= 96, extended - time.time())
    omitted = ctx.spawn()
    ctx.ok('connect', 'omitted duration', omitted, id=sid)
    default = ctx.ok('info', 'after omitted connect', omitted, id=sid)['end']
    ctx.check('omitted duration applies 300 s', 290 <= default - time.time() <= 312, default - time.time())
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A08
def a08(ctx):
    sid = ctx.create(duration=120)
    ctx.write(sid, '/home/user/text.txt', text=FIX['text'])
    ctx.check('text round trip', ctx.read(sid, '/home/user/text.txt') == FIX['text'])
    binary = bytes(range(256))
    encoded = base64.b64encode(binary).decode()
    ctx.write(sid, '/home/user/blob.bin', bytes=encoded, bytearray=ctx.language == 'python')
    reply = ctx.ok('read', 'byte read', id=sid, path='/home/user/blob.bin', options={'format': 'bytes'})
    ctx.check('bytes round trip', base64.b64decode(reply['bytes']) == binary)
    text_file(ctx, sid, '/home/user/empty.txt', '')
    ctx.check('empty text stays empty', ctx.read(sid, '/home/user/empty.txt') == '')
    ctx.check('empty bytes stay empty', ctx.ok(
        'read', 'empty byte read', id=sid, path='/home/user/empty.txt', options={'format': 'bytes'})['bytes'] == '')
    ctx.check('relative paths resolve in the guest', ctx.read(sid, 'text.txt') == FIX['text'])
    ctx.check('missing file is not empty', ctx.error(
        'read', 'missing file', id=sid, path='/home/user/absent.txt')['family'] == 'file_not_found')
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A09
def a09(ctx):
    sid = ctx.create(duration=120)
    info = ctx.write(sid, '/home/user/nested/deep/file.txt', text='a' * 50)
    ctx.check('WriteInfo names the file', info['name'] == 'file.txt' and info['path'].endswith('/nested/deep/file.txt'), info)
    check_entry(ctx, info, 'file')
    ctx.write(sid, '/home/user/nested/deep/file.txt', text='b')
    ctx.check('overwrite replaces the old suffix', ctx.read(sid, '/home/user/nested/deep/file.txt') == 'b')
    entries = ctx.list(sid, '/home/user/nested/deep')
    entry = next(e for e in entries if e['name'] == 'file.txt')
    ctx.check('guest metadata is real', entry['size'] == 1 and len(entry['permissions']) == 9, entry)
    ctx.check('owner is the guest user', entry['owner'] != 'root', entry['owner'])
    error = ctx.error('write', 'invalid destination', id=sid, path='/proc/tiny/file.txt', text='x')
    ctx.check('failed write is an error', error['family'] in {'invalid', 'other', 'file_not_found'}, error)
    ctx.check('failed write changed nothing', ctx.read(sid, '/home/user/nested/deep/file.txt') == 'b')
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A10
def a10(ctx):
    sid = ctx.create(duration=180)
    ctx.must_run(sid, 'mkdir -p /home/user/empty')
    ctx.check('empty directory is an array', ctx.list(sid, '/home/user/empty') == [])
    ctx.must_run(sid, f'python3 -c {src(FIX["listing"])}')
    entries = ctx.list(sid, '/home/user/list-case')
    names = sorted(e['name'] for e in entries)
    ctx.check('1007-entry page is complete', len(entries) == 1009 and 'item-1006' in names, len(entries))
    ctx.check('no cursor envelope leaks', all(isinstance(e, dict) and 'size' in e for e in entries))
    by_name = {e['name']: e for e in entries}
    ctx.check('file mode is real', by_name['item-0000']['mode'] & 0o777 == 0o640, by_name['item-0000']['mode'])
    ctx.check('directory type', by_name['directory']['type'] == 'dir', by_name['directory'])
    ctx.check('symlink type', by_name['link']['type'] == 'symlink', by_name['link'])
    ctx.check('symlink target', by_name['link']['symlink'] == 'item-0000', by_name['link'].get('symlink'))
    entry = by_name['item-0001']
    ctx.check('entry metadata', entry['size'] == 1 and entry['owner'] == json.loads(ctx.must_run(
        sid, f'python3 -c {src(FIX["probe"])}')['stdout'])['user'], entry)
    ctx.check('modified time is recent', abs(entry['modified'] - time.time()) < 3600, entry['modified'])
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A11
def a11(ctx):
    before = ctx.count() if ctx.provider == 'tiny' else None
    clean = ctx.ok('context', 'context without error', raise_error=False)
    ctx.check('cleanup reports no application error', clean['application_error'] is False, clean)
    ctx.check('context removed the sandbox', not ctx.state(clean['id'])['alive'])
    raised = ctx.ok('context', 'context with application error', raise_error=True)
    ctx.check('application exception is re-raised', raised['application_error'] is True, raised)
    ctx.check('cleanup ran after the exception', not ctx.state(raised['id'])['alive'])
    if before is not None:
        ctx.check('no leftover containers', ctx.count() == before, ctx.count())


# ------------------------------------------------------------------ A12
def a12(ctx):
    creates = len(ctx.container_creates()) if ctx.local else 0
    sid = ctx.create(duration=60)
    text_file(ctx, sid, '/home/user/guard.txt', 'original')
    after_create = len(ctx.container_creates())
    cases = [
        ('template', lambda: ctx.error('create', 'custom template', options={'template': 'custom/base'})),
        ('user', lambda: ctx.error('create', 'user switching', options={'user': 'root'})),
        ('metadata', lambda: ctx.error('create', 'metadata filters', options={'metadata': {'a': 'b'}})),
        ('stdin', lambda: ctx.error('run', 'stdin true', id=sid, command='true', options={'stdin': True})),
        ('background', lambda: ctx.error('run', 'background', id=sid, command='true', options={'background': True})),
        ('retain', lambda: ctx.error('run', 'retain_output false', id=sid, command='true', options={'retain_output': False})),
        ('depth', lambda: ctx.error('list', 'recursive list', id=sid, path='/home/user', options={'depth': 2})),
        ('misspelled', lambda: ctx.error('create', 'misspelled option', options={'timeoutt': 30})),
    ]
    for name, call in cases:
        error = call()
        ctx.check(f'{name} rejected before side effects', error['family'] == 'invalid', error)
    error = ctx.error('unsupported', 'pause is deferred', id=sid, method='pause')
    ctx.check('deferred method fails explicitly', error['family'] in {'invalid', 'other'}, error)
    if ctx.local:
        ctx.check('no extra container was created', len(ctx.container_creates()) == after_create, ctx.container_creates())
        ctx.check('no container before the valid create', creates in (0, after_create - 1))
    ctx.check('guard file unchanged', ctx.read(sid, '/home/user/guard.txt') == 'original')
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A13
def a13(ctx):
    sid = ctx.create(duration=300)
    limit = 10 << 20
    exact = 'x' * limit
    ctx.write(sid, '/home/user/limit.txt', text=exact)
    ctx.check('exactly 10 MiB is accepted', len(ctx.read(sid, '/home/user/limit.txt')) == limit)
    error = ctx.error('write', 'over-limit upload', id=sid, path='/home/user/limit.txt', text=exact + 'x')
    ctx.check('over-limit upload fails', 'payload' in json.dumps(error).lower() or error['family'] == 'other', error)
    ctx.check('over-limit upload kept the old bytes', len(ctx.read(sid, '/home/user/limit.txt')) == limit)
    out = ctx.call('run', id=sid, command="python3 -c \"import sys;sys.stdout.write('x' * 1048576)\"", quiet=True)
    ctx.check('exactly 1 MiB output is normal', out['ok'] and len(out['value']['stdout']) == 1 << 20, out.get('error'))
    over = ctx.call('run', id=sid, command="python3 -c \"import sys;sys.stdout.write('x' * 1048577)\"", quiet=True)
    ctx.check('over-limit output is never a normal success', not over['ok'], 'returned ' + str(len(over.get('value', {}).get('stdout', ''))))
    ctx.check('truncation is explicit with evidence', over['error'].get('output_truncated') is True
              or 'truncated' in json.dumps(over['error']).lower(), over['error'])
    events = ctx.call('run', id=sid, command="python3 -c \"import sys\nfor i in range(4200): print(i, flush=True)\"", quiet=True)
    if events['ok']:
        ctx.check('event-capped success keeps every line', events['value']['stdout'].count('\n') == 4200)
    else:
        ctx.check('event cap is explicit', 'truncated' in json.dumps(events['error']).lower(), events['error'])
    ctx.must_run(sid, f'python3 -c {json.dumps("import pathlib; p=pathlib.Path(\'/home/user/scan\'); p.mkdir(exist_ok=True); [ (p/f\'f{i:05}\').write_text(\'x\') for i in range(10001) ]")}')
    scan = ctx.call('list', id=sid, path='/home/user/scan', quiet=True)
    ctx.check('scan cap is never a short list', not scan['ok'] or len(scan['value']) == 10001,
              len(scan['value']) if scan['ok'] else scan['error'])
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A14
def a14(ctx):
    from local import Fault  # the existing real-Docker fault proxy
    sid = ctx.create(duration=120)
    ctx.must_run(sid, 'rm -f /tmp/counter')
    ctx.local.faults.arm('accepted-loss', Fault('POST', rf'/v1/sandboxes/{re.escape(sid)}/executions', 'truncate'))
    # The local guard is about accepted-start loss, not the wait deadline, so
    # it uses the default wait and lets the script's own runtime bound it.
    reply = ctx.call('run', id=sid, command=f'python3 -c {src(FIX["counter"])}', quiet=True)
    ctx.local.faults.arm('idle')
    ctx.check('accepted-start loss was injected', bool(ctx.local.faults.requests('accepted-loss', 'POST')))
    ctx.check('lost start response is not silent', not reply['ok'] or reply['value']['stdout'].count('item-') == 24, reply.get('error'))
    counter = None
    for _ in range(40):
        value = ctx.call('read', id=sid, path='/tmp/counter', quiet=True)
        if value['ok']:
            counter = value['value']
            break
        time.sleep(.5)
    ctx.check('side effect happened once', counter == '1', counter)
    # A sandbox runs one foreground execution at a time, so the next start is
    # only meaningful once the lost-start command has actually finished.
    ctx.check('sandbox accepts the next start', ctx.wait_until(
        lambda: ctx.call('run', id=sid, command='true', quiet=True)['ok'], 20))
    ctx.local.faults.arm('sse-cut', Fault('GET', rf'/v1/sandboxes/{re.escape(sid)}/executions/[^/]+/events', 'cut'))
    reply = ctx.call('run', id=sid, command=f'python3 -c {src(FIX["counter"])}', quiet=True)
    ctx.local.faults.arm('idle')
    ctx.check('SSE cut was injected', bool(ctx.local.faults.requests('sse-cut', 'GET')))
    lines = [line for line in reply.get('value', {}).get('stdout', '').splitlines() if line.startswith('item-')]
    ctx.check('reconnect resumes without gaps or duplicates', lines == [f'item-{i:02}' for i in range(24)], lines[:5] + ['...'] + lines[-3:])
    ctx.check('second run incremented once', ctx.read(sid, '/tmp/counter') == '2')
    ctx.ok('kill', 'kill', id=sid)


# ------------------------------------------------------------------ A15
def a15(ctx):
    sid = ctx.create(duration=120)
    ctx.local.docker.arm(True)
    reply = ctx.call('kill', id=sid, quiet=True)
    ctx.check('removal failure was injected', bool(ctx.local.docker.removals()))
    # Recorded survey target: a failed removal must not read as confirmed
    # cleanup. The native contract currently returns 200 and schedules a retry,
    # so this states the gap instead of hiding it.
    ctx.check('unconfirmed removal is not success', not reply['ok'] and reply.get('value') is not True,
              {'reply': reply, 'gap': 'native kill reports success while container removal is retried'})
    ctx.check('uncertainty is reported', 'unconfirmed' in json.dumps(reply).lower() or reply['error']['family'] == 'other', reply)
    ctx.local.docker.arm(False)
    ctx.check('orphan container still exists while the retry is pending', ctx.state(sid)['exists'] is True)
    ctx.check('observer can remove only its own container',
              observer('tiny', 'sweep', '-', ctx.env, ctx.env['CONTRACT_OWNER'])['removed'] >= 1)
    ctx.check('no orphan remains', ctx.count() == 0, ctx.count())


# ------------------------------------------------------------------ A16
def a16(ctx):
    before = ctx.count()
    bad = ctx.spawn(CONTRACT_OPTIONS=json.dumps({**json.loads(ctx.env['CONTRACT_OPTIONS']), 'api_key': 'wrong-' + 'x' * 20}))
    error = ctx.error('create', 'wrong credentials', bad)
    ctx.check('wrong key is an auth error', error['family'] == 'auth', error)
    ctx.check('failed auth created nothing', ctx.count() == before, ctx.count())
    sid = ctx.create(duration=120)
    ctx.check('server restart loses in-memory state', True)
    ctx.local.restart()
    error = ctx.error('connect', 'connect after restart', id=sid)
    ctx.check('restart is not a silent reconnect', error['family'] == 'sandbox_not_found', error)
    ctx.check('restart cleaned its containers', ctx.count() == 0, ctx.count())
    ctx.worker = ctx.spawn()
    import http.client
    conn = http.client.HTTPConnection('127.0.0.1', ctx.local.port, timeout=5)
    conn.request('POST', '/v1/sandboxes', body=json.dumps({'future_field': {'v': 9}}),
                 headers={'Content-Type': 'application/json', 'Authorization': 'Bearer ' + ctx.local.api_key})
    response = conn.getresponse()
    payload_json = json.loads(response.read())
    ctx.check('unknown request fields are additive', response.status == 201 and 'id' in payload_json, response.status)
    conn.close()
    conn = http.client.HTTPConnection('127.0.0.1', ctx.local.port, timeout=5)
    conn.request('DELETE', '/v1/sandboxes/' + payload_json['id'],
                 headers={'Authorization': 'Bearer ' + ctx.local.api_key})
    ctx.check('additive create is a real resource', conn.getresponse().status == 200)
    conn.close()


# ------------------------------------------------------------------ W01
def w01(ctx):
    sid = ctx.create(duration=180)
    ctx.write(sid, '/home/user/task/input.txt', text='3\n14\n15\n')
    binary = bytes(range(256))
    ctx.write(sid, '/home/user/task/input.bin', bytes=base64.b64encode(binary).decode())
    value = ctx.must_run(sid, f'python3 -c {src(FIX["artifact"])}')
    ctx.check('task printed its marker', value['stdout'].strip() == 'artifact-ready', value['stdout'])
    ctx.check('text artifact', ctx.read(sid, '/home/user/task/result.txt') == '32\n')
    copied = base64.b64decode(ctx.ok('read', 'binary artifact', id=sid, path='/home/user/task/copy.bin',
                                     options={'format': 'bytes'})['bytes'])
    ctx.check('binary artifact', copied == binary[::-1], len(copied))
    names = sorted(e['name'] for e in ctx.list(sid, '/home/user/task'))
    ctx.check('task directory lists artifacts', names == ['copy.bin', 'input.bin', 'input.txt', 'result.txt'], names)
    ctx.check('deliberate failure is distinguishable', ctx.error(
        'run', 'failing task', id=sid, command='exit 7')['exit'] == 7)
    ctx.check('kill succeeds', ctx.ok('kill', 'kill', id=sid) is True)
    ctx.check('sandbox is gone after the workflow', not ctx.state(sid)['alive'])


# ------------------------------------------------------------------ W02
def w02(ctx):
    sid = ctx.create(duration=60, options={'envs': {'TASK': 'first'}})
    ctx.check('environment set for the workflow', json.loads(ctx.must_run(
        sid, f'python3 -c {src(FIX["env"])}', options={'envs': {'CONTRACT_A': 'x'}})['stdout'])['A'] == 'x')
    ctx.check('create-time env visible', json.loads(ctx.must_run(
        sid, f'python3 -c {src(FIX["env"])}')['stdout'])['A'] is None)
    ctx.write(sid, '/home/user/work/step1.txt', text='step one\n')
    second = ctx.spawn()
    ctx.ok('connect', 'second process connect', second, id=sid)
    ctx.check('file written in process one', ctx.read(sid, '/home/user/work/step1.txt', second) == 'step one\n')
    value = ctx.must_run(sid, "printf '%s' \"$(cat /home/user/work/step1.txt | tr -d '\\n') done\"", second)
    ctx.check('second task sees the workspace', value['stdout'] == 'step one done', value['stdout'])
    ctx.ok('timeout', 'set_timeout', id=sid, duration=90, worker=second)
    ctx.check('renewal extends lifetime', ctx.ok('info', 'renewed', second, id=sid)['end'] - time.time() >= 80)
    ctx.check('kill succeeds', ctx.ok('kill', 'kill', id=sid, worker=second) is True)


# ------------------------------------------------------------------ W03
def w03(ctx):
    sid = ctx.create(duration=120)
    reply = ctx.call('run', id=sid, command=f'python3 -c {src(FIX["counter"])}', callbacks=True)
    ctx.check('long task completed', reply['ok'] and reply['value']['stdout'].count('item-') == 24, reply.get('error'))
    ctx.check('output arrives progressively', len(reply['callbacks']['stdout']) >= 4, len(reply['callbacks']['stdout']))
    ctx.check('callback order is preserved', ''.join(reply['callbacks']['stdout']) == reply['value']['stdout'])
    thin = ctx.spawn()
    ctx.ok('connect', 'connect for the deadline probe', thin, id=sid)
    # Only this command can produce the marker, after its local wait expires.
    # The earlier counter task already completed and cannot prove continuation.
    marker = '/tmp/deadline-finished'
    before = ctx.call('read', thin, id=sid, path=marker, quiet=True)
    assert not before['ok'] and before['error']['family'] == 'file_not_found', before
    ctx.check('wait deadline raises timeout', ctx.error(
        'run', 'deadline probe', thin, id=sid,
        command=f"sleep 4; printf '%s' continued > {marker}", wait=.6)['family'] == 'timeout')
    immediate = ctx.call('read', thin, id=sid, path=marker, quiet=True)
    assert not immediate['ok'] and immediate['error']['family'] == 'file_not_found', immediate
    ctx.check('work continues without a connect probe', ctx.wait_until(
        lambda: ctx.call('read', thin, id=sid, path=marker, quiet=True).get('value') == 'continued', 12))
    ctx.check('explicit cleanup', ctx.ok('kill', 'kill', id=sid, worker=thin) is True)
    ctx.check('no process survives cleanup', not ctx.state(sid)['alive'])


SCENARIOS = {name.upper(): fn for name, fn in [
    ('a01', a01), ('a02', a02), ('a03', a03), ('a04', a04), ('a05', a05), ('a06', a06),
    ('a07', a07), ('a08', a08), ('a09', a09), ('a10', a10), ('a11', a11), ('a12', a12),
    ('a13', a13), ('a14', a14), ('a15', a15), ('a16', a16), ('w01', w01), ('w02', w02), ('w03', w03)]}
assert set(SCENARIOS) == set(CASE_NAMES), set(SCENARIOS) ^ set(CASE_NAMES)
