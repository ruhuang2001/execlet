---
title: "Quickstart"
description: "Install execlet, start the daemon, and run Python in a fresh sandbox."
---

This walkthrough takes about five minutes. You need a running Docker daemon
and the Docker CLI — no Git clone, no Go toolchain, and no model API key.

macOS and Linux on amd64 or arm64 are supported, with a reachable Docker Unix
socket. Windows is not supported, and WSL2 is not a tested environment.

<Note>
The installer downloads published binaries, while this documentation describes
the current source. Check `execletctl version` for the build you actually have.
To try unreleased features, see [Build from source](/installation).
</Note>

<Steps>
  <Step title="Install the binaries">
    ```sh
    curl -fsSL https://raw.githubusercontent.com/ruhuang2001/tiny-sandbox/main/scripts/install.sh | sh
    ```

    The installer selects your platform and writes `execletd` and `execletctl`
    into a quickstart directory. It needs no `sudo`.
  </Step>

  <Step title="Start the daemon">
    In terminal A:

    ```sh
    cd "$HOME/tiny-sandbox-quickstart"
    docker info
    export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
    ./execletd
    ```

    Keep this terminal open. The daemon listens on `127.0.0.1:8080`.

    <Warning>
    `execletd` needs a local Docker **Unix socket**. A TCP endpoint such as
    `tcp://127.0.0.1:2375` will not work. If `docker info` fails, fix Docker
    first; you can also pass the socket explicitly with
    `./execletd -docker /path/to/docker.sock`.
    </Warning>
  </Step>

  <Step title="Check that it answers">
    In terminal B:

    ```sh
    cd "$HOME/tiny-sandbox-quickstart"
    export SANDBOX_DOMAIN=http://127.0.0.1:8080
    ./execletctl doctor
    ```

    It ends with `Ready`. `doctor` is the one command that talks to the server
    outside the SDK, which is deliberate: it has to work when the SDK does not.
  </Step>

  <Step title="Run Python">
    ```sh
    ./execletctl run --rm -- code 'print(2 ** 10)'
    ```

    Expected stdout:

    ```text
    1024
    ```

    Command stdout goes to stdout; the execution status line and the command's
    own stderr go to stderr. That split is what makes
    `execletctl run -- cmd > out.txt` capture only your program's output.

    `--rm` asks the daemon to clean up the sandbox this invocation created. Each
    run without `--sandbox` starts from a fresh container.
  </Step>

  <Step title="Watch output stream, then time out">
    This program prints one, two, three about a second apart:

    ```sh
    ./execletctl run --rm -- code '
    import time
    for i in range(1, 4):
        print(i, flush=True)
        time.sleep(1)
    '
    ```

    Output appears as it is produced, not when the command exits. Now bound it:

    ```sh
    ./execletctl run --rm --exec-timeout 1 -- code '
    import time
    time.sleep(30)
    '
    ```

    The command is killed and the status line names the reason. Notice what you
    are *not* doing: inferring it from exit code 137. See
    [Typed failures](/concepts/errors).
  </Step>
</Steps>

## Next

<Columns cols={2}>
  <Card title="Keep a sandbox around" icon="recycle" href="/concepts/lifecycle">
    Pause costs memory retention; kill costs files. Learn which one you want.
  </Card>
  <Card title="Use an SDK" icon="code" href="/sdk/python">
    The CLI covers the ergonomics; the SDKs cover retries, streams, and errors.
  </Card>
</Columns>

## When something is wrong

| Symptom | Check |
| --- | --- |
| `execletd` exits at startup | Is `DOCKER_HOST` a `unix://` path the user can read? |
| A command needs network and hangs | Network defaults to `bridge`; confirm the image can reach a registry |
| The sandbox vanished | A lifetime timeout fired. It defaults to 300 seconds |
| First run is slow | The image is being pulled. Pre-pull it, or pin a local image tag |
