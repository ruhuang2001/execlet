"""Synchronous SDK orchestration over the language-neutral HTTP contract."""

from __future__ import annotations

import base64
import json
from collections.abc import Callable,Iterator
from typing import NoReturn

from ._api import SandboxAPI
from ._cancel import CancellationToken, _Cancellation
from ._errors import Error, outcome_error
from ._models import Event, ExecuteRequest, Execution
from ._stream import read_event, read_frame
from ._transport import _backoff, _sleep, _status_error


class Client(SandboxAPI):
    """Client for sandboxd. See docs/sdk-python.md for the public contract."""

    def stream_terminal(
        self,
        sandbox_id: str,
        terminal_id: str,
        *,
        offset: int = 0,
        cancel: CancellationToken | None = None,
    ) -> Iterator[bytes]:
        self.require_capabilities("terminals.v1", cancel=cancel)
        headers = {"Accept": "text/event-stream", "Last-Event-ID": str(offset)}
        try:
            with self._exchange(
                "GET",
                self._terminal_path(sandbox_id, terminal_id) + "/events",
                headers=headers,
                cancel=cancel,
            ) as response:
                if response.status != 200:
                    raise _status_error(response.status, response.read())
                while True:
                    event_id, kind, text = read_frame(response)
                    if kind == "closed":
                        return
                    if kind == "stdout" and event_id is not None and event_id > offset:
                        try:
                            data = base64.b64decode(
                                json.loads(text)["data"], validate=True
                            )
                        except (ValueError, KeyError, TypeError) as err:
                            raise Error(
                                "stream", "invalid terminal frame", cause=err
                            ) from err
                        offset = event_id
                        yield data
        except _Cancellation as err:
            raise Error("canceled", cause=err) from err

    def exec(
        self,
        sandbox_id: str,
        command: list[str],
        *,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        return self.exec_stream(sandbox_id, command, cancel=cancel)

    def exec_stream(
        self, sandbox_id: str, command: list[str], *,
        on_event: Callable[[Event], None] | None = None,
        on_stdout: Callable[[str], None] | None = None,
        on_stderr: Callable[[str], None] | None = None,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        started = self.start_execution(sandbox_id, ExecuteRequest(command=command), cancel=cancel)
        execution = self.stream_execution(
            sandbox_id, started.id, on_event=on_event, on_stdout=on_stdout,
            on_stderr=on_stderr, cancel=cancel,
        )
        error = outcome_error(execution)
        if error is not None:
            raise error
        return execution

    def stream_execution(
        self, sandbox_id: str, execution_id: str, *,
        on_event: Callable[[Event], None] | None = None,
        on_stdout: Callable[[str], None] | None = None,
        on_stderr: Callable[[str], None] | None = None,
        cancel: CancellationToken | None = None,
        cancel_remote: bool = True,
        last_event_id: int = 0,
    ) -> Execution:
        """Observe after last_event_id; reconnect only after delivered events."""
        if type(last_event_id) is not int or not 0 <= last_event_id < 2**64:
            raise Error("invalid_request", "last_event_id must be a nonnegative uint64 integer")
        cursor = last_event_id
        try:
            for attempt in range(self._max_retries + 1):
                headers = {"Accept": "text/event-stream"}
                if cursor:
                    headers["Last-Event-ID"] = str(cursor)
                try:
                    with self._exchange(
                        "GET", self._execution_path(sandbox_id, execution_id) + "/events",
                        headers=headers, cancel=cancel,
                    ) as response:
                        if response.status != 200:
                            raise _status_error(response.status, response.read())
                        while True:
                            if cancel is not None:
                                cancel._check()
                            event = read_event(response)
                            if event.id <= cursor:
                                continue
                            if event.type not in {"stdout", "stderr", "truncated", "complete"}:
                                cursor = event.id
                                continue
                            if event.execution is not None and (
                                event.execution.id != execution_id or event.execution.sandbox_id != sandbox_id
                            ):
                                raise Error("stream", "complete event identifies a different execution")
                            try:
                                if on_event is not None:
                                    on_event(event)
                                if event.type == "stdout" and on_stdout is not None:
                                    on_stdout(event.data)
                                if event.type == "stderr" and on_stderr is not None:
                                    on_stderr(event.data)
                            except Exception as exc:
                                raise Error("callback", "event callback failed", cause=exc) from exc
                            cursor = event.id
                            if event.execution is not None:
                                return event.execution
                except Error as exc:
                    if exc.code == "callback":
                        raise
                    if cancel is not None:
                        cancel._check()
                    if not exc.retryable:
                        raise
                    if attempt == self._max_retries:
                        raise Error(
                            "stream", "stream reconnect budget exhausted",
                            status_code=exc.status_code, cause=exc,
                        ) from exc
                _sleep(_backoff(attempt), cancel)
        except _Cancellation as exc:
            if not cancel_remote:
                raise Error(
                    "wait_timeout", "stopped waiting; remote execution was not canceled",
                    execution=Execution(id=execution_id, sandbox_id=sandbox_id), cause=exc,
                ) from exc
            return self._cancel_remote(sandbox_id, execution_id, cause=exc)
        raise Error("stream", "stream ended without completion")

    def _cancel_remote(
        self, sandbox_id: str, execution_id: str | None, *,
        request: ExecuteRequest | None = None, cause: BaseException | None = None,
    ) -> NoReturn:
        """Recover and confirm under one fresh five-second total deadline."""
        cleanup = CancellationToken(timeout=5)
        key = request.idempotency_key if request is not None else ""
        execution = Execution(id=execution_id or "", sandbox_id=sandbox_id)
        try:
            if request is not None:
                recovered = Execution.from_dict(self._json(
                    "POST", self._sandbox_path(sandbox_id) + "/executions",
                    json_body=request.to_dict(), cancel=cleanup,
                ))
                if recovered.sandbox_id != sandbox_id:
                    raise Error("decode", "recovered start identifies another sandbox")
                execution = recovered
                execution_id = execution.id
            if not execution_id:
                raise Error("decode", "cancellation requires an execution id")
            snapshot = self._cancel_execution(sandbox_id, execution_id, cancel=cleanup, retry=False)
            while True:
                if snapshot.id != execution_id or snapshot.sandbox_id != sandbox_id:
                    raise Error("decode", "cancellation snapshot identifies another execution")
                execution = snapshot
                if execution.state != "running":
                    break
                _sleep(0.05, cleanup)
                snapshot = self.get_execution(sandbox_id, execution_id, cancel=cleanup)
            reason = execution.termination_reason
            if execution.state != "completed" or reason not in {"canceled", "timeout", "oom", "evicted", "exited"} or (
                reason == "exited" and execution.exit_code is None
            ):
                raise Error("backend_error", "cancellation snapshot did not confirm a known terminal outcome")
        except (Error, _Cancellation) as exc:
            raise Error(
                "cancel_unconfirmed", "could not confirm that remote execution stopped",
                execution=execution, idempotency_key=key, cause=exc,
            ) from exc
        raise Error(
            "canceled", "caller canceled; remote execution is terminal",
            execution=execution, idempotency_key=key, cause=cause,
        )
