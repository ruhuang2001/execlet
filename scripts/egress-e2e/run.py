"""Run a sandboxd and an authenticated HTTPS fixture on an internal Docker network."""

import hashlib
import json
import os
import secrets
import subprocess
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / "artifacts/egress"
IMAGE = "python:3.12-alpine"


def command(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    suffix = secrets.token_hex(5)
    network, upstream, controller = [
        part + suffix for part in ("egress-net-", "upstream-", "controller-")
    ]
    owner = "egress-" + suffix
    token = secrets.token_urlsafe(32)
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        arch = command("docker", "info", "--format", "{{.Architecture}}")
        arch = {"aarch64": "arm64", "x86_64": "amd64"}.get(arch, arch)
        coverage = os.environ.get("GOCOVERDIR")
        coverage_flags = (
            [
                "-v",
                f"{Path(coverage).resolve()}:/coverage",
                "-e",
                "GOCOVERDIR=/coverage",
            ]
            if coverage
            else []
        )
        subprocess.run(
            ["go", "build", "-o", work / "sandboxd", "./cmd/sandboxd"],
            cwd=ROOT,
            check=True,
            env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch},
        )
        command(
            "openssl",
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-days",
            "1",
            "-subj",
            "/CN=packages.fixture.test",
            "-addext",
            "subjectAltName=DNS:packages.fixture.test",
            "-keyout",
            work / "key.pem",
            "-out",
            work / "ca.pem",
            stderr=subprocess.DEVNULL,
        )
        policy = dict(
            max_sandboxes=2,
            total_memory_mb=1024,
            total_nano_cpus=2_000_000_000,
            total_pids=2048,
            allowed_images=[IMAGE, "alpine:3.21"],
            allowed_networks=["filtered"],
            egress=dict(
                domains=["packages.fixture.test", "fakeip.fixture.test"],
                tunnel_idle_seconds=4,
                credential=dict(
                    url="https://packages.fixture.test/account",
                    token_env="TEST_TOKEN",
                    images=[IMAGE],
                ),
            ),
        )
        wheel_source = command(
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            IMAGE,
            "python3",
            "-c",
            "import pathlib,ensurepip;print(next((pathlib.Path(ensurepip.__file__).parent/'_bundled').glob('pip-*.whl')))",
        )
        wheel = work / Path(wheel_source).name
        with wheel.open("wb") as package:
            subprocess.run(
                [
                    "docker",
                    "run",
                    "--rm",
                    "--network",
                    "none",
                    IMAGE,
                    "cat",
                    wheel_source,
                ],
                check=True,
                stdout=package,
            )
        digest = hashlib.sha256(wheel.read_bytes()).hexdigest()
        (work / "policy.json").write_text(json.dumps(policy))
        subnet = "11.250." + str(1 + secrets.randbelow(250))
        command(
            "docker",
            "network",
            "create",
            "--internal",
            "--subnet",
            subnet + ".0/28",
            network,
        )
        try:
            fixture_env = {**os.environ, "TEST_TOKEN": token}
            command(
                "docker",
                "run",
                "-d",
                "--name",
                upstream,
                "--network",
                network,
                "--ip",
                subnet + ".3",
                "-v",
                str(work) + ":/fixture:ro",
                "-v",
                str(Path(__file__).with_name("upstream.py")) + ":/upstream.py:ro",
                "-e",
                "TEST_TOKEN",
                IMAGE,
                "python3",
                "/upstream.py",
                env=fixture_env,
            )
            command(
                "docker",
                "run",
                "-d",
                "--name",
                controller,
                *coverage_flags,
                "--network",
                network,
                "--add-host",
                "packages.fixture.test:" + subnet + ".3",
                "--add-host",
                "fakeip.fixture.test:198.18.0.192",
                "-e",
                "SSL_CERT_FILE=/fixture/ca.pem",
                "-e",
                "TEST_TOKEN",
                "-v",
                "/var/run/docker.sock:/var/run/docker.sock",
                "-v",
                str(work) + ":/fixture:ro",
                IMAGE,
                "/fixture/sandboxd",
                "-owner",
                owner,
                "-policy",
                "/fixture/policy.json",
                "-state-dir",
                "/tmp/sandboxd-state",
                env=fixture_env,
            )

            def api(method, path, body=None):
                code = (
                    "import sys,json,urllib.request as u; "
                    "b=sys.argv[3].encode(); r=u.Request('http://127.0.0.1:8080'+sys.argv[2],"
                    "data=b or None,method=sys.argv[1],headers={'Content-Type':'application/json'});"
                    "print(u.urlopen(r,timeout=25).read().decode())"
                )
                out = command(
                    "docker",
                    "exec",
                    controller,
                    "python3",
                    "-c",
                    code,
                    method,
                    path,
                    json.dumps(body) if body is not None else "",
                    timeout=35,
                    stderr=subprocess.PIPE,
                )
                return json.loads(out)

            for _ in range(80):
                try:
                    api("GET", "/readyz")
                    break
                except subprocess.CalledProcessError:
                    time.sleep(0.1)
            sb = api("POST", "/v1/sandboxes", {"image": IMAGE, "network": "filtered"})
            ident = sb["id"]
            assert sb["agent_mode"] == "guest"
            container = "tiny-sandbox-" + ident

            def execute(script):
                ex = api(
                    "POST",
                    "/v1/sandboxes/" + ident + "/executions",
                    {"command": ["python3", "-c", script]},
                )
                for _ in range(100):
                    ex = api(
                        "GET", "/v1/sandboxes/" + ident + "/executions/" + ex["id"]
                    )
                    if ex["state"] == "completed":
                        assert ex["exit_code"] == 0, ex
                        return json.loads(ex["stdout"])
                    time.sleep(0.1)
                raise TimeoutError("execution")

            probe = """import json,urllib.request as u,ssl,socket,os
ctx=ssl.create_default_context(cadata=CERT)
def fetch(url,method="GET"):
    try:
        return json.load(u.urlopen(u.Request(url,method=method),context=ctx,timeout=5))
    except Exception as e:
        return str(e)
print(json.dumps(dict(index=fetch('https://packages.fixture.test/index'),
 account=fetch('http://127.0.0.1:8888/credential'),denied=fetch('https://unapproved.fixture.test/index'),
 fake_ip=fetch('https://fakeip.fixture.test/'),
 direct_account=fetch('https://packages.fixture.test/account'),
 other_method=fetch('http://127.0.0.1:8888/credential','POST'),
 other_path=fetch('http://127.0.0.1:8888/elsewhere'), token_present=bool(os.getenv('TEST_TOKEN')))))
""".replace("cadata=CERT", "cadata=" + repr((work / "ca.pem").read_text()))
            result = execute(probe)
            assert result["index"] == {"path": "/index", "authenticated": False}, result
            assert result["account"] == {"path": "/account", "authenticated": True}, (
                result
            )
            assert all(
                "403" in result[key]
                for key in ("denied", "other_path", "direct_account", "other_method")
            ), result
            assert not result["token_present"] and token not in json.dumps(result)
            assert (
                command(
                    "docker",
                    "inspect",
                    "--format",
                    "{{.HostConfig.NetworkMode}}",
                    container,
                )
                == "none"
            )
            concurrency = """import concurrent.futures, json, ssl, urllib.request
ctx=ssl.create_default_context(cadata=CERTIFICATE)
def download(i):
    url='https://packages.fixture.test/stream'
    with urllib.request.urlopen(url,context=ctx,timeout=12) as res:
        return res.read().decode()
with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
    results=list(pool.map(download,range(6)))
assert results==['123456']*6,results
print(json.dumps({'simultaneous_downloads':len(results),'bytes_each':len(results[0])}))
""".replace("CERTIFICATE", repr((work / "ca.pem").read_text()))
            result["streaming"] = execute(concurrency)
            assert "403" in result["fake_ip"]
            install = """import json, pathlib, subprocess, sys, tempfile
with tempfile.TemporaryDirectory() as tmp:
    dest=pathlib.Path(tmp)
    ca=dest/'fixture.pem'
    ca.write_text(CERTIFICATE)
    subprocess.run([sys.executable,'-m','pip','--disable-pip-version-check','--no-cache-dir','--cert',str(ca),
        'install','--no-index','--no-deps','--target',str(dest/'site'),WHEEL_URL],stdout=sys.stderr,check=True)
    from importlib.metadata import distributions
    dist,=(d for d in distributions(path=[str(dest/'site')]) if d.metadata['Name']=='pip')
    print(json.dumps({'name':dist.metadata['Name'],'version':dist.version}))
""".replace("CERTIFICATE", repr((work / "ca.pem").read_text())).replace(
                "WHEEL_URL",
                repr(
                    "https://packages.fixture.test/" + wheel.name + "#sha256=" + digest
                ),
            )
            result["installed_dependency"] = execute(install)
            assert result["installed_dependency"]["version"] == wheel.name.split("-")[1]
            result["wheel_sha256"] = digest
            api("DELETE", "/v1/sandboxes/" + ident)
            unprivileged = api(
                "POST", "/v1/sandboxes", {"image": "alpine:3.21", "network": "filtered"}
            )
            status = command(
                "docker",
                "exec",
                "tiny-sandbox-" + unprivileged["id"],
                "sh",
                "-c",
                "wget --timeout=5 -S -O /dev/null http://127.0.0.1:8888/credential 2>&1 || true",
            )
            assert "403" in status, status
            result["unauthorized_image_status"] = 403
            api("DELETE", "/v1/sandboxes/" + unprivileged["id"])
            second = api(
                "POST", "/v1/sandboxes", {"image": IMAGE, "network": "filtered"}
            )
            second_container = "tiny-sandbox-" + second["id"]
            command("docker", "stop", "--time", "20", controller)
            output = command(
                "docker",
                "exec",
                second_container,
                "python3",
                "-c",
                "import socket,sys; s=socket.socket();s.settimeout(2);"
                "print(s.connect_ex((sys.argv[1],443)))",
                subnet + ".3",
            )
            assert int(output) != 0
            logs = command("docker", "logs", controller, stderr=subprocess.STDOUT)
            assert token not in logs
            refusals = [
                json.loads(line) for line in logs.splitlines() if line.startswith("{")
            ]
            result["denials"] = [
                {"host": row["host"], "reason": row["reason"]}
                for row in refusals
                if row.get("msg") == "egress request denied"
            ]
            assert {
                "host": "fakeip.fixture.test",
                "reason": "reserved address 198.18.0.192",
            } in result["denials"], result["denials"]
            assert {
                "host": "unapproved.fixture.test",
                "reason": "domain not allowed",
            } in result["denials"]
            result.update(
                network="none",
                direct_connection_errno_after_proxy_stop=int(output),
                token_in_log=False,
            )
            (OUT / "acceptance.json").write_text(json.dumps(result, indent=2) + "\n")
        finally:
            subprocess.run(
                ["docker", "rm", "-f", controller, upstream],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            for cid in command(
                "docker", "ps", "-aq", "--filter", "label=tiny-sandbox.owner=" + owner
            ).splitlines():
                command("docker", "rm", "-f", cid)
            command("docker", "network", "rm", network)


if __name__ == "__main__":
    main()
