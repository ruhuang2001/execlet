"""Typed endpoint operations shared by the synchronous client."""

from __future__ import annotations

import secrets
from abc import ABC, abstractmethod
from contextlib import contextmanager
from dataclasses import replace
from functools import wraps
from typing import Any, Callable, Iterator, NoReturn, ParamSpec, TypeVar
from urllib.parse import quote, urlencode

from ._cancel import CancellationToken, _Cancellation
from ._errors import Error
from ._models import (
    Capabilities, CommandAttachment, CreateSandboxRequest, ExecuteRequest, Execution,
    ExecutionKillResult, FileEntry, FileList, Sandbox, UpdateSandboxRequest,
)
from ._transport import Transport

_P = ParamSpec("_P")
_T = TypeVar("_T")


def _public_cancellation(method: Callable[_P, _T]) -> Callable[_P, _T]:
    """Keep the transport's cancellation signal out of the public API."""

    @wraps(method)
    def call(*args: _P.args, **kwargs: _P.kwargs) -> _T:
        try:
            return method(*args, **kwargs)
        except _Cancellation as exc:
            raise Error("canceled", cause=exc) from exc

    return call


def _resource_items(payload: dict[str, Any], field: str) -> list[dict[str, Any]]:
    if field not in payload:
        raise Error("decode", f"Response is missing {field!r}")
    items = payload[field]
    if items is None:
        return []
    if not isinstance(items, list) or any(not isinstance(item, dict) for item in items):
        raise Error("decode", f"Response field {field!r} must be an array of objects")
    return items


class SandboxAPI(Transport, ABC):
    """Resource methods using the client's transport and cleanup helpers."""

    @staticmethod
    def _sandbox_path(sandbox_id: str) -> str:
        return "/v1/sandboxes/" + quote(sandbox_id, safe="")

    def _execution_path(self, sandbox_id: str, execution_id: str) -> str:
        return self._sandbox_path(sandbox_id) + "/executions/" + quote(
            execution_id, safe=""
        )

    @_public_cancellation
    def get_capabilities(self, *, cancel: CancellationToken | None = None) -> Capabilities:
        """Read this endpoint's authenticated feature advertisement; never cached."""
        return Capabilities.from_dict(self._json("GET", "/v1/capabilities", cancel=cancel))

    def require_capabilities(self, *required: str, cancel: CancellationToken | None = None) -> None:
        if not required:
            return
        try:
            flags = self.get_capabilities(cancel=cancel).capabilities
        except Error as exc:
            if exc.status_code in {404, 405, 501} or (not exc.status_code and exc.code == "decode"):
                raise Error("incompatible_server", "server does not advertise required capabilities", cause=exc) from exc
            raise
        missing = set(required).difference(flags)
        if missing:
            raise Error("incompatible_server", "server lacks " + ", ".join(sorted(missing)))

    @_public_cancellation
    def get_command(
        self, sandbox_id: str, pid: int, *, cancel: CancellationToken | None = None,
    ) -> CommandAttachment:
        """Resolve a managed guest PID and its atomic live-attach event boundary."""
        if type(pid) is not int or pid <= 0:
            raise Error("invalid_request", "pid must be a positive integer")
        self.require_capabilities("e2b.background.v1", cancel=cancel)
        payload = self._json(
            "GET", self._sandbox_path(sandbox_id) + "/commands/" + str(pid), cancel=cancel,
        )
        execution = Execution.from_dict(payload.get("execution"))
        attached = CommandAttachment.from_dict({**payload, "execution": execution})
        if execution.sandbox_id != sandbox_id or execution.pid != pid:
            raise Error("decode", "command attachment identifies a different command")
        if not 0 <= attached.last_event_id < 2**64:
            raise Error("decode", "command attachment has an invalid event boundary", execution=execution)
        if execution.state not in {"running", "completed"}:
            raise Error("decode", "command attachment has an invalid state", execution=execution)
        return attached

    def kill_execution(
        self, sandbox_id: str, execution_id: str, *, cancel: CancellationToken | None = None,
    ) -> ExecutionKillResult:
        """Request immediate group kill once; true requires a confirmed terminal outcome.

        Unlike graceful cancel, an uncertain acknowledgement is never retried
        automatically: a later 'already done' response cannot explain the loss.
        """
        identity = Execution(id=execution_id, sandbox_id=sandbox_id)
        try:
            self.require_capabilities("e2b.background.v1", cancel=cancel)
            payload = self._json(
                "POST", self._execution_path(sandbox_id, execution_id) + "/kill",
                cancel=cancel, retry=False,
            )
            execution = Execution.from_dict(payload.get("execution"))
            result = ExecutionKillResult.from_dict({**payload, "execution": execution})
            if execution.id != execution_id or execution.sandbox_id != sandbox_id:
                raise Error("backend_error", "kill response identifies a different execution")
            identity = execution
            if execution.state != "completed" or not execution.termination_reason:
                raise Error("backend_error", "command termination was not confirmed")
            if result.signaled and execution.termination_reason != "canceled":
                raise Error("backend_error", "kill response does not confirm the requested signal")
            return result
        except _Cancellation as exc:
            raise Error("backend_error", "kill acknowledgement was not confirmed",
                        execution=identity, cause=exc) from exc
        except Error as exc:
            if exc.code in {"decode", "transport"}:
                raise Error("backend_error", "kill acknowledgement was not confirmed",
                            execution=identity, cause=exc) from exc
            if exc.execution is None:
                exc.execution = identity
            raise

    @_public_cancellation
    def create_sandbox(
        self,
        request: CreateSandboxRequest | None = None,
        *,
        cancel: CancellationToken | None = None,
    ) -> Sandbox:
        """Create a sandbox once; creation is never automatically retried."""
        payload = (request if request is not None else CreateSandboxRequest()).to_dict()
        required = []
        if payload.get("runtime"):
            required.append("sandbox.runtime.v1")
        if payload.get("env"):
            required.append("execution.env-cwd.v1")
        if payload.get("max_concurrent_executions") is not None:
            required.append("execution.concurrent.v1")
        self.require_capabilities(*required, cancel=cancel)
        return Sandbox.from_dict(
            self._json("POST", "/v1/sandboxes", json_body=payload, cancel=cancel, retry=False)
        )

    @_public_cancellation
    def get_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("GET", self._sandbox_path(sandbox_id), cancel=cancel)
        )

    @_public_cancellation
    def update_sandbox(
        self, sandbox_id: str, request: UpdateSandboxRequest, *,
        cancel: CancellationToken | None = None,
    ) -> Sandbox:
        """Replace or extend the lifetime once; relative deadlines are not replayed."""
        return Sandbox.from_dict(self._json(
            "PATCH", self._sandbox_path(sandbox_id), json_body=request.to_dict(),
            cancel=cancel, retry=False,
        ))

    @_public_cancellation
    def list_sandboxes(
        self, *, cancel: CancellationToken | None = None
    ) -> list[Sandbox]:
        payload = self._json("GET", "/v1/sandboxes", cancel=cancel)
        return [Sandbox.from_dict(item) for item in _resource_items(payload, "sandboxes")]

    @_public_cancellation
    def pause_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("POST", self._sandbox_path(sandbox_id) + "/pause", cancel=cancel)
        )

    @_public_cancellation
    def resume_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("POST", self._sandbox_path(sandbox_id) + "/resume", cancel=cancel)
        )

    @_public_cancellation
    def kill_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("DELETE", self._sandbox_path(sandbox_id), cancel=cancel)
        )

    def start_execution(
        self,
        sandbox_id: str,
        request: ExecuteRequest,
        *,
        cancel: CancellationToken | None = None,
        cancel_remote: bool = True,
    ) -> Execution:
        """Start once across retries; cancel_remote=False only abandons the wait."""
        keyed_request = replace(
            request,
            command=list(request.command),
            env=dict(request.env) if request.env is not None else None,
            idempotency_key=request.idempotency_key or secrets.token_hex(16),
        )
        if cancel is not None:
            try:
                cancel._check()
            except _Cancellation as exc:
                raise Error("canceled", cause=exc) from exc
        body = keyed_request.to_dict()
        required = []
        if keyed_request.env or keyed_request.cwd:
            required.append("execution.env-cwd.v1")
        if keyed_request.stdin:
            required.append("execution.stdin.v1")
        if keyed_request.timeout_ms is not None:
            required.append("e2b.background.v1")
        self.require_capabilities(*required, cancel=cancel)
        try:
            payload = self._json(
                "POST",
                self._sandbox_path(sandbox_id) + "/executions",
                json_body=body,
                cancel=cancel,
            )
            try:
                execution = Execution.from_dict(payload)
                if execution.sandbox_id != sandbox_id:
                    raise Error("decode", "start response identifies a different sandbox")
                return execution
            except Error as exc:
                execution_id = payload.get("id")
                if isinstance(execution_id, str) and execution_id:
                    exc.execution = Execution(id=execution_id, sandbox_id=sandbox_id)
                raise
        except _Cancellation as exc:
            if not cancel_remote:
                raise Error(
                    "wait_timeout", "stopped waiting; remote start may have been accepted",
                    idempotency_key=keyed_request.idempotency_key, cause=exc,
                ) from exc
            if not exc.dispatched:
                raise Error("canceled", cause=exc) from exc
            return self._cancel_remote(
                sandbox_id, execution_id=None, request=keyed_request, cause=exc
            )
        except Error as exc:
            if not exc.idempotency_key:
                exc.idempotency_key = keyed_request.idempotency_key
            raise

    @_public_cancellation
    def get_execution(
        self,
        sandbox_id: str,
        execution_id: str,
        *,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        return Execution.from_dict(
            self._json("GET", self._execution_path(sandbox_id, execution_id), cancel=cancel)
        )

    @_public_cancellation
    def list_executions(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> list[Execution]:
        payload = self._json(
            "GET", self._sandbox_path(sandbox_id) + "/executions", cancel=cancel
        )
        return [Execution.from_dict(item) for item in _resource_items(payload, "executions")]

    @_public_cancellation
    def cancel_execution(
        self,
        sandbox_id: str,
        execution_id: str,
        *,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        """Request cancellation and return the server's latest execution snapshot."""
        return self._cancel_execution(sandbox_id, execution_id, cancel=cancel, retry=True)

    def _cancel_execution(self, sandbox_id, execution_id, *, cancel, retry) -> Execution:
        return Execution.from_dict(
            self._json(
                "POST",
                self._execution_path(sandbox_id, execution_id) + "/cancel",
                cancel=cancel, retry=retry,
            )
        )

    @_public_cancellation
    def list_files(
        self, sandbox_id: str, path: str, *, limit: int = 100, cursor: str = "",
        cancel: CancellationToken | None = None,
    ) -> FileList:
        """Return one name-ordered page of immediate directory children."""
        self.require_capabilities("files.list.v1", cancel=cancel)
        return self._list_files_page(sandbox_id, path, limit, cursor, cancel)

    @_public_cancellation
    def _list_files_page(self, sandbox_id, path, limit, cursor, cancel) -> FileList:
        query = urlencode({"path": path, "limit": limit, "cursor": cursor})
        payload = self._json("GET", self._sandbox_path(sandbox_id) + "/files/list?" + query, cancel=cancel)
        return FileList.from_dict(payload)

    @_public_cancellation
    def read_archive(
        self, sandbox_id: str, path: str, *, cancel: CancellationToken | None = None,
    ) -> bytes:
        """Download a validated tar rooted at the selected file/directory name."""
        self.require_capabilities("files.archive.v1", cancel=cancel)
        return self._request("GET", self._sandbox_path(sandbox_id) + "/files/archive?" + urlencode({"path": path}), cancel=cancel)

    @_public_cancellation
    def write_archive(
        self, sandbox_id: str, path: str, data: bytes, *, cancel: CancellationToken | None = None,
    ) -> None:
        """Extract validated tar beneath an existing directory, without retries."""
        self.require_capabilities("files.archive.v1", cancel=cancel)
        self._request("PUT", self._sandbox_path(sandbox_id) + "/files/archive?" + urlencode({"path": path}), body=data, cancel=cancel, retry=False)

    @_public_cancellation
    def read_file(
        self,
        sandbox_id: str,
        path: str,
        *,
        cancel: CancellationToken | None = None,
    ) -> bytes:
        return self._request(
            "GET",
            self._sandbox_path(sandbox_id) + "/files?" + urlencode({"path": path}),
            cancel=cancel,
        )

    @_public_cancellation
    def write_file(
        self,
        sandbox_id: str,
        path: str,
        data: bytes,
        *,
        cancel: CancellationToken | None = None,
    ) -> FileEntry:
        payload = self._json(
            "PUT",
            self._sandbox_path(sandbox_id) + "/files?" + urlencode({"path": path}),
            body=data,
            cancel=cancel,
        )
        return FileEntry.from_dict(payload)

    @contextmanager
    def sandbox(
        self,
        request: CreateSandboxRequest | None = None,
        *,
        cancel: CancellationToken | None = None,
    ) -> Iterator[Sandbox]:
        """Create a sandbox and kill it on exit with an independent cleanup budget."""
        sandbox = self.create_sandbox(request, cancel=cancel)
        try:
            yield sandbox
        except BaseException as body_error:
            try:
                self.kill_sandbox(sandbox.id, cancel=CancellationToken(timeout=5))
            except BaseException as cleanup_error:
                raise body_error from cleanup_error
            raise
        else:
            self.kill_sandbox(sandbox.id, cancel=CancellationToken(timeout=5))

    @abstractmethod
    def _cancel_remote(
        self, sandbox_id: str, execution_id: str | None, *,
        request: ExecuteRequest | None = None, cause: BaseException | None = None,
    ) -> NoReturn:
        """Recover and confirm cancellation, then raise its typed outcome."""
        raise NotImplementedError
