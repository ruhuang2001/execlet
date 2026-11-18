"""Source-compatible E2B 2.64.0 subset over sandboxd, without E2B dependencies.

See docs/e2b-compatibility.md for the allowlist and image prerequisites.
Native Client cancellation still cancels remote work; this facade only stops
waiting. No cloud credentials, endpoints or template IDs are used.
"""
from __future__ import annotations

import math
import os
import re
import stat
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Literal, overload

from . import CancellationToken, Client, CreateSandboxRequest, Error, ExecuteRequest, UpdateSandboxRequest
from ._models import Event, Execution, Sandbox as NativeSandbox

__all__ = [
    "Sandbox", "SandboxInfo", "CommandResult", "CommandExitException",
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
        self.execution_id = execution.id if execution else ""
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


def _unknown(options: dict, allowed: set[str] = frozenset()) -> None:
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
        self.commands = _Commands(client, sandbox.id)
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
            # A killed record is not evidence of removal. DELETE must confirm
            # even on repeat calls, or a pending removal would become false.
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


class _Commands:
    def __init__(self, client: Client, sandbox_id: str):
        self._client, self._id = client, sandbox_id

    def run(self, cmd: str, *, envs: dict[str, str] | None = None, cwd: str | None = None,
            timeout: float = 60, on_stdout: Callable[[str], None] | None = None,
            on_stderr: Callable[[str], None] | None = None, background: bool = False,
            stdin: bool = False, retain_output: bool = True, **options) -> CommandResult:
        _unknown(options)
        _text(cmd, "command")
        _duration(timeout)
        environment = _env(envs)
        if background is not False or stdin is not False or retain_output is not True:
            raise InvalidArgumentException("only foreground, closed stdin and retained output are supported")
        if cwd is not None and (not _text(cwd, "cwd").startswith("/")):
            raise InvalidArgumentException("cwd must be absolute")
        if any(cb is not None and not callable(cb) for cb in (on_stdout, on_stderr)):
            raise InvalidArgumentException("output callbacks must be callable")
        token = CancellationToken(timeout=timeout) if timeout else None
        partial = Execution(id="", sandbox_id=self._id)
        stdout, stderr = [], []

        def event(value: Event) -> None:
            if value.type == "stdout":
                stdout.append(value.data)
            elif value.type == "stderr":
                stderr.append(value.data)
            elif value.type == "truncated":
                partial.stdout, partial.stderr = "".join(stdout), "".join(stderr)
                partial.output_truncated = True
                raise AdapterError("command output was truncated", code="output_truncated", execution=partial)

        try:
            started = self._client.start_execution(self._id, ExecuteRequest(
                command=["/bin/bash", "-l", "-c", cmd], env=environment, cwd=cwd or _HOME,
            ), cancel=token, cancel_remote=False)
            partial.id = started.id
            execution = self._client.stream_execution(
                self._id, started.id, on_event=event, on_stdout=on_stdout, on_stderr=on_stderr,
                cancel=token, cancel_remote=False,
            )
        except Error as exc:
            if exc.code == "callback" and exc.cause is not None:
                raise exc.cause
            translated = _translate(exc)
            translated.execution_id = translated.execution_id or partial.id
            translated.stdout, translated.stderr = "".join(stdout), "".join(stderr)
            raise translated from exc
        if execution.output_truncated:
            raise AdapterError("command output was truncated", code="output_truncated", execution=execution)
        if execution.termination_reason != "exited" or type(execution.exit_code) is not int:
            raise AdapterError("command did not report an ordinary exit",
                               code=execution.termination_reason or "incomplete", execution=execution)
        result = CommandResult(execution.stderr, execution.stdout, execution.exit_code)
        if result.exit_code != 0:
            raise CommandExitException(result.stderr, result.stdout, result.exit_code, result.error)
        return result


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
        self._call(lambda: self._client.write_file(self._id, target, payload, cancel=token), token)
        parent, _, name = target.rpartition("/")
        found = next((entry for entry in self._list(parent or "/", token) if entry.name == name), None)
        if found is None or found.type != FileType.FILE:
            raise AdapterError("write metadata was not confirmed", code="write_unconfirmed")
        return WriteInfo(found.name, found.type, found.path)

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
        return self._list(target, CancellationToken(timeout=self._request_timeout))

    def _list(self, target: str, token: CancellationToken) -> list[EntryInfo]:
        entries: list[EntryInfo] = []
        cursor, seen = "", set()
        while True:
            page = self._call(lambda: self._client.list_files(
                self._id, target, limit=1000, cursor=cursor, cancel=token), token)
            for entry in page.entries:
                extra = entry.extra
                if not isinstance(extra.get("owner"), str) or not isinstance(extra.get("group"), str):
                    raise AdapterError("guest owner/group metadata is unavailable", code="incompatible_server")
                kind = extra.get("type")
                if kind not in {"file", "dir", "symlink", "other"}:
                    raise AdapterError("guest file type is unavailable", code="incompatible_server")
                entries.append(EntryInfo(
                    name=entry.name, path=target.rstrip("/") + "/" + entry.name,
                    type=None if kind == "other" else FileType(kind), size=entry.size, mode=entry.mode,
                    permissions=stat.filemode(stat.S_IFREG | (entry.mode & 0o7777))[1:],
                    owner=extra["owner"], group=extra["group"], modified_time=_date(entry.mtime),
                    symlink_target=extra.get("symlink_target"),
                ))
            if len(entries) > 10000:
                raise AdapterError("directory exceeds 10000 entries", code="payload_too_large")
            cursor = page.next_cursor
            if not cursor:
                return entries
            if cursor in seen:
                raise AdapterError("directory cursor repeated", code="incompatible_server")
            seen.add(cursor)
