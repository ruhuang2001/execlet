"""One real SDK per process; JSON lines are the harness transport, not mocks."""
from __future__ import annotations

import base64
from datetime import datetime
import importlib
import importlib.metadata
import json
import os
from pathlib import Path
import sys
import time


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def own(sandbox):
    sid = sandbox.sandbox_id
    assert isinstance(sid, str) and sid
    with Path(os.environ["CONTRACT_OWNED"]).open("a") as f:
        f.write(json.dumps({"id": sid}) + "\n")
        f.flush()
        os.fsync(f.fileno())
    return sid


try:
    provider = os.environ["CONTRACT_PROVIDER"]
    module = "e2b" if provider == "e2b" else os.environ.get("CONTRACT_MODULE", "tiny_sandbox.e2b")
    sdk = importlib.import_module(module)
    if provider == "e2b":
        version = importlib.metadata.version("e2b")
        if version != "2.64.0":
            raise RuntimeError("reference SDK version mismatch")
    else:
        try:
            version = importlib.metadata.version("tiny-sandbox")
        except importlib.metadata.PackageNotFoundError:
            version = getattr(sdk, "__version__", None)
    Sandbox = sdk.Sandbox
    # Provider configuration is deliberately confined to this bootstrap.
    options = json.loads(os.environ.get("CONTRACT_OPTIONS", "{}"))
    # Template/image selection is create-only, so connect never receives it.
    connect_options = {k: v for k, v in options.items() if k not in {"image", "template"}}
    emit({"ready": True, "version": version, "module": sdk.__name__, "runtime": sys.version})
except ModuleNotFoundError as e:
    # A missing SDK or adapter module is not_run; a broken dependency is a failure.
    expected = {"e2b", "tiny_sandbox", "tiny_sandbox.e2b", os.environ.get("CONTRACT_MODULE", "")}
    emit({"ready": False, "status": "not_run" if e.name in expected else "failed", "reason": str(e)})
    sys.exit(2)
except Exception as e:
    emit({"ready": False, "status": "failed", "reason": type(e).__name__ + ": " + str(e)})
    sys.exit(1)

sandboxes = {}


def result(value):
    assert type(value.exit_code) is int
    assert isinstance(value.stdout, str) and isinstance(value.stderr, str)
    assert value.error is None or isinstance(value.error, str)
    return {"exit": value.exit_code, "stdout": value.stdout, "stderr": value.stderr, "has_error": bool(value.error)}


def entry(value, full=False):
    assert isinstance(value, sdk.EntryInfo if full else sdk.WriteInfo)
    assert isinstance(value.name, str) and isinstance(value.path, str)
    assert value.type is None or isinstance(value.type, sdk.FileType)
    data = {"name": value.name, "path": value.path, "type": value.type.value if value.type else None}
    if full:
        assert type(value.size) is int and value.size >= 0
        assert type(value.mode) is int
        assert all(isinstance(getattr(value, field), str) for field in ("permissions", "owner", "group"))
        assert isinstance(value.modified_time, datetime)
        data.update(size=value.size, mode=value.mode, permissions=value.permissions, owner=value.owner, group=value.group,
                    modified=value.modified_time.timestamp(), symlink=getattr(value, "symlink_target", None))
    return data


def failure(exc):
    families = [("CommandExitException", "exit"), ("SandboxNotFoundException", "sandbox_not_found"),
                ("FileNotFoundException", "file_not_found"), ("AuthenticationException", "auth"),
                ("InvalidArgumentException", "invalid"), ("TimeoutException", "timeout")]
    family = next((label for name, label in families if isinstance(exc, getattr(sdk, name))), "other")
    data = {"family": family, "class": type(exc).__name__, "message": str(exc)}
    if family == "exit":
        data.update(result(exc))
    # Diagnostic extensions are not coerced into normal E2B exit errors.
    for name in ("code", "execution_id", "idempotency_key", "termination_reason", "output_truncated", "stdout", "stderr"):
        v = getattr(exc, name, None)
        if isinstance(v, (str, int, bool)):
            data[name] = v
    return data


def dispatch(q, callbacks):
    op = q["op"]
    kwargs = q.get("options", {}).copy()
    if "duration" in q:
        kwargs["timeout"] = q["duration"]
    if op in {"create", "context"}:
        s = Sandbox.create(**{**options, **kwargs})
        sid = own(s)
        sandboxes[sid] = s
        if op == "context":
            sentinel = RuntimeError("application-sentinel")
            try:
                with s:
                    if q.get("raise_error"):
                        raise sentinel
            except RuntimeError as e:
                assert e is sentinel
                return {"id": sid, "application_error": True}
            except Exception as e:
                if not q.get("cleanup_error"):
                    raise
                assert e is not sentinel and e.__cause__ is sentinel
                assert getattr(e, 'code', '') == 'backend_error' and 'unconfirmed' in str(e), str(e)
                return {"id": sid, "cleanup_error_preserved": True}
            assert not q.get("raise_error"), "context swallowed application error"
            return {"id": sid, "application_error": False}
        return {"id": sid}
    if op == "connect":
        s = Sandbox.connect(q["id"], **{**connect_options, **kwargs})
        assert s.sandbox_id == q["id"]
        sandboxes[q["id"]] = s
        return {"id": s.sandbox_id}
    s = sandboxes[q["id"]]
    if op == "kill":
        value = s.kill(**kwargs)
        assert type(value) is bool
        return value
    if op == "timeout":
        s.set_timeout(**kwargs)
        return None
    if op == "info":
        i = Sandbox.get_info(q["id"])
        assert i.sandbox_id == q["id"]
        return {"id": i.sandbox_id, "state": str(i.state), "template": i.template_id, "end": i.end_at.timestamp()}
    if op == "run":
        sentinel = sdk.TimeoutException("callback-sentinel") if q.get("callback_error") == 'sdk' else RuntimeError("callback-sentinel")
        if "wait" in q:
            kwargs["timeout"] = q["wait"]
        def callback(stream, chunk):
            assert isinstance(chunk, str)
            callbacks[stream].append(chunk)
            callbacks["times"].append(time.monotonic())
            if q.get("callback_error"):
                raise sentinel
        if q.get("callbacks") or q.get("callback_error"):
            kwargs.update(on_stdout=lambda c: callback("stdout", c), on_stderr=lambda c: callback("stderr", c))
        try:
            return result(s.commands.run(q["command"], **kwargs))
        except Exception as e:
            if e is sentinel:
                return {"callback_error_preserved": True}
            raise
    if op == "write":
        data = q.get("text") if "text" in q else base64.b64decode(q["bytes"], validate=True)
        if q.get("bytearray"):
            data = bytearray(data)
        return entry(s.files.write(q["path"], data, **kwargs))
    if op == "read":
        value = s.files.read(q["path"], **kwargs)
        if kwargs.get("format") == "bytes":
            assert isinstance(value, bytearray), "byte reads must be bytearray"
            return {"bytes": base64.b64encode(value).decode()}
        assert isinstance(value, str)
        return value
    if op == "list":
        values = s.files.list(q["path"], **kwargs)
        assert isinstance(values, list)
        return [entry(v, True) for v in values]
    if op == "unsupported":
        return getattr(s, q["method"])()
    raise ValueError("unknown harness operation")


for line in sys.stdin:
    callbacks = {"stdout": [], "stderr": [], "times": []}
    started = time.monotonic()
    try:
        value = dispatch(json.loads(line), callbacks)
        response = {"ok": True, "value": value}
    except Exception as e:
        response = {"ok": False, "error": failure(e)}
    response.update(callbacks=callbacks, elapsed=time.monotonic() - started)
    emit(response)
