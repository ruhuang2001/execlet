"""Additive wire models: open strings, keyword-only fields, unknown-field retention."""

from __future__ import annotations

import re
from dataclasses import dataclass, field, fields
from typing import Any, TypeVar

_M = TypeVar("_M", bound="_Model")
_MIB = 1 << 20


@dataclass(kw_only=True)
class _Model:
    extra: dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls: type[_M], data: dict[str, Any]) -> _M:
        from ._errors import Error

        if not isinstance(data, dict):
            raise Error("decode", "expected a JSON object")
        definitions = {f.name: f for f in fields(cls) if f.name != "extra"}
        known = definitions.keys()
        for key, value in data.items():
            if key not in known:
                continue
            annotation = definitions[key].type
            if value is None and "None" in annotation:
                continue
            expected = annotation.split(" | ")[0]
            valid = {
                "str": lambda: isinstance(value, str),
                "int": lambda: type(value) is int,
                "bool": lambda: type(value) is bool,
                "list[str]": lambda: isinstance(value, list) and all(isinstance(v, str) for v in value),
            }.get(expected)
            if valid is not None and not valid():
                raise Error("decode", "invalid response field: " + key)
        for key in ("id", "sandbox_id"):
            if key in known and not data.get(key):
                raise Error("decode", "response is missing " + key)
        if "state" in known and not data.get("state"):
            raise Error("decode", "response is missing state")
        if "pid" in known and data.get("pid") is not None and data["pid"] <= 0:
            raise Error("decode", "response pid must be positive")
        try:
            return cls(
                **{key: value for key, value in data.items() if key in known},
                extra={key: value for key, value in data.items() if key not in known},
            )
        except TypeError as exc:
            raise Error("decode", "response is missing required fields", cause=exc) from exc


@dataclass(kw_only=True)
class Volume(_Model):
    id: str
    size_bytes: int
    created_at: str

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Volume:
        from ._errors import Error

        volume = super().from_dict(data)
        if not 0 < volume.size_bytes < 2**63 or not volume.created_at:
            raise Error("decode", "invalid volume metadata")
        return volume


@dataclass(kw_only=True)
class VolumeMount(_Model):
    volume_id: str
    path: str
    read_only: bool | None = None

    def to_dict(self) -> dict[str, Any]:
        from ._errors import Error

        if isinstance(self.path, str) and len(self.path) > 4096:
            raise Error("invalid_request", "mount path exceeds 4096 bytes")
        if not isinstance(self.volume_id, str) or not self.volume_id or (
            not isinstance(self.path, str) or not self.path.startswith("/mnt/") or any(
                part in {".", ".."} or not re.fullmatch(r"[A-Za-z0-9_.-]+", part)
                for part in self.path.split("/")[2:]
            )
        ):
            raise Error("invalid_request", "mounts require a volume_id and a canonical path below /mnt")
        if self.read_only is not None and type(self.read_only) is not bool:
            raise Error("invalid_request", "mount read_only must be a boolean")
        payload: dict[str, Any] = {"volume_id": self.volume_id, "path": self.path}
        if self.read_only is not None:
            payload["read_only"] = self.read_only
        return payload


@dataclass(kw_only=True)
class Sandbox(_Model):
    agent_mode: str | None = None
    protected: bool | None = None
    runtime: str | None = None
    id: str
    state: str
    image: str = ""
    memory_mb: int | None = None
    max_concurrent_executions: int | None = None
    pids_limit: int | None = None
    nano_cpus: int | None = None
    network: str | None = None
    timeout_seconds: int | None = None
    timeout_action: str | None = None
    idle_timeout_seconds: int | None = None
    idle_timeout_action: str | None = None
    created_at: str = ""
    expires_at: str | None = None
    kill_reason: str | None = None
    mounts: list[VolumeMount] | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Sandbox:
        from ._errors import Error

        if isinstance(data, dict) and "mounts" in data:
            mounts = data["mounts"]
            if not isinstance(mounts, list):
                raise Error("decode", "sandbox mounts must be an array")
            data = {**data, "mounts": [VolumeMount.from_dict(mount) for mount in mounts]}
        return super().from_dict(data)


@dataclass(kw_only=True)
class Execution(_Model):
    id: str
    sandbox_id: str
    state: str = ""
    pid: int | None = None
    command: list[str] = field(default_factory=list)
    cwd: str | None = None
    exit_code: int | None = None
    termination_reason: str | None = None
    stdout: str = ""
    stderr: str = ""
    output_truncated: bool = False
    started_at: str = ""
    finished_at: str | None = None


@dataclass(kw_only=True)
class Capabilities(_Model):
    capabilities: list[str]


@dataclass(kw_only=True)
class CommandAttachment(_Model):
    execution: Execution
    last_event_id: int


@dataclass(kw_only=True)
class ExecutionKillResult(_Model):
    execution: Execution
    signaled: bool


@dataclass(kw_only=True)
class Event:
    id: int
    type: str
    data: str = ""
    limit_bytes: int = 0
    execution: Execution | None = None


@dataclass(kw_only=True)
class _Request:
    def to_dict(self) -> dict[str, Any]:
        return {
            f.name: getattr(self, f.name)
            for f in fields(self)
            if getattr(self, f.name) is not None
            and not (f.name == "idempotency_key" and not getattr(self, f.name))
        }


@dataclass(kw_only=True)
class CreateVolumeRequest(_Request):
    size_bytes: int

    def to_dict(self) -> dict[str, Any]:
        from ._errors import Error

        size = self.size_bytes
        if type(size) is not int or not 16 * _MIB <= size <= (1 << 40) or size % _MIB:
            raise Error("invalid_request", "size_bytes must be a whole MiB between 16 MiB and 1 TiB")
        return {"size_bytes": size}


@dataclass(kw_only=True)
class CreateSandboxRequest(_Request):
    runtime: str | None = None
    image: str | None = None
    memory_mb: int | None = None
    max_concurrent_executions: int | None = None
    pids_limit: int | None = None
    nano_cpus: int | None = None
    network: str | None = None
    timeout_seconds: int | None = None
    timeout_action: str | None = None
    idle_timeout_seconds: int | None = None
    idle_timeout_action: str | None = None
    env: dict[str, str] | None = None
    mounts: list[VolumeMount] | None = None

    def to_dict(self) -> dict[str, Any]:
        from ._errors import Error

        payload = super().to_dict()
        mounts = payload.pop("mounts", None)
        if mounts is None:
            return payload
        if not isinstance(mounts, list):
            raise Error("invalid_request", "mounts must be a list of VolumeMount objects")
        if len(mounts) > 8:
            raise Error("invalid_request", "at most eight volume mounts are allowed")
        entries = []
        for mount in mounts:
            if not isinstance(mount, VolumeMount):
                raise Error("invalid_request", "mounts must be a list of VolumeMount objects")
            entry = mount.to_dict()
            path = entry["path"]
            for prior in entries:
                other = prior["path"]
                if (entry["volume_id"] == prior["volume_id"] or path == other or
                        path.startswith(other + "/") or other.startswith(path + "/")):
                    raise Error("invalid_request", "mounts cannot repeat a volume or overlap")
            entries.append(entry)
        if entries:
            payload["mounts"] = entries
        return payload


@dataclass(kw_only=True)
class UpdateSandboxRequest(_Request):
    timeout_seconds: int | None = None
    extend_seconds: int | None = None


@dataclass(kw_only=True)
class ExecuteRequest(_Request):
    output_tail: bool | None = None
    command: list[str]
    idempotency_key: str = ""
    timeout_seconds: int | None = None
    timeout_ms: int | None = None
    env: dict[str, str] | None = None
    cwd: str | None = None
    stdin: str | None = None

    def to_dict(self) -> dict[str, Any]:
        from ._errors import Error

        if self.timeout_ms is not None:
            if type(self.timeout_ms) is not int or not 0 <= self.timeout_ms <= 9_223_372_036_854:
                raise Error("invalid_request", "timeout_ms must be nonnegative integer milliseconds within the server duration range")
            if self.timeout_seconds is not None:
                raise Error("invalid_request", "timeout_ms and timeout_seconds are mutually exclusive")
        return super().to_dict()


@dataclass(kw_only=True)
class FileEntry(_Model):
    name: str
    size: int
    mode: int
    mtime: str
    is_dir: bool
    type: str
    uid: int
    gid: int
    owner: str | None = None
    group: str | None = None
    symlink_target: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> FileEntry:
        from ._errors import Error

        entry = super().from_dict(data)
        for key in ("owner", "group", "symlink_target"):
            if key in data and data[key] is None:
                raise Error("decode", "invalid response field: " + key)
        return entry


@dataclass(kw_only=True)
class FileList(_Model):
    entries: list[FileEntry]
    next_cursor: str = ""

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> FileList:
        from ._errors import Error

        if not isinstance(data, dict) or not isinstance(data.get("entries"), list):
            raise Error("decode", "file list entries must be an array")
        return super().from_dict({**data, "entries": [FileEntry.from_dict(item) for item in data["entries"]]})
