"""Public SDK contract tests. No Docker, cloud account, or E2B dependency.

Run: PYTHONPATH=sandbox-python/src python3.15 -B -m unittest discover -s sandbox-python/tests -v
The HTTP fixture models responses and wire faults, not process-group safety.
Installed Docker acceptance separately proves the runtime behavior.
"""
from __future__ import annotations

import json
import threading
import time
import unittest
from collections import deque
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

from tiny_sandbox import (
    CancellationToken, Capabilities, Client, CommandAttachment, Error, ExecuteRequest,
    Execution, ExecutionKillResult, Sandbox as NativeSandbox,
)
from tiny_sandbox.e2b import (
    AdapterError, AuthenticationException, CommandExitException, CommandHandle,
    CommandResult, InvalidArgumentException, NotFoundException, Sandbox,
    SandboxNotFoundException, TimeoutException,
)

SID = "sandbox-one"
EID = "execution-one"
PID = 42
BASE = f"/v1/sandboxes/{SID}"
START = BASE + "/executions"
ATTACH = BASE + f"/commands/{PID}"
EXECUTION = START + "/" + EID
EVENTS = EXECUTION + "/events"
KILL = EXECUTION + "/kill"
IMAGE = "sha256:" + "a" * 64


def execution(**changes):
    return {"id": EID, "sandbox_id": SID, "pid": PID, "state": "running", **changes}


def complete(**changes):
    return execution(**{"state": "completed", "termination_reason": "exited", "exit_code": 0, **changes})


def frame(event_id, kind, payload):
    return f"id: {event_id}\nevent: {kind}\ndata: {json.dumps(payload, ensure_ascii=False)}\n\n".encode()


def output_events(stdout="hello 世界\n", stderr="diagnostic\n", **changes):
    return (frame(1, "stdout", {"data": stdout}) + frame(2, "stderr", {"data": stderr})
            + frame(3, "complete", complete(stdout=stdout, stderr=stderr, **changes)))


@dataclass
class Reply:
    body: object
    status: int = 200
    truncate: bool = False
    delay: float = 0
    silent: bool = False


class HTTPFixture:
    def __init__(self):
        self.records = []
        self.client_ports = []
        self.routes = {}
        self.stopped = threading.Event()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_):
                pass

            def do_GET(self):
                self.respond()

            do_POST = do_PATCH = do_DELETE = do_GET

            def respond(self):
                raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                body = json.loads(raw) if raw else None
                record = dict(method=self.command, path=self.path, body=body, headers=dict(self.headers))
                fixture.records.append(record)
                fixture.client_ports.append(self.client_address[1])
                if self.headers.get("Authorization") != "Bearer unit-key":
                    reply = Reply({"code": "unauthorized", "message": "invalid test key"}, 401)
                else:
                    replies = fixture.routes.get((self.command, self.path))
                    reply = (replies.popleft() if len(replies) > 1 else replies[0]) if replies else Reply(
                        {"code": "not_found", "message": "not found"}, 404)
                if reply.delay:
                    fixture.stopped.wait(reply.delay)
                data = reply.body if isinstance(reply.body, bytes) else json.dumps(reply.body).encode()
                try:
                    self.send_response(reply.status)
                    self.send_header("Content-Type", "text/event-stream" if isinstance(reply.body, bytes) else "application/json")
                    self.send_header("Content-Length", str(len(data) + (100 if reply.truncate or reply.silent else 0)))
                    self.send_header("Connection", "close")
                    self.end_headers()
                    self.wfile.write(data)
                    self.wfile.flush()
                    if reply.silent:
                        fixture.stopped.wait(3)
                except (BrokenPipeError, ConnectionResetError):
                    pass
                self.close_connection = True

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": .01}, daemon=True)
        self.thread.start()
        self.domain = f"http://127.0.0.1:{self.server.server_port}"
        self.put("GET", "/v1/capabilities", {"capabilities": ["e2b.background.v1", "execution.env-cwd.v1", "execution.stdin.v1", "execution.concurrent.v1", "files.list.v1", "files.archive.v1"]})
        sandbox = {"id": SID, "state": "running"}
        self.put("POST", "/v1/sandboxes", sandbox)
        self.put("GET", BASE, sandbox)
        self.put("PATCH", BASE, sandbox)
        self.put("POST", START, execution())
        self.put("GET", ATTACH, {"execution": execution(), "last_event_id": 0})
        self.put("GET", EVENTS, output_events())
        self.put("POST", KILL, {"execution": execution(state="completed", termination_reason="canceled", exit_code=137), "signaled": True})

    def put(self, method, path, *replies):
        self.routes[method, path] = deque(r if isinstance(r, Reply) else Reply(r) for r in replies)

    def requests(self, method=None, path=None):
        return [r for r in self.records if (method is None or r["method"] == method) and (path is None or r["path"] == path)]

    def close(self):
        self.stopped.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()


class BackgroundCommandsTests(unittest.TestCase):
    def setUp(self):
        self.http = HTTPFixture()
        self.addCleanup(self.http.close)
        self.native = Client(domain=self.http.domain, api_key="unit-key", max_retries=1)
        self.addCleanup(self.native.close)
        self.sandbox = Sandbox.create(image=IMAGE, endpoint=self.http.domain, api_key="unit-key")
        self.http.records.clear()

    def handle(self, **kwargs):
        return self.sandbox.commands.run("printf test", **{"background": True, "timeout": 0, **kwargs})

    def assert_no_cancel(self):
        self.assertFalse([r for r in self.http.records if r["path"].endswith("/cancel")])

    def test_native_models_and_authenticated_capabilities(self):
        self.http.put("GET", "/v1/capabilities", {"capabilities": ["e2b.background.v1", "future"], "revision": 7})
        capabilities = self.native.get_capabilities()
        self.assertIsInstance(capabilities, Capabilities)
        self.assertEqual(capabilities.extra, {"revision": 7})
        self.assertIn("future", capabilities.capabilities)
        attachment = self.native.get_command(SID, PID)
        self.assertIsInstance(attachment, CommandAttachment)
        self.assertIsInstance(attachment.execution, Execution)
        self.assertEqual(attachment.execution.pid, PID)
        killed = self.native.kill_execution(SID, EID)
        self.assertIsInstance(killed, ExecutionKillResult)
        self.assertTrue(killed.signaled)
        self.assertTrue(all(r["headers"]["Authorization"] == "Bearer unit-key" for r in self.http.records))

    def test_native_timeouts_preserve_defaults_and_zero(self):
        for request, expected in [(ExecuteRequest(command=["true"]), {}),
                                  (ExecuteRequest(command=["true"], timeout_seconds=2), {"timeout_seconds": 2}),
                                  (ExecuteRequest(command=["true"], timeout_ms=0), {"timeout_ms": 0}),
                                  (ExecuteRequest(command=["true"], timeout_ms=1250), {"timeout_ms": 1250})]:
            self.native.start_execution(SID, request)
            body = self.http.requests("POST", START)[-1]["body"]
            self.assertEqual({k: v for k, v in body.items() if k.startswith("timeout_")}, expected)
        before = len(self.http.records)
        for value in (True, -1, 1.5, 9_223_372_036_855):
            with self.assertRaises(Error):
                self.native.start_execution(SID, ExecuteRequest(command=["true"], timeout_ms=value))
        with self.assertRaises(Error):
            self.native.start_execution(SID, ExecuteRequest(command=["true"], timeout_seconds=0, timeout_ms=0))
        self.assertEqual(len(self.http.records), before)

    def test_background_returns_identity_without_observing(self):
        handle = self.handle()
        self.assertIsInstance(handle, CommandHandle)
        self.assertEqual((handle.sandbox_id, handle.execution_id, handle.pid), (SID, EID, PID))
        self.assertEqual([r["path"] for r in self.http.records], ["/v1/capabilities", START])
        for name, value in (("sandbox_id", "other"), ("execution_id", "other"), ("pid", 99)):
            with self.assertRaises(AttributeError):
                setattr(handle, name, value)
        wire = self.http.requests("POST", START)[0]["body"]
        self.assertEqual(wire["timeout_ms"], 0)
        self.assertEqual(wire["command"], ["/bin/bash", "-l", "-c", "printf test"])
        self.assertEqual(wire["cwd"], "/home/user")
        self.assertNotIn("timeout_seconds", wire)

    def test_foreground_and_default_deadline(self):
        seen = []
        result = self.sandbox.commands.run("true", on_stdout=seen.append, envs={"A": "B"}, cwd="/tmp")
        self.assertIsInstance(result, CommandResult)
        self.assertEqual((result.stdout, result.stderr, result.exit_code), ("hello 世界\n", "diagnostic\n", 0))
        self.assertEqual(seen, [result.stdout])
        body = self.http.requests("POST", START)[0]["body"]
        self.assertEqual(body["timeout_ms"], 60000)
        self.assertEqual(body["env"], {"A": "B"})
        self.assertEqual(body["cwd"], "/tmp")

    def test_pinned_duration_conversion_via_public_api(self):
        with patch.object(Client, "get_capabilities", return_value=Capabilities(capabilities=["e2b.background.v1"])), \
                patch.object(Client, "start_execution", return_value=Execution.from_dict(execution())) as start:
            for seconds, milliseconds in ((0, 0), (1.25, 1250), (.00001, 1), (.0025, 2), (.0035, 4), (9223372036.854, 9223372036854)):
                handle = self.handle(timeout=seconds)
                self.assertEqual(start.call_args.args[1].timeout_ms, milliseconds)
                handle.disconnect()

    def test_invalid_arguments_precede_all_requests(self):
        commands = self.sandbox.commands
        for options in ({"background": "true"}, {"background": 1}, {"stdin": True}, {"stdin": "text"},
                        {"retain_output": False}, {"request_timeout": 1}, {"unknown": 1},
                        {"cwd": "relative"}, {"envs": {"A": 1}}, {"on_stdout": 1},
                        {"background": True, "on_stdout": lambda _: None}):
            with self.subTest(options=options), self.assertRaises(InvalidArgumentException):
                commands.run("true", **options)
        for timeout in (True, -1, float("nan"), float("inf"), -float("inf"), 9_223_372_037, 10**500, "1", None):
            for call in (lambda: commands.run("true", timeout=timeout), lambda: commands.connect(PID, timeout=timeout)):
                with self.subTest(timeout=timeout), self.assertRaises(InvalidArgumentException):
                    call()
        for pid in (True, False, 0, -1, 1.5, "42", None):
            for call in (commands.connect, commands.kill):
                with self.subTest(pid=pid), self.assertRaises(InvalidArgumentException):
                    call(pid)
        for options in ({"on_stdout": lambda _: None}, {"request_timeout": 1}, {"pty": True}):
            with self.assertRaises(InvalidArgumentException):
                commands.connect(PID, **options)
        self.assertEqual(self.http.records, [])

    def test_fail_closed_capability_gate_for_all_operations(self):
        commands = self.sandbox.commands
        handle = self.handle()
        for reply in (Reply(b"404 page not found\n", 404), {"capabilities": []}, {}, {"capabilities": "e2b.background.v1"}):
            self.http.put("GET", "/v1/capabilities", reply)
            for call in (lambda: commands.run("true"), lambda: self.handle(),
                         lambda: commands.connect(PID), lambda: commands.kill(PID), handle.kill):
                self.http.records.clear()
                with self.assertRaises(AdapterError) as caught:
                    call()
                self.assertEqual(caught.exception.code, "incompatible_server")
                self.assertEqual([r["path"] for r in self.http.records], ["/v1/capabilities"])

    def test_capability_and_auth_do_not_leak_between_clients(self):
        self.handle()
        other = HTTPFixture()
        self.addCleanup(other.close)
        other.put("GET", "/v1/capabilities", {"capabilities": []})
        second = Sandbox.create(image=IMAGE, endpoint=other.domain, api_key="unit-key")
        with self.assertRaises(AdapterError) as caught:
            second.commands.run("true")
        self.assertEqual(caught.exception.code, "incompatible_server")
        self.assertEqual(other.requests("POST", START), [])
        with Client(domain=self.http.domain, api_key="wrong", max_retries=0) as wrong:
            bad = Sandbox(NativeSandbox(id=SID, state="running"), wrong, 1)
            for call in (lambda: bad.commands.run("true"), lambda: bad.commands.connect(PID), lambda: bad.commands.kill(PID)):
                with self.assertRaises(AuthenticationException) as auth:
                    call()
                self.assertEqual(str(auth.exception), "invalid test key")
                self.assertEqual(auth.exception.code, "unauthorized")

    def test_fast_exit_start_is_still_a_handle_and_wait_is_cached(self):
        self.http.put("POST", START, complete(stdout="hello 世界\n", stderr="diagnostic\n"))
        handle = self.handle()
        seen = []
        first = handle.wait(on_stdout=seen.append)
        before = len(self.http.records)
        self.assertIs(handle.wait(on_stdout=seen.append), first)
        handle.disconnect()
        self.assertIs(handle.wait(on_stdout=seen.append), first)
        self.assertEqual(seen, ["hello 世界\n"])
        self.assertEqual(len(self.http.records), before)

    def test_ordinary_nonzero_exit_is_cached_with_evidence(self):
        self.http.put("GET", EVENTS, output_events(exit_code=37))
        handle = self.handle()
        with self.assertRaises(CommandExitException) as first:
            handle.wait()
        self.assertEqual(first.exception.exit_code, 37)
        self.assertEqual(first.exception.execution_id, EID)
        self.assertEqual(first.exception.stdout, "hello 世界\n")
        before = len(self.http.records)
        with self.assertRaises(CommandExitException) as again:
            handle.wait()
        self.assertIs(first.exception, again.exception)
        self.assertEqual(len(self.http.records), before)

    def test_completed_attach_has_outcome_without_history_or_stream(self):
        self.http.put("GET", ATTACH, {"execution": complete(stdout="PRE", stderr="OLD", exit_code=37), "last_event_id": 12})
        handle = self.sandbox.commands.connect(PID, timeout=0)
        before = len(self.http.records)
        seen = []
        with self.assertRaises(CommandExitException) as caught:
            handle.wait(on_stdout=seen.append)
        self.assertEqual((caught.exception.stdout, caught.exception.stderr, caught.exception.exit_code), ("", "", 37))
        self.assertEqual(seen, [])
        self.assertEqual(len(self.http.records), before)
        handle.disconnect()
        with self.assertRaises(CommandExitException) as again:
            handle.wait()
        self.assertIs(caught.exception, again.exception)

    def test_fresh_attach_and_same_handle_resume_use_distinct_cursors(self):
        self.http.put("GET", ATTACH, {"execution": execution(stdout="PRE"), "last_event_id": 9})
        first = frame(10, "stdout", {"data": "POST"})
        second = frame(11, "stderr", {"data": "ERR"})
        terminal = frame(12, "complete", complete(stdout="PREPOST", stderr="OLDERR"))
        self.http.put("GET", EVENTS, first + second[:20], first + second + terminal)
        handle = self.sandbox.commands.connect(PID, timeout=0)
        chunks = []
        result = handle.wait(on_stdout=chunks.append, on_stderr=chunks.append)
        self.assertEqual((result.stdout, result.stderr), ("POST", "ERR"))
        self.assertEqual(chunks, ["POST", "ERR"])
        self.assertEqual([r["headers"].get("Last-Event-ID") for r in self.http.requests("GET", EVENTS)], ["9", "10"])
        self.assertEqual(len(self.http.requests("GET", ATTACH)), 1)
        self.assertEqual(self.http.requests("POST"), [])
        self.assertEqual(self.http.requests("PATCH"), [])

    def test_start_replay_skips_delivered_callbacks(self):
        first = frame(1, "stdout", {"data": "once"})
        self.http.put("GET", EVENTS, first + b"id: 2\nevent: stdout\ndata: {",
                      first + frame(2, "complete", complete(stdout="once")))
        seen = []
        result = self.handle().wait(on_stdout=seen.append)
        self.assertEqual(result.stdout, "once")
        self.assertEqual(seen, ["once"])
        self.assertEqual([r["headers"].get("Last-Event-ID") for r in self.http.requests("GET", EVENTS)], [None, "1"])

    def test_accepted_start_loss_reuses_key_and_exact_invocation(self):
        self.http.put("POST", START, Reply(execution(), truncate=True), execution())
        handle = self.handle(timeout=1.25)
        requests = self.http.requests("POST", START)
        self.assertEqual(len(requests), 2)
        self.assertEqual(requests[0]["body"], requests[1]["body"])
        self.assertEqual(requests[0]["body"]["timeout_ms"], 1250)
        self.assertTrue(requests[0]["body"]["idempotency_key"])
        self.assertEqual(handle.execution_id, EID)
        self.assert_no_cancel()

    def test_uncertain_start_keeps_key_without_cleanup_or_new_invocation(self):
        self.http.put("POST", START, Reply(execution(), delay=.3))
        with self.assertRaises(TimeoutException) as caught:
            self.handle(timeout=.1)
        self.assertTrue(caught.exception.idempotency_key)
        self.assertEqual(len(self.http.requests("POST", START)), 1)
        self.assertIn("may have been accepted", str(caught.exception))
        self.assert_no_cancel()

    def test_missing_pid_never_returns_handle_and_keeps_start_identity(self):
        self.http.put("POST", START, {k: v for k, v in execution().items() if k != "pid"})
        with self.assertRaises(AdapterError) as caught:
            self.handle()
        self.assertEqual(caught.exception.execution_id, EID)
        self.assertTrue(caught.exception.idempotency_key)
        self.assert_no_cancel()

    def test_malformed_start_handshake_retains_key_and_available_execution_id(self):
        for body in (execution(pid=0), execution(pid=True), execution(pid=-1),
                     execution(pid="42"), execution(state="unknown"), execution(sandbox_id="wrong")):
            self.http.put("POST", START, body)
            before = len(self.http.requests("POST", START))
            with self.subTest(body=body), self.assertRaises(AdapterError) as caught:
                self.handle()
            self.assertEqual(caught.exception.execution_id, EID)
            self.assertEqual(caught.exception.sandbox_id, SID)
            self.assertTrue(caught.exception.idempotency_key)
            self.assertEqual(len(self.http.requests("POST", START)), before + 1)
        self.assert_no_cancel()

    def test_capability_transport_consumes_observation_budget_before_start(self):
        self.http.put("GET", "/v1/capabilities", Reply({"capabilities": ["e2b.background.v1"]}, delay=.3))
        with self.assertRaises(TimeoutException):
            self.handle(timeout=.1)
        self.assertEqual(self.http.requests("POST", START), [])
        self.assert_no_cancel()

    def test_callback_failure_keeps_identity_and_is_permanently_cached(self):
        for sentinel in (RuntimeError("sentinel"), OSError("user IO"), Error("user", "user native error")):
            with self.subTest(sentinel=sentinel):
                handle = self.handle()
                seen = []

                def callback(chunk):
                    seen.append(chunk)
                    raise sentinel

                with self.assertRaises(type(sentinel)) as caught:
                    handle.wait(on_stdout=callback)
                self.assertIs(caught.exception, sentinel)
                before = len(self.http.records)
                handle.disconnect()
                with self.assertRaises(type(sentinel)) as again:
                    handle.wait()
                self.assertIs(again.exception, sentinel)
                self.assertEqual(len(self.http.records), before)
                self.assertEqual(len(seen), 1)
                self.assertTrue(handle.kill())
        self.assert_no_cancel()

    def test_disconnect_is_local_idempotent_and_cannot_revive(self):
        handle = self.handle()
        before = len(self.http.records)
        handle.disconnect()
        handle.disconnect()
        with self.assertRaises(AdapterError) as caught:
            handle.wait()
        self.assertEqual(caught.exception.code, "disconnected")
        self.assertEqual(len(self.http.records), before)
        self.assertTrue(handle.kill())
        self.assert_no_cancel()

    def test_disconnect_from_callback_stops_next_dispatch(self):
        handle = self.handle()
        seen = []

        def callback(text):
            seen.append(text)
            handle.disconnect()

        with self.assertRaises(AdapterError) as caught:
            handle.wait(on_stdout=callback, on_stderr=seen.append)
        self.assertEqual(caught.exception.code, "disconnected")
        self.assertEqual(seen, ["hello 世界\n"])
        self.assert_no_cancel()

    def test_wait_clock_begins_at_connect_and_timeout_is_cached(self):
        handle = self.sandbox.commands.connect(PID, timeout=.08)
        time.sleep(.1)
        before = len(self.http.records)
        with self.assertRaises(TimeoutException) as caught:
            handle.wait()
        self.assertEqual(caught.exception.execution_id, EID)
        with self.assertRaises(TimeoutException) as again:
            handle.wait()
        self.assertIs(caught.exception, again.exception)
        self.assertEqual(len(self.http.records), before)
        self.assert_no_cancel()

    def test_silent_stream_timeout_is_local_and_reconnect_does_not_start(self):
        self.http.put("GET", EVENTS, Reply(b"", silent=True))
        handle = self.sandbox.commands.connect(PID, timeout=.1)
        with self.assertRaises(TimeoutException):
            handle.wait()
        self.http.put("GET", EVENTS, output_events())
        again = self.sandbox.commands.connect(PID, timeout=0)
        self.assertEqual(again.wait().exit_code, 0)
        self.assertEqual(self.http.requests("POST"), [])
        self.assertEqual(self.http.requests("PATCH"), [])
        self.assert_no_cancel()

    def test_cross_thread_and_reentrant_consumers_fail_explicitly(self):
        handle = self.handle()
        errors = []

        def other_thread():
            for operation in (handle.wait, handle.disconnect, handle.kill):
                try:
                    operation()
                except AdapterError as exc:
                    errors.append(exc.code)

        thread = threading.Thread(target=other_thread)
        thread.start()
        thread.join()
        self.assertEqual(errors, ["unsupported_operation"] * 3)
        before = len(self.http.records)
        self.assertEqual(before, 2)

        def callback(_):
            with self.assertRaises(AdapterError) as caught:
                handle.wait()
            self.assertEqual(caught.exception.code, "unsupported_operation")

        self.assertEqual(handle.wait(on_stdout=callback).exit_code, 0)

    def test_handle_options_are_rejected_without_poisoning_observation(self):
        handle = self.handle()
        before = len(self.http.records)
        for operation in (handle.wait, handle.disconnect, handle.kill):
            with self.assertRaises(InvalidArgumentException):
                operation(request_timeout=1)
        with self.assertRaises(InvalidArgumentException):
            handle.wait(timeout=1)
        self.assertEqual(len(self.http.records), before)
        self.assertEqual(handle.wait().exit_code, 0)

    def test_truncation_before_attach_cannot_be_hidden_by_empty_suffix(self):
        for state in ("running", "completed"):
            snapshot = execution(state=state, output_truncated=True, stdout="PRE")
            self.http.put("GET", ATTACH, {"execution": snapshot, "last_event_id": 100})
            handle = self.sandbox.commands.connect(PID, timeout=0)
            with self.assertRaises(AdapterError) as caught:
                handle.wait()
            self.assertEqual(caught.exception.code, "output_truncated")
            self.assertTrue(caught.exception.output_truncated)
            self.assertEqual(caught.exception.execution_id, EID)
            self.assertEqual(caught.exception.stdout, "")
        self.assertEqual(self.http.requests("GET", EVENTS), [])

    def test_truncation_event_preserves_partial_output(self):
        self.http.put("GET", EVENTS, frame(1, "stdout", {"data": "partial"})
                      + frame(2, "truncated", {"limit_bytes": 1048576}))
        with self.assertRaises(AdapterError) as caught:
            self.handle().wait()
        self.assertEqual(caught.exception.stdout, "partial")
        self.assertTrue(caught.exception.output_truncated)
        self.assertEqual(caught.exception.code, "output_truncated")
        self.assert_no_cancel()

    def test_abnormal_outcomes_and_missing_exit_are_not_success(self):
        for reason in ("timeout", "canceled", "oom", "evicted", "unknown", "future_reason", None):
            terminal = execution(state="completed", termination_reason=reason, exit_code=137)
            self.http.put("GET", EVENTS, frame(1, "complete", terminal))
            with self.subTest(reason=reason), self.assertRaises(AdapterError) as caught:
                self.handle().wait()
            self.assertEqual(caught.exception.code, reason or "incomplete")
            self.assertEqual(caught.exception.exit_code, 137)
            self.assertEqual(caught.exception.execution_id, EID)
        self.http.put("GET", EVENTS, frame(1, "complete", complete(exit_code=None)))
        with self.assertRaises(AdapterError):
            self.handle().wait()

    def test_premature_eof_is_bounded_failure_not_empty_success(self):
        self.http.put("GET", EVENTS, b"")
        handle = self.handle()
        with self.assertRaises(AdapterError) as caught:
            handle.wait()
        self.assertEqual(caught.exception.code, "stream")
        self.assertEqual(len(self.http.requests("GET", EVENTS)), 4)
        before = len(self.http.records)
        with self.assertRaises(AdapterError) as again:
            handle.wait()
        self.assertIs(caught.exception, again.exception)
        self.assertEqual(len(self.http.records), before)
        self.assert_no_cancel()

    def test_command_missing_is_distinct_from_missing_or_paused_sandbox(self):
        missing = Reply({"code": "not_found", "message": "command absent"}, 404)
        self.http.put("GET", ATTACH, missing)
        with self.assertRaises(NotFoundException) as caught:
            self.sandbox.commands.connect(PID)
        self.assertIs(type(caught.exception), NotFoundException)
        self.assertFalse(self.sandbox.commands.kill(PID))
        self.http.put("GET", BASE, Reply({"code": "not_found", "message": "sandbox absent"}, 404))
        with self.assertRaises(SandboxNotFoundException):
            self.sandbox.commands.connect(PID)
        self.http.put("GET", ATTACH, Reply({"code": "invalid_state", "message": "sandbox paused"}, 409))
        with self.assertRaises(SandboxNotFoundException):
            self.sandbox.commands.connect(PID)
        self.assertEqual(self.http.requests("POST"), [])
        self.assertEqual(self.http.requests("PATCH"), [])

    def test_expired_retention_during_wait_is_command_not_found(self):
        handle = self.handle()
        self.http.put("GET", EVENTS, Reply({"code": "not_found", "message": "execution expired"}, 404))
        with self.assertRaises(NotFoundException) as caught:
            handle.wait()
        self.assertIs(type(caught.exception), NotFoundException)
        self.assertEqual(caught.exception.execution_id, EID)

    def test_stale_handle_kill_uses_original_execution_not_new_pid_mapping(self):
        handle = self.handle()
        self.http.put("GET", ATTACH, {"execution": execution(id="replacement"), "last_event_id": 0})
        self.http.put("POST", KILL, {"execution": complete(), "signaled": False})
        self.assertFalse(handle.kill())
        self.assertEqual(self.http.requests("GET", ATTACH), [])
        self.assertEqual([r["path"] for r in self.http.requests("POST")], [START, KILL])
        self.http.put("POST", START + "/replacement/kill", {"execution": execution(
            id="replacement", state="completed", termination_reason="canceled", exit_code=137), "signaled": True})
        self.assertTrue(self.sandbox.commands.kill(PID))
        self.assertEqual(len(self.http.requests("GET", ATTACH)), 1)

    def test_kill_loss_is_uncertain_and_not_retried(self):
        handle = self.handle()
        self.http.put("POST", KILL, Reply({"execution": complete(), "signaled": True}, truncate=True),
                      {"execution": complete(), "signaled": False})
        with self.assertRaises(AdapterError) as caught:
            handle.kill()
        self.assertEqual(caught.exception.code, "backend_error")
        self.assertEqual(caught.exception.execution_id, EID)
        self.assertEqual(len(self.http.requests("POST", KILL)), 1)
        self.assert_no_cancel()

    def test_server_kill_failure_preserves_native_diagnostics(self):
        handle = self.handle()
        message = "group death unconfirmed; sandbox removed; sibling impact possible"
        self.http.put("POST", KILL, Reply({"code": "backend_error", "message": message}, 503))
        with self.assertRaises(AdapterError) as caught:
            handle.kill()
        self.assertEqual(str(caught.exception), message)
        self.assertEqual(caught.exception.code, "backend_error")
        self.assertEqual(caught.exception.execution_id, EID)
        self.assertEqual(caught.exception.pid, PID)
        self.assertEqual(len(self.http.requests("POST", KILL)), 1)

    def test_kill_uses_connection_request_budget_and_preserves_identity(self):
        sandbox = Sandbox.create(image=IMAGE, endpoint=self.http.domain, api_key="unit-key", request_timeout=.1)
        handle = sandbox.commands.run("true", background=True, timeout=0)
        self.http.put("POST", KILL, Reply({"execution": complete(), "signaled": False}, delay=.3))
        with self.assertRaises(AdapterError) as caught:
            handle.kill()
        self.assertEqual(caught.exception.code, "backend_error")
        self.assertEqual(caught.exception.execution_id, EID)
        self.assertEqual(caught.exception.pid, PID)
        self.assertEqual(len(self.http.requests("POST", KILL)), 1)
        self.assertEqual(self.http.requests("GET", ATTACH), [])
        self.assert_no_cancel()

    def test_terminal_pid_mismatch_cannot_complete_original_handle(self):
        self.http.put("GET", EVENTS, frame(1, "complete", complete(pid=43)))
        with self.assertRaises(AdapterError) as caught:
            self.handle().wait()
        self.assertEqual(caught.exception.code, "backend_error")
        self.assertEqual(caught.exception.execution_id, EID)
        self.assertEqual(caught.exception.pid, PID)

    def test_initial_truncation_preserves_available_start_evidence(self):
        self.http.put("POST", START, complete(output_truncated=True, stdout="partial"))
        handle = self.handle()
        with self.assertRaises(AdapterError) as caught:
            handle.wait()
        self.assertEqual(caught.exception.stdout, "partial")
        self.assertTrue(caught.exception.output_truncated)
        self.assertEqual(caught.exception.code, "output_truncated")

    def test_unconfirmed_and_mismatched_kill_responses_fail_closed(self):
        handle = self.handle()
        for body in ({"execution": execution(), "signaled": True},
                     {"execution": complete(), "signaled": True},
                     {"execution": complete(id="wrong"), "signaled": False},
                     {"execution": complete(pid=43), "signaled": False},
                     {"execution": complete(), "signaled": 1}, {"signaled": True}):
            self.http.put("POST", KILL, body)
            with self.subTest(body=body), self.assertRaises(AdapterError) as caught:
                handle.kill()
            self.assertEqual(caught.exception.code, "backend_error")
            self.assertEqual(caught.exception.execution_id, EID)

    def test_malformed_attach_boundary_and_identity_are_rejected(self):
        for snapshot, cursor in ((execution(), True), (execution(), -1), (execution(), 2**64),
                                 (execution(pid=None), 0), (execution(pid=43), 0),
                                 (execution(sandbox_id="wrong"), 0), (execution(state="queued"), 0)):
            self.http.put("GET", ATTACH, {"execution": snapshot, "last_event_id": cursor})
            with self.subTest(snapshot=snapshot, cursor=cursor), self.assertRaises(AdapterError):
                self.sandbox.commands.connect(PID, timeout=0)
        self.assertEqual(self.http.requests("GET", EVENTS), [])
        self.assertEqual(self.http.requests("POST"), [])

    def test_native_stream_initial_cursor_is_additive(self):
        self.http.put("GET", EVENTS, frame(8, "stdout", {"data": "old"})
                      + frame(9, "stdout", {"data": "new"})
                      + frame(10, "complete", complete(stdout="oldnew")))
        seen = []
        result = self.native.stream_execution(SID, EID, last_event_id=8, on_stdout=seen.append)
        self.assertEqual(result.stdout, "oldnew")
        self.assertEqual(seen, ["new"])
        self.assertEqual(self.http.requests("GET", EVENTS)[0]["headers"]["Last-Event-ID"], "8")

    def test_native_cancellation_still_requests_graceful_cancel(self):
        self.http.put("POST", EXECUTION + "/cancel", execution(state="completed", termination_reason="canceled", exit_code=143))
        token = CancellationToken()
        token.cancel()
        with self.assertRaises(Error) as caught:
            self.native.stream_execution(SID, EID, cancel=token)
        self.assertEqual(caught.exception.code, "canceled")
        self.assertEqual(caught.exception.execution.exit_code, 143)
        self.assertEqual(len(self.http.requests("POST", EXECUTION + "/cancel")), 1)
        self.assertEqual(self.http.requests("GET", "/v1/capabilities"), [])

    def test_native_new_fields_fail_before_mutation_on_old_server(self):
        from tiny_sandbox import CreateSandboxRequest
        self.http.put("GET", "/v1/capabilities", Reply({}, 404))
        operations = [
            lambda: self.native.create_sandbox(CreateSandboxRequest(env={"TOKEN": "private"})),
            lambda: self.native.create_sandbox(CreateSandboxRequest(max_concurrent_executions=2)),
            *[lambda extra=extra: self.native.start_execution(SID, ExecuteRequest(command=["true"], **extra))
              for extra in ({"env": {"X": "Y"}}, {"cwd": "/tmp"}, {"stdin": "payload"}, {"timeout_ms": 0})],
            lambda: self.native.get_command(SID, PID),
            lambda: self.native.kill_execution(SID, EID),
            lambda: self.native.list_files(SID, "/"),
            lambda: self.native.read_archive(SID, "/"),
            lambda: self.native.write_archive(SID, "/", b"tar"),
        ]
        for operation in operations:
            self.http.records.clear()
            with self.assertRaises(Error) as caught:
                operation()
            self.assertEqual(caught.exception.code, "incompatible_server")
            self.assertTrue(self.http.records)
            self.assertTrue(all(r["path"] == "/v1/capabilities" for r in self.http.records))
        self.http.records.clear()
        self.native.start_execution(SID, ExecuteRequest(command=["true"]))
        self.assertEqual([r["path"] for r in self.http.records], [START])

    def test_native_cancellation_rejects_unknown_terminal_outcome_and_file_entry_metadata(self):
        for bad_reason in ("unknown", "future_reason"):
            self.http.put("POST", EXECUTION + "/cancel", execution(state="completed", termination_reason=bad_reason, exit_code=None))
            token = CancellationToken()
            token.cancel()
            with self.subTest(reason=bad_reason), self.assertRaises(Error) as caught:
                self.native.stream_execution(SID, EID, cancel=token)
            self.assertEqual(caught.exception.code, "cancel_unconfirmed")
        self.http.put("GET", f"/v1/sandboxes/{SID}/files/list?path=%2Fwork&limit=100&cursor=", {
            "entries": [{
                "name": "link", "size": 4, "mode": 0o777, "mtime": "2026-11-18T00:00:00Z",
                "is_dir": False, "type": "symlink", "uid": 1000, "gid": 1000,
                "owner": "user", "group": "user", "symlink_target": "dest", "future": 17,
            }],
        })
        page = self.native.list_files(SID, "/work")
        self.assertEqual(page.entries[0].type, "symlink")
        self.assertEqual(page.entries[0].uid, 1000)
        self.assertEqual(page.entries[0].symlink_target, "dest")
        self.assertEqual(page.entries[0].extra, {"future": 17})

    def test_malformed_file_list_is_not_coerced(self):
        entry = dict(name="x", size=0, mode=0, mtime="2026-11-18T00:00:00Z", is_dir=False,
                     type="future", uid=0, gid=0)
        pages = [{"entries": None}, {"entries": [], "next_cursor": 1}]
        pages.append({"entries": [{**entry, "owner": None}]})
        for key, wrong in (("type", None), ("uid", True), ("gid", "0")):
            pages.extend([{"entries": [{k: v for k, v in entry.items() if k != key}]},
                          {"entries": [{**entry, key: wrong}]}])
        for page in pages:
            self.http.put("GET", BASE + "/files/list?path=%2F&limit=100&cursor=", page)
            with self.subTest(page=page), self.assertRaises(Error) as caught:
                self.native.list_files(SID, "/")
            self.assertEqual(caught.exception.code, "decode")

    def test_e2b_write_rejects_missing_list_support_before_overwrite(self):
        self.http.put("GET", "/v1/capabilities", {"capabilities": []})
        with patch.object(self.sandbox.files._client, "write_file") as write:
            with self.assertRaises(AdapterError) as caught:
                self.sandbox.files.write("/work/file", "replacement")
            self.assertEqual(caught.exception.code, "incompatible_server")
            write.assert_not_called()

    def test_cancel_validates_every_snapshot_before_polling(self):
        token = CancellationToken()
        token.cancel()
        for ack, poll, code, gets in [
            (execution(), execution(), "canceled", 2),
            (execution(id="wrong"), execution(), "cancel_unconfirmed", 0),
            (execution(), execution(sandbox_id="wrong"), "cancel_unconfirmed", 1),
            (Reply({}, 503), execution(), "cancel_unconfirmed", 0),
        ]:
            self.http.records.clear()
            self.http.put("POST", EXECUTION + "/cancel", ack)
            self.http.put("GET", EXECUTION, poll, complete())
            with self.subTest(ack=ack, poll=poll), self.assertRaises(Error) as caught:
                self.native.stream_execution(SID, EID, cancel=token)
            self.assertEqual(caught.exception.code, code)
            self.assertEqual((caught.exception.execution.id, caught.exception.execution.sandbox_id), (EID, SID))
            self.assertEqual([(r["method"], r["path"]) for r in self.http.records],
                             [("POST", EXECUTION + "/cancel")] + [("GET", EXECUTION)] * gets)

    def test_recovered_start_from_another_sandbox_is_never_canceled(self):
        self.http.put("POST", START, execution(sandbox_id="other"))
        request = ExecuteRequest(command=["true"], idempotency_key="original")
        with self.assertRaises(Error) as caught:
            self.native._cancel_remote(SID, None, request=request)
        self.assertEqual(caught.exception.code, "cancel_unconfirmed")
        self.assertEqual(caught.exception.idempotency_key, "original")
        self.assertEqual([r["path"] for r in self.http.records], [START])

    def test_keep_alive_connection_pool_reuses_socket_across_requests(self):
        ports = []

        class KeepAliveHandler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_):
                pass

            def do_GET(self):
                ports.append(self.client_address[1])
                payload = json.dumps({"id": SID, "state": "running"}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        server = ThreadingHTTPServer(("127.0.0.1", 0), KeepAliveHandler)
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        thread.start()
        try:
            with Client(domain=f"http://127.0.0.1:{server.server_port}", api_key="unit-key") as pooled:
                pooled.get_sandbox(SID)
                start = threading.Thread.start
                with patch.object(threading.Thread, "start", autospec=True, side_effect=start) as spawned:
                    for _ in range(8):
                        token = CancellationToken(timeout=2)
                        self.assertEqual(pooled.get_sandbox(SID, cancel=token).id, SID)
                        token.cancel()
                    self.assertLessEqual(spawned.call_count, 1)
            self.assertEqual(len(ports), 9)
            self.assertEqual(len(set(ports)), 1)
        finally:
            server.shutdown()
            server.server_close()

    def test_lost_ack_on_pooled_socket_obeys_attempt_budget(self):
        calls = []

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_):
                pass

            def do_GET(self):
                calls.append((self.command, self.client_address[1]))
                if len(calls) > 1:
                    self.close_connection = True
                    return
                data = json.dumps({"id": SID, "state": "running"}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self.do_GET()

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": .01}, daemon=True)
        thread.start()
        try:
            for method, retries, attempts in [("POST", 3, 1), ("GET", 0, 1), ("GET", 1, 2)]:
                calls.clear()
                with self.subTest(method=method, retries=retries), Client(domain=f"http://127.0.0.1:{server.server_port}", max_retries=retries) as pooled:
                    pooled.get_sandbox(SID)
                    with self.assertRaises(Error) as caught:
                        pooled.create_sandbox() if method == "POST" else pooled.get_sandbox(SID)
                    self.assertEqual(caught.exception.code, "transport")
                    self.assertEqual([call[0] for call in calls], ["GET"] + [method] * attempts)
                    self.assertEqual(calls[0][1], calls[1][1])
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
