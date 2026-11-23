import hmac
import json
import os
import ssl
import time
from pathlib import Path
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Packages(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/stream":
            self.send_response(200)
            self.send_header("Content-Length", "6")
            self.end_headers()
            for value in b"123456":
                self.wfile.write(bytes([value]))
                self.wfile.flush()
                time.sleep(1)
            return
        authorized = hmac.compare_digest(
            self.headers.get("Authorization", ""), "Bearer " + os.environ["TEST_TOKEN"]
        )
        allowed = self.path == "/index" or self.path == "/account" and authorized
        body = json.dumps({"path": self.path, "authenticated": authorized}).encode()
        if self.path == "/" + wheel.name:
            allowed = True
            body = wheel.read_bytes()
        self.send_response(200 if allowed else 403)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


wheel = next(Path("/fixture").glob("pip-*.whl"))
server = ThreadingHTTPServer(("0.0.0.0", 443), Packages)
tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
tls.load_cert_chain("/fixture/ca.pem", "/fixture/key.pem")
server.socket = tls.wrap_socket(server.socket, server_side=True)
server.serve_forever()
