"""Raw-wire and CLI stdin gate against the runner's real Docker daemon."""
import hashlib
import http.client
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from urllib.parse import urlsplit
import uuid

out = Path(sys.argv[1])
base = urlsplit(os.environ['SANDBOX_DOMAIN'])
checks = []
records = []
secret = 'unprinted-input-' + uuid.uuid4().hex
sources = [Path('scripts/stdin-e2e/run.py'), Path('scripts/e2e-stdin.sh'), Path('sandbox-go/live_stdin_test.go'), out / 'sandboxd', out / 'sandboxctl']
(out / 'stdin-source-sha256.json').write_text(json.dumps({str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in sources}, indent=2) + '\n')



def require(name, condition, **evidence):
    checks.append({'name': name, 'passed': bool(condition), **evidence})
    (out / 'raw-results.json').write_text(json.dumps(checks, indent=2) + '\n')
    if not condition:
        raise AssertionError(name)


def request(method, path, payload=None, raw=None):
    body = raw if raw is not None else (None if payload is None else json.dumps(payload).encode())
    conn = http.client.HTTPConnection(base.hostname, base.port, timeout=15)
    try:
        conn.request(method, path, body=body, headers={'Content-Type': 'application/json'})
        response = conn.getresponse()
        data = response.read()
        records.append({'method': method, 'path': path, 'status': response.status,
                        'request_bytes': 0 if body is None else len(body), 'response_bytes': len(data),
                        'response_sha256': hashlib.sha256(data).hexdigest()})
        (out / 'raw-requests.json').write_text(json.dumps(records, indent=2) + '\n')
        return response.status, data
    finally:
        conn.close()


def decoded(raw):
    return json.loads(raw)


def complete(path, execution_id):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        status, raw = request('GET', path + '/' + execution_id)
        require('execution lookup status', status == 200)
        value = decoded(raw)
        if value['state'] == 'completed':
            return value, raw
        time.sleep(.05)
    raise AssertionError('execution did not finish within twelve seconds')


def no_input_field(value):
    if isinstance(value, dict):
        return 'stdin' not in value and all(no_input_field(v) for v in value.values())
    if isinstance(value, list):
        return all(no_input_field(v) for v in value)
    return True


def reject(name, path, payload=None, raw=None):
    status, body = request('POST', path, payload, raw)
    require(name, status == 413 and decoded(body).get('code') == 'payload_too_large', status=status)


status, body = request('POST', '/v1/sandboxes', {})
require('raw fixture created', status == 201)
sandbox_id = decoded(body)['id']
path = '/v1/sandboxes/' + sandbox_id + '/executions'
try:
    status, start = request('POST', path, {'command': ['true'], 'stdin': secret})
    require('raw start accepted', status == 201)
    execution_id = decoded(start)['id']
    done, get = complete(path, execution_id)
    status, listing = request('GET', path)
    require('raw list accepted', status == 200)
    status, sse = request('GET', path + '/' + execution_id + '/events')
    require('raw SSE complete', status == 200 and b'event: complete' in sse)
    for name, raw in [('start', start), ('get', get), ('list', listing), ('sse', sse)]:
        values = [decoded(line[6:]) for line in raw.splitlines() if line.startswith(b'data: ')] if name == 'sse' else [decoded(raw)]
        require('wire privacy ' + name, secret.encode() not in raw and all(no_input_field(v) for v in values))
        (out / ('privacy-' + name + ('.sse' if name == 'sse' else '.json'))).write_bytes(raw)
    for label, payload in [('omitted', {'command': ['cat'], 'timeout_seconds': 2}),
                           ('empty', {'command': ['cat'], 'stdin': '', 'timeout_seconds': 2}),
                           ('escaped maximum', {'command': ['wc', '-c'], 'stdin': '\0' * (1 << 20), 'timeout_seconds': 5})]:
        status, raw = request('POST', path, payload)
        require(label + ' accepted', status == 201)
        result, _ = complete(path, decoded(raw)['id'])
        want = '1048576' if label == 'escaped maximum' else ''
        require(label + ' EOF and exact length', result['termination_reason'] == 'exited' and result['exit_code'] == 0 and result['stdout'].strip() == want)
    reject('decoded stdin over limit', path, {'command': ['true'], 'stdin': 'x' * ((1 << 20) + 1)})
    reject('serialized execution body over limit', path, raw=b'{"command":["true"],"padding":"' + b'x' * (8 << 20) + b'"}')
    reject('serialized nonexecution body over limit', '/v1/sandboxes', raw=b'{"padding":"' + b'x' * (1 << 20) + b'"}')
    reject('command aggregate over limit', path, {'command': ['true', 'x' * (1 << 20)]})
    status, raw = request('GET', path)
    require('rejected requests create no execution', status == 200 and len(decoded(raw)['executions']) == 4)
finally:
    request('DELETE', '/v1/sandboxes/' + sandbox_id)

cli = str(out / 'sandboxctl')
fixture = 'CLI 世界\n'.encode() + b'\0literal $HOME\n\n'
(out / 'cli-input.bin').write_bytes(fixture)
result = subprocess.run([cli, 'run', '--rm', '--stdin-file', str(out / 'cli-input.bin'), '--', 'cat'], capture_output=True, timeout=30)
(out / 'cli-exact.out').write_bytes(result.stdout)
(out / 'cli-exact.err').write_bytes(result.stderr)
require('CLI exact bytes', result.returncode == 0 and result.stdout == fixture, bytes=len(fixture))
subprocess.run(['cmp', str(out / 'cli-input.bin'), str(out / 'cli-exact.out')], check=True)
for name, content in [('invalid-utf8', b'\xff'), ('oversized', b'x' * ((1 << 20) + 1))]:
    before = decoded(request('GET', '/v1/sandboxes')[1])
    input_path = out / ('cli-' + name + '.bin')
    input_path.write_bytes(content)
    result = subprocess.run([cli, 'run', '--stdin-file', str(input_path), '--', 'cat'], capture_output=True, timeout=10)
    after = decoded(request('GET', '/v1/sandboxes')[1])
    require('CLI rejects ' + name + ' before create', result.returncode == 2 and before == after, exit_code=result.returncode)
for flag, value in [('--exec-timeout', '-1'), ('--memory', '64')]:
    before = decoded(request('GET', '/v1/sandboxes')[1])
    proc = subprocess.Popen([cli, 'run', '--sandbox', sandbox_id, flag, value, '--stdin-file', '-', '--', 'cat'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        code = proc.wait(timeout=3)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
        raise AssertionError('invalid CLI flag blocked reading input')
    finally:
        proc.stdin.close()
    after = decoded(request('GET', '/v1/sandboxes')[1])
    require('CLI ' + flag + ' rejects before input read', code != 0 and before == after, exit_code=code)
print('PASS raw HTTP and CLI stdin gate: ' + str(len(checks)) + ' assertions')
