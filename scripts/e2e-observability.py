"""Real-Docker HTTP/SSE checks with a private Unix-socket fault proxy."""
import hashlib
import http.client
import json
import os
from pathlib import Path
import select
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
import uuid

out = Path(sys.argv[1]).resolve()
owner = 'observability-e2e-' + uuid.uuid4().hex
key = 'secret-authorization-' + uuid.uuid4().hex
secret = 'payload-must-not-be-logged-' + uuid.uuid4().hex
report = []
(out / 'responses.jsonl').write_text('')
sources = [*Path('internal').rglob('*.go'), *Path('cmd/sandboxd').rglob('*.go'), Path('scripts/e2e-observability.py')]
(out / 'source-sha256.json').write_text(json.dumps({str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in sources}, indent=2) + '\n')
(out / 'command.txt').write_text('bash scripts/e2e-observability.sh ' + str(out) + '\n')
mode = 'healthy'
proxy_requests = []
docker_host = os.environ.get('DOCKER_HOST') or subprocess.check_output(
    ['docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'], text=True).strip()
assert docker_host.startswith('unix://'), docker_host
socket_path = docker_host.removeprefix('unix://')


class Proxy(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


class Forward(socketserver.BaseRequestHandler):
    def handle(self):
        data = b''
        self.request.settimeout(10)
        while b'\r\n\r\n' not in data:
            part = self.request.recv(65536)
            if not part:
                return
            data += part
        path = data.split(b' ', 2)[1].decode()
        proxy_requests.append({'path': path, 'mode': mode})
        if path.split('?', 1)[0].endswith('/containers/create') and mode == 'create-error':
            body = json.dumps({'message': secret}).encode()
            self.request.sendall(b'HTTP/1.1 500 Internal Server Error\r\nContent-Length: ' + str(len(body)).encode() + b'\r\nConnection: close\r\n\r\n' + body)
            return
        if path == '/_ping' and mode != 'healthy':
            selected = mode
            if selected == 'refused':
                return
            if selected == 'stall':
                time.sleep(2.5)
                return
            status = b'503 Unavailable' if selected == 'status' else b'200 OK'
            body = b'OK' if selected == 'status' else b'NOT-OK'
            self.request.sendall(b'HTTP/1.1 ' + status + b'\r\nContent-Length: ' + str(len(body)).encode() + b'\r\nConnection: close\r\n\r\n' + body)
            return
        with socket.socket(socket.AF_UNIX) as upstream:
            upstream.connect(socket_path)
            # One request per connection makes fault changes deterministic.
            head, body = data.split(b'\r\n\r\n', 1)
            head = b'\r\n'.join(line for line in head.split(b'\r\n') if not line.lower().startswith(b'connection:'))
            upstream.sendall(head + b'\r\nConnection: close\r\n\r\n' + body)
            peers = [upstream, self.request]
            while True:
                ready, _, _ = select.select(peers, [], [], 30)
                if not ready:
                    return
                for peer in ready:
                    chunk = peer.recv(65536)
                    if not chunk:
                        return
                    other = self.request if peer is upstream else upstream
                    other.sendall(chunk)


def check(name, condition, evidence=None):
    report.append({'name': name, 'passed': bool(condition), 'evidence': evidence})
    (out / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
    assert condition, (name, evidence)


def request(path, *, method='GET', payload=None, headers=(), auth=False):
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=5)
    body = None if payload is None else json.dumps(payload)
    conn.putrequest(method, path)
    if auth:
        conn.putheader('Authorization', 'Bearer ' + key)
    for name, value in headers:
        conn.putheader(name, value)
    if body is not None:
        conn.putheader('Content-Type', 'application/json')
        conn.putheader('Content-Length', str(len(body.encode())))
    conn.endheaders(None if body is None else body.encode())
    start = time.monotonic()
    response = conn.getresponse()
    raw = response.read()
    record = {'status': response.status, 'request_id': response.getheader('X-Request-ID'),
              'body': raw.decode(), 'elapsed': time.monotonic() - start}
    with (out / 'responses.jsonl').open('a') as f:
        f.write(json.dumps(record) + '\n')
    conn.close()
    return record


def is_uuid(value):
    try:
        return str(uuid.UUID(value)) == value
    except (ValueError, TypeError):
        return False


server = None
proxy = None
log = (out / 'sandboxd.jsonl').open('w')
try:
    with tempfile.TemporaryDirectory(prefix='ts-observe-') as temp:
        proxy = Proxy(str(Path(temp) / 'docker.sock'), Forward)
        threading.Thread(target=proxy.serve_forever, daemon=True).start()
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        server = subprocess.Popen([str(out / 'sandboxd'), '-addr', f'127.0.0.1:{port}',
                                   '-docker', proxy.server_address, '-owner', owner],
                                  env={**os.environ, 'SANDBOX_API_KEY': key}, stdout=log, stderr=log)
        for _ in range(100):
            try:
                response = request('/healthz')
                break
            except OSError:
                if server.poll() is not None:
                    raise RuntimeError('sandboxd exited during startup')
                time.sleep(.05)
        check('public liveness', response['status'] == 200)
        check('generated request id', is_uuid(response['request_id']))
        response = request('/readyz')
        check('real Docker ready', response['status'] == 200 and json.loads(response['body'])['docker'] == 'ok', response)
        for token in ['trace_123-Ab.C', 'x' * 128]:
            response = request('/healthz', headers=[('X-Request-ID', token)])
            check('preserved bounded request id', response['request_id'] == token)
        for headers in [[('X-Request-ID', 'invalid value')], [('X-Request-ID', 'x' * 129)],
                        [('X-Request-ID', 'one'), ('X-Request-ID', 'two')], [('X-Request-ID', '')]]:
            response = request('/healthz', headers=headers)
            check('rejected request id', is_uuid(response['request_id']))
        for failure in ['refused', 'status', 'body', 'stall']:
            mode = failure
            response = request('/readyz')
            check('readiness ' + failure, response['status'] == 503 and json.loads(response['body'])['docker'] == 'unreachable' and response['elapsed'] < 3, response)
            response = request('/healthz')
            check('liveness independent of ' + failure, response['status'] == 200 and response['elapsed'] < 1)
        mode = 'healthy'
        check('readiness recovers', request('/readyz')['status'] == 200)
        check('authentication required', request('/v1/sandboxes')['status'] == 401)
        response = request('/v1/sandboxes', method='POST', payload={}, auth=True)
        check('real sandbox created', response['status'] == 201, response)
        sandbox_id = json.loads(response['body'])['id']
        base = '/v1/sandboxes/' + sandbox_id
        response = request(base + '/executions', method='POST', auth=True,
                           headers=[('X-Request-ID', 'execution-correlation')],
                           payload={'command': ['sh', '-c', 'echo early; sleep 2; echo ' + secret], 'env': {'PRIVATE': secret}})
        check('execution started', response['status'] in (200, 201), response)
        execution_id = json.loads(response['body'])['id']
        conn = http.client.HTTPConnection('127.0.0.1', port, timeout=10)
        conn.request('GET', base + '/executions/' + execution_id + '/events', headers={'Authorization': 'Bearer ' + key, 'X-Request-ID': 'stream-correlation'})
        started = time.monotonic()
        stream = conn.getresponse()
        first_output = None
        lines = []
        for raw in stream:
            line = raw.decode()
            lines.append(line)
            if 'early' in line and first_output is None:
                first_output = time.monotonic() - started
        elapsed = time.monotonic() - started
        (out / 'events.sse').write_text(''.join(lines))
        check('SSE flush before completion', first_output is not None and first_output < 1.5 and elapsed >= 1.5, {'first_output': first_output, 'elapsed': elapsed})
        check('SSE complete', 'event: complete' in ''.join(lines))
        conn.close()
        replay = request(base + '/executions/' + execution_id + '/events', auth=True)
        check('SSE replay completes', replay['status'] == 200 and 'event: complete' in replay['body'])
        request(base + '/files?path=/' + secret, auth=True)
        request('/unknown-' + secret + '?secret=' + secret, auth=True)
        request('/unknown//' + secret, auth=True)
        mode = 'create-error'
        failed = request('/v1/sandboxes', method='POST', payload={'image': 'missing-' + secret}, auth=True, headers=[('X-Request-ID', 'failure-correlation')])
        mode = 'healthy'
        check('runtime error path', failed['status'] == 502, failed)
        request(base, method='DELETE', auth=True)
        proxy.shutdown()
        proxy.server_close()
        proxy = None
        unavailable = request('/readyz')
        check('readiness with unavailable socket', unavailable['status'] == 503 and unavailable['elapsed'] < 3, unavailable)
        check('liveness with unavailable socket', request('/healthz')['status'] == 200)
        server.terminate()
        server.wait(timeout=20)
        log.flush()
        logs = [json.loads(line) for line in (out / 'sandboxd.jsonl').read_text().splitlines()]
        check('logs contain no private data', secret not in (out / 'sandboxd.jsonl').read_text() and key not in (out / 'sandboxd.jsonl').read_text())
        access = [entry for entry in logs if entry['msg'] == 'http request']
        check('structured access fields', all({'request_id', 'method', 'route', 'status', 'duration_ms', 'bytes'} <= entry.keys() for entry in access))
        lifecycle = [entry for entry in logs if entry['msg'] in ('execution started', 'execution completed')]
        check('execution lifecycle correlated', len(lifecycle) == 2 and all(entry['request_id'] == 'execution-correlation' and entry['execution_id'] == execution_id for entry in lifecycle))
        check('runtime error correlated', any(entry['msg'] == 'request failed' and entry['request_id'] == 'failure-correlation' and entry['code'] == 'backend_error' for entry in logs))
        check('unknown route redacted', any(entry.get('route') == 'unmatched' for entry in access))
        check('stream access recorded', any(entry.get('request_id') == 'stream-correlation' and entry['status'] == 200 and entry['bytes'] > 0 for entry in access))
        check('shutdown recorded', any(entry['msg'] == 'shutdown completed' for entry in logs))
finally:
    if server is not None and server.poll() is None:
        server.terminate()
        try:
            server.wait(timeout=20)
        except subprocess.TimeoutExpired:
            server.kill()
            server.wait(timeout=5)
    if proxy is not None:
        proxy.shutdown()
        proxy.server_close()
    log.close()
    (out / 'proxy-requests.json').write_text(json.dumps(proxy_requests, indent=2) + '\n')
    remaining = subprocess.check_output(['docker', 'ps', '-aq', '--filter', 'label=tiny-sandbox.owner=' + owner], text=True).split()
    if remaining:
        subprocess.run(['docker', 'rm', '-f', *remaining], check=True, capture_output=True)
    after = subprocess.check_output(['docker', 'ps', '-aq', '--filter', 'label=tiny-sandbox.owner=' + owner], text=True).split()
    (out / 'cleanup.json').write_text(json.dumps({'owner': owner, 'remaining_before_fallback': remaining, 'remaining': after}, indent=2) + '\n')
    check('no owned container leaks', not remaining and not after)
print(f'{len(report)} checks passed; evidence: {out}')
