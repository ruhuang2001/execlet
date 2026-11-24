"""Source-compatible E2B 2.64.0 subset over sandboxd, without E2B dependencies.

See examples/e2b/README.md for the supported subset and image prerequisites.
Native Client cancellation still cancels remote work; local observation failure
in this facade does not. Command deadlines are enforced remotely.
No cloud credentials, endpoints or template IDs are used.
"""
from __future__ import annotations

import builtins
import math
import os
import re
import secrets
import stat
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field, replace
from datetime import datetime
from enum import Enum
from typing import Any, Literal, overload

from . import CancellationToken, Client, CreateSandboxRequest, Error, ExecuteRequest, UpdateSandboxRequest
from ._models import Event, Execution, Sandbox as NativeSandbox

__all__ = [
    "Sandbox", "SandboxInfo", "CommandResult", "CommandHandle", "CommandExitException",
    "SandboxException", "NotFoundException", "SandboxNotFoundException",
    "FileNotFoundException", "AuthenticationException",
    "InvalidArgumentException", "TimeoutException", "AdapterError",
    "FileType", "WriteInfo", "EntryInfo",
]

_HOME = "/home/user"
_CONNECTION = {"endpoint", "api_key", "request_timeout"}


class SandboxException(Exception):
    status_code: int | None = None

    def __init__(self, message: str, *, status_code: int | None = None,
                 code: str = "", execution: Execution | None = None,
                 idempotency_key: str = ""):
        super().__init__(message)
        self.status_code = status_code
        self.code = code
        self.execution = execution
        self.execution_id = execution.id if execution else ""
        self.sandbox_id = execution.sandbox_id if execution else ""
        self.pid = execution.pid if execution else None
        self.idempotency_key = idempotency_key
        self.termination_reason = execution.termination_reason if execution else None
        self.output_truncated = execution.output_truncated if execution else False
        self.stdout = execution.stdout if execution else ""
        self.stderr = execution.stderr if execution else ""


class NotFoundException(SandboxException):
    pass


class SandboxNotFoundException(NotFoundException):
    pass


class FileNotFoundException(NotFoundException):
    pass


class AuthenticationException(SandboxException):
    pass


class InvalidArgumentException(SandboxException):
    pass


class TimeoutException(SandboxException):
    pass


class AdapterError(SandboxException):
    """Native limits, incomplete evidence and uncertainty outside E2B parity."""

    @property
    def exit_code(self) -> int | None:
        return self.execution.exit_code if self.execution else None


@dataclass
class CommandResult:
    stderr: str
    stdout: str
    exit_code: int
    error: str | None = None


@dataclass
class CommandExitException(SandboxException, CommandResult):
    def __str__(self) -> str:
        return f"Command exited with code {self.exit_code}: {self.stderr}"


class FileType(Enum):
    FILE = "file"
    DIR = "dir"
    SYMLINK = "symlink"


@dataclass
class WriteInfo:
    name: str
    type: FileType | None
    path: str
    metadata: dict[str, str] | None = field(default=None, kw_only=True)


@dataclass
class EntryInfo(WriteInfo):
    size: int
    mode: int
    permissions: str
    owner: str
    group: str
    modified_time: datetime
    symlink_target: str | None = None


@dataclass
class SandboxInfo:
    sandbox_id: str
    state: str
    template_id: str
    end_at: datetime


def _unknown(options: dict, allowed: set[str] | frozenset[str] = frozenset()) -> None:
    unknown = sorted(set(options) - allowed)
    if unknown:
        raise InvalidArgumentException("unsupported options: " + ", ".join(unknown))


def _text(value: Any, name: str) -> str:
    if not isinstance(value, str) or "\x00" in value:
        raise InvalidArgumentException(name + " must be text without NUL")
    try:
        value.encode("utf-8")
    except UnicodeError as exc:
        raise InvalidArgumentException(name + " must be UTF-8") from exc
    return value


def _duration(value: Any, *, lifetime: bool = False) -> float:
    if lifetime:
        if type(value) is not int or not 0 < value <= 9_223_372_036:
            raise InvalidArgumentException("lifetime must be positive integer seconds within the server duration range")
    elif type(value) not in (int, float) or not math.isfinite(value) or value < 0:
        raise InvalidArgumentException("timeout must be finite nonnegative seconds")
    return value


def _command_duration_ms(value: Any) -> int:
    if type(value) not in (int, float) or not 0 <= value <= 9_223_372_037:
        raise InvalidArgumentException("timeout must be finite nonnegative seconds within the server duration range")
    milliseconds = 0 if value == 0 else max(1, round(value * 1000))
    if milliseconds > 9_223_372_036_854:
        raise InvalidArgumentException("timeout exceeds the server duration range")
    return milliseconds


def _observation_token(milliseconds: int, entered: float) -> CancellationToken:
    return CancellationToken(timeout=(
        max(0, milliseconds / 1000 - (time.monotonic() - entered)) if milliseconds else None
    ))


def _pid(value: Any) -> int:
    if type(value) is not int or value <= 0:
        raise InvalidArgumentException("pid must be a positive integer")
    return value


def _callbacks(*values: Any) -> None:
    if any(value is not None and not callable(value) for value in values):
        raise InvalidArgumentException("output callbacks must be callable")


def _env(value: Any) -> dict[str, str] | None:
    if value is None:
        return None
    if not isinstance(value, dict):
        raise InvalidArgumentException("envs must be a string mapping")
    for key, item in value.items():
        if not _text(key, "environment name") or "=" in key:
            raise InvalidArgumentException("invalid environment name")
        _text(item, "environment value")
    return dict(value)


def _path(value: str) -> str:
    if not _text(value, "path") or ".." in value.split("/"):
        raise InvalidArgumentException("path must be nonempty and cannot contain parent traversal")
    target = value if value.startswith("/") else _HOME + "/" + value
    target = "/" + "/".join(p for p in target.split("/") if p not in ("", "."))
    if len(target.encode()) > 4096:
        raise InvalidArgumentException("path exceeds 4096 bytes")
    return target


def _translate(exc: Error, *, file: bool = False) -> SandboxException:
    cls: type[SandboxException] = AdapterError
    if exc.status_code == 401:
        cls = AuthenticationException
    elif exc.code in {"wait_timeout", "canceled"}:
        cls = TimeoutException
    elif exc.status_code == 404 or exc.code == "not_found":
        cls = FileNotFoundException if file else SandboxNotFoundException
    elif exc.code == "invalid_state":
        cls = SandboxNotFoundException
    elif exc.status_code == 400 or exc.code in {"config", "encode"}:
        cls = InvalidArgumentException
    return cls(str(exc), status_code=exc.status_code or None, code=exc.code,
               execution=exc.execution, idempotency_key=exc.idempotency_key)


def _client(options: dict) -> tuple[Client, float]:
    _unknown(options, _CONNECTION)
    seconds = _duration(options.get("request_timeout", 30))
    if seconds == 0:
        raise InvalidArgumentException("request_timeout must be positive")
    for key in ("endpoint", "api_key"):
        if key in options:
            _text(options[key], key)
    try:
        return Client(domain=options.get("endpoint", ""), api_key=options.get("api_key", "")), seconds
    except Error as exc:
        raise _translate(exc) from exc


def _date(value: str | None) -> datetime:
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00")) if value else None
        if parsed is not None and parsed.tzinfo is not None:
            return parsed
    except ValueError:
        pass
    raise AdapterError("server did not provide a valid timestamp", code="incompatible_server")


class Sandbox:
    """Basic Sandbox facade. Create with an explicit compatible image digest."""

    def __init__(self, sandbox: NativeSandbox, client: Client, request_timeout: float):
        self.sandbox_id = sandbox.id
        self._client = client
        self._request_timeout = request_timeout
        self.commands = _Commands(client, sandbox.id, request_timeout)
        self.files = _Files(client, sandbox.id, request_timeout)

    @classmethod
    def create(cls, template: str | None = None, *, timeout: int = 300,
               envs: dict[str, str] | None = None, **options) -> Sandbox:
        _unknown(options, _CONNECTION | {"image"})
        if template not in (None, "base"):
            raise InvalidArgumentException("only the local base image profile is supported")
        _duration(timeout, lifetime=True)
        environment = _env(envs)
        image = options.pop("image", None) or os.environ.get("SANDBOX_E2B_IMAGE")
        if not isinstance(image, str) or not re.fullmatch(r"(?:[^\s]+@)?sha256:[a-f0-9]{64}", image):
            raise InvalidArgumentException("image or SANDBOX_E2B_IMAGE must pin a compatibility image digest")
        client, seconds = _client(options)
        try:
            created = client.create_sandbox(CreateSandboxRequest(
                image=image, timeout_seconds=timeout, timeout_action="kill",
                idle_timeout_seconds=0, env=environment,
            ), cancel=CancellationToken(timeout=seconds))
        except Error as exc:
            raise _translate(exc) from exc
        return cls(created, client, seconds)

    @classmethod
    def connect(cls, sandbox_id: str, *, timeout: int = 300, **options) -> Sandbox:
        _text(sandbox_id, "sandbox_id")
        _duration(timeout, lifetime=True)
        client, seconds = _client(options)
        token = CancellationToken(timeout=seconds)
        try:
            current = client.get_sandbox(sandbox_id, cancel=token)
            if current.state != "running":
                raise SandboxNotFoundException("connect requires a running sandbox")
            current = client.update_sandbox(sandbox_id, UpdateSandboxRequest(extend_seconds=timeout), cancel=token)
        except Error as exc:
            raise _translate(exc) from exc
        return cls(current, client, seconds)

    @classmethod
    def get_info(cls, sandbox_id: str, **options) -> SandboxInfo:
        _text(sandbox_id, "sandbox_id")
        client, seconds = _client(options)
        try:
            value = client.get_sandbox(sandbox_id, cancel=CancellationToken(timeout=seconds))
            if value.state == "killed":
                raise SandboxNotFoundException("sandbox is no longer running")
            return SandboxInfo(value.id, value.state, "base", _date(value.expires_at))
        except Error as exc:
            raise _translate(exc) from exc
        finally:
            client.close()

    def set_timeout(self, timeout: int, **options) -> None:
        _unknown(options)
        _duration(timeout, lifetime=True)
        token = CancellationToken(timeout=self._request_timeout)
        try:
            if self._client.get_sandbox(self.sandbox_id, cancel=token).state != "running":
                raise SandboxNotFoundException("set_timeout requires a running sandbox")
            self._client.update_sandbox(self.sandbox_id, UpdateSandboxRequest(timeout_seconds=timeout), cancel=token)
        except Error as exc:
            raise _translate(exc) from exc

    def kill(self, **options) -> bool:
        _unknown(options)
        token = CancellationToken(timeout=self._request_timeout)
        try:
            current = self._client.get_sandbox(self.sandbox_id, cancel=token)
            self._client.kill_sandbox(self.sandbox_id, cancel=token)
            return current.state != "killed"
        except Error as exc:
            if exc.status_code == 404:
                return False
            raise _translate(exc) from exc

    def __enter__(self) -> Sandbox:
        return self

    def __exit__(self, _type, error, _traceback) -> None:
        try:
            self.kill()
        except Exception as cleanup:
            if error is not None:
                raise cleanup from error
            raise


def _require_commands(client: Client, token: CancellationToken) -> None:
    try:
        capabilities = client.get_capabilities(cancel=token).capabilities
    except Error as exc:
        if exc.status_code in {404, 405, 501} or exc.code == "decode":
            raise AdapterError("server does not support e2b.background.v1",
                               code="incompatible_server", status_code=exc.status_code or None) from exc
        raise
    if "e2b.background.v1" not in capabilities:
        raise AdapterError("server does not support e2b.background.v1", code="incompatible_server")


def _command_error(client: Client, sandbox_id: str, exc: Error,
                   token: CancellationToken) -> SandboxException:
    if exc.status_code == 404 or exc.code == "not_found":
        try:
            current = client.get_sandbox(sandbox_id, cancel=token)
        except Error as lookup:
            if lookup.execution is None:
                lookup.execution = exc.execution
            return _translate(lookup)
        cls = NotFoundException if current.state == "running" else SandboxNotFoundException
        return cls(str(exc), code=exc.code, status_code=exc.status_code or None,
                   execution=exc.execution, idempotency_key=exc.idempotency_key)
    return _translate(exc)


@dataclass(frozen=True)
class _CommandIdentity:
    sandbox_id: str
    execution_id: str
    pid: int


def _kill_command(client: Client, identity: _CommandIdentity, token: CancellationToken) -> bool:
    try:
        result = client.kill_execution(identity.sandbox_id, identity.execution_id, cancel=token)
        if result.execution.pid != identity.pid:
            raise Error("backend_error", "kill response identifies a different guest process")
        return result.signaled
    except Error as exc:
        if exc.execution is None:
            exc.execution = Execution(id=identity.execution_id, sandbox_id=identity.sandbox_id, pid=identity.pid)
        else:
            exc.execution = replace(exc.execution, id=identity.execution_id,
                                    sandbox_id=identity.sandbox_id, pid=identity.pid)
        translated = _command_error(client, identity.sandbox_id, exc, token)
        if type(translated) is NotFoundException:
            return False
        raise translated from exc


class CommandHandle:
    """One thread-confined observation, bound permanently to a managed command.

    Constructed by commands.run(background=True) or commands.connect(pid).
    Disconnect and local failures never signal the command. Use a new handle
    to observe again; kill always addresses this handle's original execution.
    """

    __slots__ = ("__identity", "_client", "_request_timeout", "_token", "_owner",
                 "_snapshot", "_cursor", "_stdout", "_stderr", "_result", "_error",
                 "_disconnected", "_waiting")

    def __init__(self, client: Client, execution: Execution, *, token: CancellationToken,
                 request_timeout: float, last_event_id: int = 0, attached: bool = False):
        if (not execution.id or not execution.sandbox_id or type(execution.pid) is not int
                or execution.pid <= 0 or execution.state not in {"running", "completed"}):
            raise AdapterError("server did not confirm command identity", code="backend_error", execution=execution)
        self.__identity = _CommandIdentity(execution.sandbox_id, execution.id, execution.pid)
        self._client, self._request_timeout, self._token = client, request_timeout, token
        self._owner = threading.current_thread()
        self._snapshot = replace(execution)
        self._cursor = last_event_id
        self._stdout: list[str] = []
        self._stderr: list[str] = []
        self._result: CommandResult | None = None
        self._error: BaseException | None = None
        self._disconnected = self._waiting = False
        if execution.output_truncated or (attached and execution.state == "completed"):
            if not attached:
                self._stdout.append(execution.stdout)
                self._stderr.append(execution.stderr)
            try:
                self._result = self._finish(execution)
            except SandboxException as exc:
                self._error = exc
            self._token.cancel()

    @property
    def sandbox_id(self) -> str:
        return self.__identity.sandbox_id

    @property
    def execution_id(self) -> str:
        """tiny-sandbox extension for diagnostics and native recovery."""
        return self.__identity.execution_id

    @property
    def pid(self) -> int:
        return self.__identity.pid

    def _check_thread(self) -> None:
        if threading.current_thread() is not self._owner:
            raise AdapterError("cross-thread command handle use is unsupported",
                               code="unsupported_operation", execution=self._evidence())

    def _evidence(self) -> Execution:
        return replace(self._snapshot, id=self.execution_id, sandbox_id=self.sandbox_id, pid=self.pid,
                       stdout="".join(self._stdout), stderr="".join(self._stderr))

    def _finish(self, execution: Execution) -> CommandResult:
        if (execution.id != self.execution_id or execution.sandbox_id != self.sandbox_id
                or execution.pid != self.pid):
            raise AdapterError("terminal snapshot identifies a different command",
                               code="backend_error", execution=self._evidence())
        self._snapshot = replace(execution)
        evidence = self._evidence()
        if evidence.output_truncated:
            raise AdapterError("command output was truncated", code="output_truncated", execution=evidence)
        if (evidence.state != "completed" or evidence.termination_reason != "exited"
                or type(evidence.exit_code) is not int):
            raise AdapterError("command did not report an ordinary exit",
                               code=evidence.termination_reason or "incomplete", execution=evidence)
        result = CommandResult(evidence.stderr, evidence.stdout, evidence.exit_code)
        if result.exit_code != 0:
            error = CommandExitException(result.stderr, result.stdout, result.exit_code, result.error)
            SandboxException.__init__(error, str(error), code="nonzero_exit", execution=evidence)
            raise error
        return result

    def wait(self, *, on_stdout: Callable[[str], None] | None = None,
             on_stderr: Callable[[str], None] | None = None, **options) -> CommandResult:
        _unknown(options)
        _callbacks(on_stdout, on_stderr)
        self._check_thread()
        if self._waiting:
            raise AdapterError("simultaneous command consumers are unsupported",
                               code="unsupported_operation", execution=self._evidence())
        if self._error is not None:
            raise self._error
        if self._result is not None:
            return self._result
        if self._disconnected:
            raise AdapterError("command handle is disconnected", code="disconnected", execution=self._evidence())

        def event(value: Event) -> None:
            if value.type == "stdout":
                self._stdout.append(value.data)
                if on_stdout is not None:
                    on_stdout(value.data)
            elif value.type == "stderr":
                self._stderr.append(value.data)
                if on_stderr is not None:
                    on_stderr(value.data)
            elif value.type == "truncated":
                self._snapshot.output_truncated = True
                raise AdapterError("command output was truncated", code="output_truncated", execution=self._evidence())
            if self._disconnected:
                raise AdapterError("command handle is disconnected", code="disconnected", execution=self._evidence())
            self._cursor = value.id

        self._waiting = True
        try:
            try:
                execution = self._client.stream_execution(
                    self.sandbox_id, self.execution_id, on_event=event, cancel=self._token,
                    cancel_remote=False, last_event_id=self._cursor,
                )
            except Error as exc:
                if exc.code == "callback" and exc.cause is not None:
                    raise exc.cause
                exc.execution = self._evidence()
                raise _command_error(self._client, self.sandbox_id, exc, self._token) from exc
            self._result = self._finish(execution)
            return self._result
        except BaseException as exc:
            self._error = exc
            raise
        finally:
            self._waiting = False
            self._token.cancel()

    def disconnect(self, **options) -> None:
        _unknown(options)
        self._check_thread()
        self._disconnected = True
        self._token.cancel()

    def kill(self, **options) -> bool:
        _unknown(options)
        self._check_thread()
        token = CancellationToken(timeout=self._request_timeout)
        try:
            _require_commands(self._client, token)
        except Error as exc:
            exc.execution = self._evidence()
            raise _translate(exc) from exc
        except SandboxException as exc:
            exc.execution = self._evidence()
            exc.execution_id, exc.sandbox_id, exc.pid = self.execution_id, self.sandbox_id, self.pid
            raise
        return _kill_command(self._client, self.__identity, token)


class _Commands:
    def __init__(self, client: Client, sandbox_id: str, request_timeout: float):
        self._client, self._id, self._request_timeout = client, sandbox_id, request_timeout

    @overload
    def run(self, cmd: str, *, background: Literal[True], envs: dict[str, str] | None = None,
            cwd: str | None = None, timeout: float = 60, on_stdout: None = None,
            on_stderr: None = None, stdin: Literal[False] = False,
            retain_output: Literal[True] = True) -> CommandHandle: ...

    @overload
    def run(self, cmd: str, *, background: Literal[False] = False, envs: dict[str, str] | None = None,
            cwd: str | None = None, timeout: float = 60, on_stdout: Callable[[str], None] | None = None,
            on_stderr: Callable[[str], None] | None = None, stdin: Literal[False] = False,
            retain_output: Literal[True] = True) -> CommandResult: ...

    @overload
    def run(self, cmd: str, *, background: bool, envs: dict[str, str] | None = None,
            cwd: str | None = None, timeout: float = 60, on_stdout: Callable[[str], None] | None = None,
            on_stderr: Callable[[str], None] | None = None, stdin: Literal[False] = False,
            retain_output: Literal[True] = True) -> CommandResult | CommandHandle: ...

    def run(self, cmd: str, *, envs: dict[str, str] | None = None, cwd: str | None = None,
            timeout: float = 60, on_stdout: Callable[[str], None] | None = None,
            on_stderr: Callable[[str], None] | None = None, background: bool = False,
            stdin: bool = False, retain_output: bool = True, **options) -> CommandResult | CommandHandle:
        entered = time.monotonic()
        _unknown(options)
        _text(cmd, "command")
        milliseconds = _command_duration_ms(timeout)
        environment = _env(envs)
        if type(background) is not bool or stdin is not False or retain_output is not True:
            raise InvalidArgumentException("background must be boolean; only closed stdin and retained output are supported")
        if cwd is not None and not _text(cwd, "cwd").startswith("/"):
            raise InvalidArgumentException("cwd must be absolute")
        _callbacks(on_stdout, on_stderr)
        if background and (on_stdout is not None or on_stderr is not None):
            raise InvalidArgumentException("background callbacks belong on handle.wait")
        token = _observation_token(milliseconds, entered)
        request = ExecuteRequest(
            command=["/bin/bash", "-l", "-c", cmd], env=environment, cwd=cwd or _HOME,
            timeout_ms=milliseconds, idempotency_key=secrets.token_hex(16),
        )
        try:
            started = self._client.start_execution(self._id, request, cancel=token, cancel_remote=False)
            if started.sandbox_id != self._id:
                raise AdapterError("start response identifies a different sandbox", code="backend_error",
                                   idempotency_key=request.idempotency_key)
            try:
                handle = CommandHandle(self._client, started, token=token, request_timeout=self._request_timeout)
            except AdapterError as exc:
                exc.idempotency_key = request.idempotency_key
                raise
        except Error as exc:
            raise _translate(exc) from exc
        if background:
            return handle
        return handle.wait(on_stdout=on_stdout, on_stderr=on_stderr)

    def connect(self, pid: int, *, timeout: float = 60, **options) -> CommandHandle:
        entered = time.monotonic()
        _unknown(options)
        _pid(pid)
        milliseconds = _command_duration_ms(timeout)
        token = _observation_token(milliseconds, entered)
        try:
            _require_commands(self._client, token)
            attached = self._client.get_command(self._id, pid, cancel=token)
        except Error as exc:
            raise _command_error(self._client, self._id, exc, token) from exc
        return CommandHandle(self._client, attached.execution, token=token,
                             request_timeout=self._request_timeout, last_event_id=attached.last_event_id, attached=True)

    def kill(self, pid: int, **options) -> bool:
        _unknown(options)
        _pid(pid)
        token = CancellationToken(timeout=self._request_timeout)
        try:
            _require_commands(self._client, token)
            attached = self._client.get_command(self._id, pid, cancel=token)
        except Error as exc:
            translated = _command_error(self._client, self._id, exc, token)
            if type(translated) is NotFoundException:
                return False
            raise translated from exc
        identity = _CommandIdentity(self._id, attached.execution.id, pid)
        return _kill_command(self._client, identity, token)


class _Files:
    def __init__(self, client: Client, sandbox_id: str, request_timeout: float):
        self._client, self._id, self._request_timeout = client, sandbox_id, request_timeout

    def _call(self, operation, token):
        try:
            return operation()
        except Error as exc:
            if exc.status_code == 404:
                try:
                    current = self._client.get_sandbox(self._id, cancel=token)
                except Error as lookup:
                    raise _translate(lookup) from lookup
                if current.state != "running":
                    raise SandboxNotFoundException("sandbox is no longer running") from exc
            raise _translate(exc, file=True) from exc

    def write(self, path: str, data: str | bytes | bytearray, **options) -> WriteInfo:
        _unknown(options)
        target = _path(path)
        if not isinstance(data, (str, bytes, bytearray)):
            raise InvalidArgumentException("data must be str, bytes or bytearray")
        try:
            payload = data.encode("utf-8") if isinstance(data, str) else bytes(data)
        except UnicodeError as exc:
            raise InvalidArgumentException("text data must be UTF-8") from exc
        if len(payload) > 10 << 20:
            raise AdapterError("file exceeds 10 MiB", code="payload_too_large")
        token = CancellationToken(timeout=self._request_timeout)
        entry = self._call(lambda: self._client.write_file(self._id, target, payload, cancel=token), token)
        if entry is None:
            return WriteInfo(target.rsplit("/", 1)[-1], None, target)
        if entry.type not in {"file", "symlink"}:
            raise AdapterError("unexpected type in write response", code="incompatible_server")
        return WriteInfo(entry.name, FileType(entry.type), target)

    @overload
    def read(self, path: str, *, format: Literal["text"] = "text") -> str: ...
    @overload
    def read(self, path: str, *, format: Literal["bytes"]) -> bytearray: ...
    def read(self, path: str, *, format: str = "text", **options) -> str | bytearray:
        _unknown(options)
        target = _path(path)
        if format not in ("text", "bytes"):
            raise InvalidArgumentException("format must be text or bytes")
        token = CancellationToken(timeout=self._request_timeout)
        data = self._call(lambda: self._client.read_file(self._id, target, cancel=token), token)
        return bytearray(data) if format == "bytes" else data.decode("utf-8", "replace")

    def list(self, path: str, *, depth: int = 1, **options) -> list[EntryInfo]:
        _unknown(options)
        target = _path(path)
        if type(depth) is not int or depth != 1:
            raise InvalidArgumentException("only depth 1 is supported")
        token = CancellationToken(timeout=self._request_timeout)
        self._call(lambda: self._client.require_capabilities("files.list.v1", cancel=token), token)
        return self._list(target, token)

    def _list(self, target: str, token: CancellationToken) -> builtins.list[EntryInfo]:
        entries: list[EntryInfo] = []
        cursor, seen = "", set()
        while True:
            page = self._call(lambda: self._client._list_files_page(self._id, target, 1000, cursor, token), token)
            for entry in page.entries:
                extra = entry.extra
                owner = entry.owner if entry.owner is not None else extra.get("owner")
                group = entry.group if entry.group is not None else extra.get("group")
                if not isinstance(owner, str) or not isinstance(group, str):
                    raise AdapterError("guest owner/group metadata is unavailable", code="incompatible_server")
                kind = entry.type if entry.type is not None else extra.get("type")
                if kind not in {"file", "dir", "symlink", "other"}:
                    raise AdapterError("guest file type is unavailable", code="incompatible_server")
                symlink_target = entry.symlink_target if entry.symlink_target is not None else extra.get("symlink_target")
                entries.append(EntryInfo(
                    name=entry.name, path=target.rstrip("/") + "/" + entry.name,
                    type=None if kind == "other" else FileType(kind), size=entry.size, mode=entry.mode,
                    permissions=stat.filemode(stat.S_IFREG | (entry.mode & 0o7777))[1:],
                    owner=owner, group=group, modified_time=_date(entry.mtime),
                    symlink_target=symlink_target,
                ))
            if len(entries) > 10000:
                raise AdapterError("directory exceeds 10000 entries", code="payload_too_large")
            cursor = page.next_cursor
            if not cursor:
                return entries
            if cursor in seen:
                raise AdapterError("directory cursor repeated", code="incompatible_server")
            seen.add(cursor)
