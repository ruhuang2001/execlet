# Migrate the E2B basic Sandbox subset

This checkout provides a synchronous Python facade and a TypeScript facade over
tiny-sandbox. No E2B Cloud account, key or runtime package is required. This is
application-source compatibility for the [declared subset](../../docs/e2b-compatibility.md),
not an E2B HTTP endpoint or a replacement package named `e2b`.

## Prepare the server, image and packages

Build and run `sandboxd` from the same checkout as the SDKs, following the
[server instructions](../../docs/quickstart.md). Do not assume an older release
supports these fields. From the repository root:

```sh
docker build -t tiny-sandbox-e2b:local examples/e2b
export SANDBOX_E2B_IMAGE=$(docker image inspect tiny-sandbox-e2b:local --format '{{.Id}}')
export SANDBOX_DOMAIN=http://localhost:8080
# If sandboxd requires authentication, set SANDBOX_API_KEY securely.
python3 -m pip install ./sandbox-python
npm --prefix sandbox-ts ci
npm --prefix sandbox-ts pack
```

Install the emitted npm tarball in your application. The compatibility profile
targets Python 3.11+ and Node 22+ (the reference harness requires Node 22.19+).
The image has Bash, Python 3, coreutils and a non-root `user` with `/home/user`;
it does not reproduce every package in E2B's cloud template. Pin the **resulting
image ID**, not `:local`. The pinned base alone does not pin apt package updates.

## Python

Change imports, connection settings and the local image selection. Application
command strings, result fields and exception branches remain the same:

```python
from tiny_sandbox.e2b import Sandbox, CommandExitException

with Sandbox.create(envs={"TASK": "migration"}, timeout=120) as sandbox:
    sandbox.files.write("input.txt", "hello 世界\n")
    result = sandbox.commands.run(
        "cat input.txt | tr a-z A-Z",
        on_stdout=lambda chunk: print(chunk, end="", flush=True),
    )
    assert result.exit_code == 0
    assert sandbox.files.read("input.txt", format="bytes") == bytearray("hello 世界\n", "utf-8")
    try:
        sandbox.commands.run("printf 'bad input' >&2; exit 37")
    except CommandExitException as error:
        assert error.exit_code == 37 and error.stderr == "bad input"
```

## TypeScript

```ts
import { Sandbox, CommandExitError } from '@tiny-sandbox/sdk/e2b';

const sandbox = await Sandbox.create({ envs: { TASK: 'migration' }, timeoutMs: 120_000 });
try {
  await sandbox.files.write('input.txt', 'hello 世界\n');
  const result = await sandbox.commands.run('cat input.txt | tr a-z A-Z', {
    onStdout: chunk => { process.stdout.write(chunk); },
  });
  console.log(result.exitCode, await sandbox.files.list('.'));
  try {
    await sandbox.commands.run("printf 'bad input' >&2; exit 37");
  } catch (error) {
    if (!(error instanceof CommandExitError)) throw error;
    console.log(error.exitCode, error.stderr);
  }
} finally {
  await sandbox.kill();
}
```

## Operational differences that remain explicit

- `image` or `SANDBOX_E2B_IMAGE` supplies the local base image. `endpoint` plus
  `api_key` / `apiKey` override `SANDBOX_DOMAIN` / `SANDBOX_API_KEY`. E2B settings
  are not read. Only omitted template or `base` is accepted.
- `connect(id)` preserves workspace/env and extends lifetime without shortening
  it; `set_timeout` / `setTimeout` replaces lifetime and may shorten it.
- Command timeout means **stop waiting, not stop the process**. Timeout, callback
  failure and output truncation can leave the execution slot occupied. Explicit
  sandbox cleanup is still your responsibility. Native client cancellation
  keeps its original remote-cancel behavior.
- `TimeoutException` / `TimeoutError` retains available execution identity,
  idempotency key and partial output. Do not blindly rerun an uncertain command.
- Files are limited to 10 MiB; output to 1 MiB combined; directory enumeration
  is bounded. `AdapterError.code == 'output_truncated'` is not an ordinary exit.
  Backend cleanup failure is an error, including repeated kill calls.
- `stdin=True`, background handles, PTY, Code Interpreter, pause/resume and
  recursive listing are outside this facade. Use native APIs for native features.
- Only matched builds are tested. There is no old-server capability negotiation,
  cloud parity certification, persistence or VM-isolation claim.

Run the [installed-package acceptance gate](../../scripts/e2b-compatibility/README.md)
before using a different image or server revision. SDK/image publication is
separate from local build and acceptance.
