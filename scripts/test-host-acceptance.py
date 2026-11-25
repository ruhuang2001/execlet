"""Failure controls for the host acceptance CLIs. Requires a snapshot and local Docker."""
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
DIST = Path(os.environ.get("DIST", ROOT / "dist")).resolve()
CLI = Path(os.environ.get("CLI", ROOT / "bin/execletctl")).resolve()
SERVER = Path(os.environ.get("SERVER", ROOT / "bin/execletd")).resolve()
IMAGE = os.environ.get("MCP_IMAGE", "execlet-mcp:local")


class AcceptanceTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="execlet-controls-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.out = self.root / "result.json"

    def invoke(self, script, *args, error=None, timeout=90, env=None):
        result = subprocess.run([sys.executable, "-B", ROOT / "scripts" / script, *args, "--out", self.out],
                                cwd=self.root, env=env, capture_output=True, text=True, timeout=timeout)
        data = json.loads(self.out.read_text())
        self.assertEqual(result.returncode == 0, error is None, result.stderr)
        self.assertIs(data["passed"], error is None, data)
        if error:
            self.assertIn(error, data["error"], data)
        if "owner" in data:
            ids = subprocess.check_output(["docker", "ps", "-aq", "--filter", f"label=execlet.owner={data['owner']}"], timeout=15)
            self.assertFalse(ids.strip(), ids)
        return data

    def native(self):
        arch = {"aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine(), platform.machine())
        archive, = DIST.glob(f"*_{platform.system().lower()}_{arch}.tar.gz")
        return archive

    def test_archive_manifest_and_platform(self):
        original = self.native()
        archive = self.root / original.name
        archive.symlink_to(original)
        args = ("release-preflight.py", "--archive", archive, "--docs", ROOT / "docs/quickstart.md")
        self.out.write_text('{"passed":true}')
        self.invoke(*args, error="checksums.txt")
        manifest = self.root / "checksums.txt"
        manifest.write_text(f"{'0' * 64}  {archive.name}\n")
        self.invoke(*args, error="incorrect checksum")
        archive.unlink()
        other = next(path for path in DIST.glob("*.tar.gz") if path != original)
        archive.symlink_to(other)
        manifest.write_text(f"{hashlib.sha256(other.read_bytes()).hexdigest()}  {archive.name}\n")
        self.invoke(*args, error="is not a")

    def test_unlisted_archive_invalidates_full_distribution(self):
        for path in (DIST / "metadata.json", DIST / "checksums.txt", *DIST.glob("*.tar.gz")):
            (self.root / path.name).symlink_to(path)
        (self.root / "obsolete.zip").touch()
        self.invoke("release-preflight.py", "--dist", self.root, "--docs", ROOT / "docs/quickstart.md", error="all four")

    def test_documented_output_order_and_actual_exit_status(self):
        guide = self.root / "quickstart.md"
        for program, expectation, trailer in (("print(1); print(2)", "Expected stdout: `2` and `1`.", ""),
                                              ("raise SystemExit(7)", "The command exits with status `0`.", '\necho "Exit code: $?"')):
            with self.subTest(program=program):
                guide.write_text(f"```sh\n./execletctl run --rm -- code '{program}'{trailer}\n```\n\n{expectation}\n")
                self.invoke("release-preflight.py", "--archive", self.native(), "--docs", guide, error="got exit")

    def test_linked_worktree_repair_and_failed_rerun(self):
        repo, work = self.root / "repo", self.root / "linked worktree"
        repo.mkdir()

        def git(*args):
            env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
            env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
            subprocess.run(["git", "-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", *args],
                           env=env, check=True, timeout=15)

        git("init", "--template=", "-q")
        (repo / ".gitignore").write_text("*.txt\n")
        (repo / "calculator.py").write_text("def double(n): return n * 2\n")
        (repo / "answer.txt").write_text("14")
        git("add", "-f", ".")
        git("commit", "-qm", "fixture")
        git("worktree", "add", "-q", "--detach", work)
        source = work / "calculator.py"
        broken = b"def double(n): return n * 3\r\n"
        source.write_bytes(broken)
        (work / "ignored.txt").touch()
        (work / ".env").touch()
        (work / "link").symlink_to(work / "answer.txt")
        (work / "check.py").write_text('from pathlib import Path\nfrom calculator import double\n'
                                     'if any(Path(p).exists() for p in ("ignored.txt", ".env", "link")): raise SystemExit("wrong files")\n'
                                     'if not Path("calculator.py").read_bytes().endswith(b"\\r\\n"): raise SystemExit("lost CRLF")\n'
                                     'if double(7) != int(Path("answer.txt").read_text()): raise SystemExit("wrong double")\n')
        args = ("pilot.py", "--cli", CLI, "--server", SERVER, "--image", IMAGE, "--workspace", work,
                "--test", "python3 -B check.py", "--expect-failure", "wrong double", "--fix-before", "n * 3")
        data = self.invoke(*args, "--fix-file", "calculator.py", "--fix-after", "n * 2", "--runs", "2")
        self.assertEqual([(r["initial_uploaded"], r["fixed_uploaded"], r["unchanged_uploaded"]) for r in data["runs"]],
                         [(4, 1, 0), (4, 1, 0)])
        self.invoke(*args, "--fix-file", "calculator.py", "--fix-after", "n * 4", error="still fails")
        self.invoke(*args, "--fix-file", source, "--fix-after", "n * 2", error="must be relative")
        before = set(work.iterdir())
        self.invoke(*args, "--fix-file", "calculator.py", "--fix-after", "n * 2", "--keep",
                    env={**os.environ, "TMPDIR": str(work)}, error="TMPDIR")
        self.assertEqual(set(work.iterdir()), before)
        self.assertEqual(source.read_bytes(), broken)
        self.assertTrue((work / ".git").is_file())

    def test_stalled_mcp_handshake_has_a_deadline(self):
        stalled = self.root / "stalled-mcp"
        stalled.write_text('#!/bin/sh\nprintf "{"\nexec sleep 5\n')
        stalled.chmod(0o700)
        self.invoke("pilot.py", "--cli", stalled, "--server", SERVER, "--image", IMAGE, "--timeout", "0.3",
                    error="incomplete response at deadline")

    def test_invalid_soak_cannot_reuse_success(self):
        self.out.write_text('{"passed":true}')
        self.invoke("soak.py", "--image", IMAGE, "--duration", "nan", error="finite", timeout=5)


if __name__ == "__main__":
    unittest.main()
