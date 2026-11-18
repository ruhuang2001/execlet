"""Exercise the acceptance runner's failure path over real HTTP sockets."""
import http.server
import json
import os
from pathlib import Path
import socket
import socketserver
import struct
import subprocess
import sys
import tempfile
import threading
import unittest


class RunnerTests(unittest.TestCase):
    def test_proxy_bypass_and_failure_cleanup(self):
        # Three sandboxes exist when admission fails. The first DELETE also
        # fails; neither failure may hide the other IDs or stop later cleanup.
        created, deleted, requests, proxy_hits = [], [], [], []
        limits = {}

        class RejectProxy(socketserver.BaseRequestHandler):
            def handle(self):
                proxy_hits.append(self.request.recv(65536))
                self.request.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))

        class Origin(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def reply(self, status, data):
                payload = json.dumps(data).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def record(self):
                requests.append((self.command, self.path, self.headers.get('X-Request-ID')))

            def do_POST(self):
                self.record()
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                limit = body.get('max_concurrent_executions', 1)
                if limit in (0, -1, 9):
                    self.reply(400, {'code': 'invalid_request'})
                elif len(created) == 3:
                    self.connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
                    self.connection.close()
                else:
                    sid = 'sbx_fixture_' + str(len(created) + 1)
                    created.append(sid)
                    limits[sid] = limit
                    self.reply(201, {'id': sid, 'max_concurrent_executions': limit})

            def do_GET(self):
                self.record()
                self.reply(200, {'max_concurrent_executions': limits[self.path.rsplit('/', 1)[1]]})

            def do_DELETE(self):
                self.record()
                sid = self.path.rsplit('/', 1)[1]
                deleted.append(sid)
                if sid == created[0]:
                    self.reply(503, {'code': 'backend_error', 'message': 'injected cleanup failure'})
                else:
                    self.reply(200, {'state': 'killed'})

        with tempfile.TemporaryDirectory() as temp, \
                socketserver.ThreadingTCPServer(('127.0.0.1', 0), RejectProxy) as proxy, \
                http.server.ThreadingHTTPServer(('127.0.0.1', 0), Origin) as origin:
            threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in (proxy, origin)]
            for thread in threads:
                thread.start()
            try:
                env = {key: value for key, value in os.environ.items() if not key.lower().endswith('_proxy')}
                env.update(http_proxy=f'http://127.0.0.1:{proxy.server_address[1]}', no_proxy='')
                report_path = Path(temp) / 'report.json'
                result = subprocess.run([sys.executable, '-E', str(Path(__file__).with_name('run.py')),
                    f'http://127.0.0.1:{origin.server_port}', str(report_path)],
                    env=env, capture_output=True, text=True, timeout=20)
                report = json.loads(report_path.read_text())
            finally:
                for server in (proxy, origin):
                    server.shutdown()
                for thread in threads:
                    thread.join()

        self.assertFalse(proxy_hits, 'local acceptance must not inherit an ambient proxy')
        self.assertEqual(len(created), 3)
        self.assertNotEqual(result.returncode, 0)
        self.assertIs(report['passed'], False)
        self.assertEqual(report['owned'], created)
        self.assertEqual(deleted, created, 'attempt every cleanup exactly once')
        self.assertEqual([row['name'] for row in report['checks'] if not row['passed']],
                         ['admission-overlap-replay-cancel-kill'])
        self.assertEqual(report['error_type'], 'ConnectionResetError')
        self.assertNotIn('injected cleanup failure', report['error'])
        self.assertNotIn('During handling of the above exception', result.stderr)
        self.assertIn('injected cleanup failure', report['cleanup'][0]['error'])
        self.assertEqual([row.get('state') for row in report['cleanup'][1:]], ['killed', 'killed'])
        self.assertEqual(sum(method == 'POST' for method, _, _ in requests), 7, 'never replay a failed request')
        self.assertEqual(len(report['request_errors']), 1)
        error = report['request_errors'][0]
        self.assertTrue(error['request_id'])
        self.assertEqual((error['method'], error['path'], error['request_id']), requests[8])


if __name__ == '__main__':
    unittest.main()
