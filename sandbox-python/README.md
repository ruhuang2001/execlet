# tiny-sandbox Python SDK

Synchronous, standard-library-only client for tiny-sandbox. Requires Python
3.10 or newer and a running `sandboxd` with Docker. Use client and server
builds from compatible revisions.

Install from the repository root:

```sh
python3 -m pip install ./sandbox-python
```

```python
from tiny_sandbox import Client, CreateSandboxRequest

with Client() as client:
    with client.sandbox(CreateSandboxRequest(image="alpine:3.20")) as sandbox:
        execution = client.exec(sandbox.id, ["echo", "hello"])
        print(execution.stdout, end="")
```

Set `SANDBOX_DOMAIN` and, when required, `SANDBOX_API_KEY` for your server.
The sandbox context manager requests cleanup on exit. `exec` raises `Error`
for nonzero exits and other unsuccessful outcomes; inspect `error.code` and
`error.execution` for details.

The client supports execution-local environment and working directory, one-shot
stdin, paginated directory listing, and tar transfers. See the
[SDK guide](../docs/sdk-python.md) for methods, cancellation, retries, and
repeatable end-to-end verification.

## Optional E2B facade

`from tiny_sandbox.e2b import Sandbox, CommandExitException` exposes the
E2B 2.64.0 basic Sandbox subset: lifecycle, Bash commands with callbacks and
compatible errors, text/binary files and complete bounded directory listings.
It has no E2B dependency or cloud-key requirement. Set `SANDBOX_E2B_IMAGE` to
the digest of the local compatibility image and use a server from the same
checkout. Command timeouts stop waiting, not remote work; cleanup is explicit
or via `with Sandbox.create()`. See the [migration example](../examples/e2b/README.md)
and [supported subset](../docs/e2b-compatibility.md). Publication is separate.
