"""Private real sandboxd plus fault proxies. Only run-owned resources are touched."""
from __future__ import annotations

import importlib.util
import http.client
import json
import os
from pathlib import Path
import secrets
import select
import socket
import socketserver
import subprocess
import threading
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class DockerFault:
    """Unix-socket proxy; can fail one container removal without touching others."""

    def __init__(self, path):
        self.path = str(path)
        self.lock = threading.Lock()
        self.fail_remove = False
        self.fail_exec = False
        self.fail_identity = False
        self.failed_identities = []
        self.failed_execs = []
        self.requests = []
        self.start_fault = None
        self.start_targets = {}
        self.start_faults = []
        self.release_start = threading.Event()
        upstream_path = os.environ.get('DOCKER_HOST') or subprocess.run(
            ['docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'],
            capture_output=True, text=True, check=True).stdout.strip()
        if not upstream_path.startswith('unix://'):
            raise RuntimeError('local guards require a unix Docker endpoint')
        self.upstream = upstream_path.removeprefix('unix://')
        outer = self

        class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
            daemon_threads = True

        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                data = b''
                self.request.settimeout(15)
                while b'\r\n\r\n' not in data:
                    part = self.request.recv(65536)
                    if not part:
                        return
                    data += part
                line = data.split(b'\r\n', 1)[0].decode('latin-1')
                # Docker is addressed with a negotiated version prefix such
                # as /v1.52, so never match the path from the first byte.
                path = line.split(' ')[1].partition('?')[0] if ' ' in line else ''
                head, body = data.split(b'\r\n\r\n', 1)
                if line.startswith('POST ') and path.endswith('/exec'):
                    length = next((int(h.split(b':', 1)[1]) for h in head.split(b'\r\n')
                                   if h.lower().startswith(b'content-length:')), 0)
                    while len(body) < length:
                        chunk = self.request.recv(min(65536, length - len(body)))
                        if not chunk:
                            return
                        body += chunk
                    data = head + b'\r\n\r\n' + body
                with outer.lock:
                    outer.requests.append(line)
                    capture_start = False
                    if outer.start_fault and line.startswith('POST ') and path.endswith('/exec'):
                        command = json.loads(body).get('Cmd', [])
                        capture_start = any('RELIABILITY_READY' in arg for arg in command) or (
                            command[:3] == ['python3', '-I', '-c'] and command[4:5] == ['start'])
                    start_mode = outer.start_targets.get(path.split('/')[-2]) if '/exec/' in path and path.endswith('/start') else None
                    fail = outer.fail_remove and line.startswith('DELETE ') and '/containers/' in path
                    if outer.fail_identity and line.startswith('POST ') and path.endswith('/exec'):
                        command = json.loads(body).get('Cmd', [])
                        if (len(command) == 2 and command[0] == 'cat'
                                and command[1].startswith('/run/tiny-sandbox/')
                                and not command[1].endswith('.result')):
                            outer.failed_identities.append(line)
                            fail = True
                    if outer.fail_exec and line.startswith('POST ') and '/containers/' in path and path.endswith('/exec'):
                        outer.failed_execs.append(line)
                        fail = True
                if fail:
                    body = b'{"message":"injected Docker operation failure"}'
                    self.request.sendall(
                        b'HTTP/1.1 500 Internal Server Error\r\nContent-Type: application/json\r\nContent-Length: '
                        + str(len(body)).encode() + b'\r\nConnection: close\r\n\r\n' + body)
                    return
                with socket.socket(socket.AF_UNIX) as upstream:
                    upstream.connect(outer.upstream)
                    head, body = data.split(b'\r\n\r\n', 1)
                    # A large upload arrives after its headers: forward the rest
                    # as it comes, in both directions, until both sides close.
                    length = None
                    for line in head.split(b'\r\n'):
                        name, _, value = line.partition(b':')
                        if name.strip().lower() == b'content-length':
                            length = int(value.strip())
                    head = b'\r\n'.join(x for x in head.split(b'\r\n') if not x.lower().startswith(b'connection:'))
                    upstream.sendall(head + b'\r\nConnection: close\r\n\r\n' + body)
                    if capture_start:
                        response = http.client.HTTPResponse(upstream)
                        response.begin()
                        payload = response.read()
                        if response.status == 201:
                            with outer.lock:
                                outer.start_targets[json.loads(payload)['Id']] = outer.start_fault
                        lines = ['HTTP/1.1 %d %s' % (response.status, response.reason),
                                 'Content-Type: application/json', 'Content-Length: %d' % len(payload),
                                 'Connection: close', '', '']
                        self.request.sendall(bytes([13, 10]).join(x.encode() for x in lines) + payload)
                        return
                    if start_mode:
                        upstream.settimeout(10)
                        observed = b''
                        while b'RELIABILITY_READY' not in observed:
                            chunk = upstream.recv(65536)
                            if not chunk:
                                raise RuntimeError('start fault did not observe user command output')
                            observed += chunk
                            if len(observed) > 65536:
                                raise RuntimeError('start fault exceeded observation limit')
                        with outer.lock:
                            outer.start_faults.append(dict(mode=start_mode, exec_id=path.split('/')[-2], marker_seen=True))
                        if start_mode == 'stall':
                            outer.release_start.wait(12)
                        return
                    while True:
                        reads = [upstream] if length is None or len(body) >= length else [upstream, self.request]
                        ready, _, _ = select.select(reads, [], [], 1)
                        if not ready:
                            # A silent Docker exec is still running. The case
                            # watchdog and owned-container cleanup bound it; an
                            # idle proxy timeout must not fabricate process EOF.
                            continue
                        for peer in ready:
                            try:
                                chunk = peer.recv(65536)
                                if not chunk:
                                    return
                                if peer is self.request:
                                    body += chunk
                                (upstream if peer is self.request else self.request).sendall(chunk)
                            except (BrokenPipeError, ConnectionResetError):
                                # Deadlines and container removal deliberately
                                # close connections while a reply is in flight.
                                return

        self.server = Server(self.path, Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def arm(self, fail):
        with self.lock:
            self.fail_remove = fail

    def deny_exec(self, fail):
        # Arm only after READY. Independent CLI observers bypass this private
        # socket, while all daemon signal helpers fail at the real Docker API.
        with self.lock:
            self.fail_exec = fail

    def removals(self):
        with self.lock:
            return [r for r in self.requests if r.startswith('DELETE ') and '/containers/' in r]

    def close(self):
        self.release_start.set()
        self.server.shutdown()
        self.server.server_close()
        Path(self.path).unlink(missing_ok=True)


class Local:
    """One private server per case, addressed by a run-unique owner label."""

    def __init__(self, out, owner, server, image):
        if not owner.startswith('e2b-contract-'):
            raise ValueError('refusing an owner label this runner did not mint')
        self.out, self.owner, self.server_path, self.image = Path(out), owner, server, image
        self.port = self._free_port()
        self.api_key = secrets.token_hex(24)
        self.process = None
        self.faults = None
        self.docker = None
        self._started = False

    @staticmethod
    def _free_port():
        with socket.socket() as s:
            s.bind(('127.0.0.1', 0))
            return s.getsockname()[1]

    @property
    def domain(self):
        return self.faults.domain if self.faults else f'http://127.0.0.1:{self.port}'

    def start(self):
        if not self._started:
            self._started = True
            self.docker = DockerFault(self.out / 'docker.sock')
            # Adapters talk to the fault proxy, so every guard sees real traffic.
            self.faults = PROXY.FaultProxy(self.port, self.out / 'proxy.jsonl')
        log = (self.out / 'sandboxd.log').open('w')
        # sandboxd has no image flag: the adapter selects the compatibility
        # image per create, from CONTRACT_OPTIONS below.
        self.process = subprocess.Popen(
            [self.server_path, '-addr', f'127.0.0.1:{self.port}', '-docker', self.docker.path,
             '-owner', self.owner],
            env={**os.environ, 'SANDBOX_API_KEY': self.api_key}, stdout=log, stderr=log)
        import http.client
        for _ in range(150):
            if self.process.poll() is not None:
                raise RuntimeError('sandboxd exited during startup; see sandboxd.log')
            try:
                conn = http.client.HTTPConnection('127.0.0.1', self.port, timeout=2)
                conn.request('GET', '/healthz')
                if conn.getresponse().status == 200:
                    conn.close()
                    return
            except OSError:
                time.sleep(.1)
        raise RuntimeError('sandboxd never became healthy')

    def environment(self, language):
        """Adapter connection configuration. Native names, no E2B cloud fallback."""
        key = 'apiKey' if language == 'typescript' else 'api_key'
        return {'SANDBOX_DOMAIN': self.domain, 'SANDBOX_API_KEY': self.api_key,
                'CONTRACT_OPTIONS': json.dumps({'endpoint': self.domain, key: self.api_key,
                                                'image': self.image})}

    def restart(self):
        self.process.terminate()
        self.process.wait(timeout=20)
        self.start()

    def close(self):
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        if self.docker:
            self.docker.close()
        if self.faults:
            self.faults.close()


PROXY = _load('e2b_fault_proxy', ROOT / 'sandbox-python/e2e/proxy.py')
Fault = PROXY.Fault
