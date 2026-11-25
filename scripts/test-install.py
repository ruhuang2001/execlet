import hashlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest


REPO = "https://github.com/ruhuang2001/execlet"
INSTALLER = Path(__file__).resolve().with_name("install.sh")
CURL = """#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift ;;
    https://*|http://*) url=$1 ;;
  esac
  shift
done
printf '%s\\n' "$url" >> "$REQUESTS"
case "$url" in
  "$REPO/releases/latest") printf '%s' "$LATEST" ;;
  "$REPO/releases/download/$TAG/"*)
    file="$ASSETS/${url##*/}"
    [ -f "$file" ] || { printf 'Fixture asset missing: %s\\n' "${file##*/}" >&2; exit 22; }
    cp "$file" "$output"
    ;;
  *) exit 22 ;;
esac
"""


class InstallerTest(unittest.TestCase):
    def run_install(self, *, tag="v0.1.0", system="Linux", machine="x86_64", projects=(),
                    redirect=None, checksum=None, fault=None, selected=None, error=None,
                    default_destination=False):
        platform = system.lower()
        arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}[machine]
        with tempfile.TemporaryDirectory(prefix="execlet-test-") as temp:
            root = Path(temp)
            tools, assets, staging = (root / name for name in ("tools", "assets", "tmp"))
            for directory in tools, assets, staging:
                directory.mkdir()
            home = root / "test home"
            destination = home / "execlet-quickstart" if default_destination else root / "install destination"
            requests = root / "requests"
            extracted = root / "extracted"
            for name, source in {
                "curl": CURL,
                "uname": '#!/bin/sh\ncase "$1" in -s) echo "$SYSTEM" ;; -m) echo "$MACHINE" ;; esac\n',
                "tar": '#!/bin/sh\necho tar >> "$EXTRACTED"\nexec "$REAL_TAR" "$@"\n',
            }.items():
                path = tools / name
                path.write_text(source)
                path.chmod(0o755)

            names, contents, checksums = {}, {}, []
            for project in projects:
                name = f"{project}_{tag[1:]}_{platform}_{arch}.tar.gz"
                names[project] = name
                prefix = "execlet" if project == "execlet" else "sandbox"
                with tarfile.open(assets / name, "w:gz") as archive:
                    for member in (f"{prefix}d", f"{prefix}ctl", "README.md"):
                        data = f"{project} {tag} {member}\n".encode()
                        info = tarfile.TarInfo(member)
                        info.size = len(data)
                        archive.addfile(info, io.BytesIO(data))
                        contents[project, member] = data
                digest = hashlib.sha256((assets / name).read_bytes()).hexdigest()
                entry = f"{digest}  {name}\n"
                if project == selected:
                    if fault == "duplicate":
                        entry *= 2
                    elif fault == "invalid":
                        entry = f"bad  {name}\n"
                    elif fault == "missing":
                        (assets / name).unlink()
                    elif fault == "corrupt":
                        with (assets / name).open("ab") as file:
                            file.write(b"corruption")
                checksums.append(entry)
            if checksum != "missing":
                (assets / "checksums.txt").write_text("".join(checksums))

            env = dict(os.environ, PATH=f"{tools}:{os.environ['PATH']}", TMPDIR=str(staging), HOME=str(home),
                       SYSTEM=system, MACHINE=machine, REQUESTS=str(requests), EXTRACTED=str(extracted),
                       REAL_TAR=shutil.which("tar"), REPO=REPO, TAG=tag, ASSETS=str(assets),
                       LATEST=redirect or f"{REPO}/releases/tag/{tag}")
            args = [] if default_destination else [destination.name]
            result = subprocess.run(["sh", str(INSTALLER), *args], env=env, cwd=root,
                                    capture_output=True, text=True, timeout=10)
            expected_requests = [f"{REPO}/releases/latest"]
            if redirect is None:
                base = f"{REPO}/releases/download/{tag}"
                expected_requests.append(f"{base}/checksums.txt")
                if selected and fault not in {"duplicate", "invalid"}:
                    expected_requests.append(f"{base}/{names[selected]}")
            output = result.stdout + result.stderr
            self.assertEqual(requests.read_text().splitlines(), expected_requests, output)
            self.assertFalse(list(staging.iterdir()), output)
            if error is not None:
                self.assertNotEqual(result.returncode, 0, output)
                self.assertIn(error, output)
                self.assertFalse(extracted.exists(), output)
                self.assertFalse(destination.exists(), output)
                return

            self.assertEqual(result.returncode, 0, output)
            self.assertEqual(extracted.read_text().splitlines(), ["tar"])
            self.assertEqual({p.name for p in destination.iterdir()}, {"execletd", "execletctl"})
            prefix = "execlet" if selected == "execlet" else "sandbox"
            for suffix in ("d", "ctl"):
                executable = destination / f"execlet{suffix}"
                self.assertEqual(executable.read_bytes(), contents[selected, f"{prefix}{suffix}"])
                self.assertEqual(executable.stat().st_mode & 0o777, 0o755)
            self.assertIn(f"{REPO}/blob/{tag}/docs/quickstart.md", result.stdout)
            self.assertEqual("still has the old behavior" in result.stderr, selected == "tiny-sandbox")
            if selected == "tiny-sandbox":
                self.assertIn(f"In both terminals use this directory: {destination.resolve()}\n", result.stderr)
                self.assertIn("./execletd instead of ./sandboxd", result.stderr)
                self.assertIn("./execletctl instead of sandboxctl", result.stderr)

    def test_new_release_on_each_platform(self):
        for system, machine in (("Darwin", "arm64"), ("Darwin", "x86_64"),
                                ("Linux", "aarch64"), ("Linux", "x86_64")):
            with self.subTest(system=system, machine=machine):
                self.run_install(system=system, machine=machine, projects=("execlet",), selected="execlet")

    def test_legacy_fallback_is_same_tag_and_explicit(self):
        for tag in ("v0.0.1", "v0.0.2", "v0.0.3"):
            with self.subTest(tag=tag):
                self.run_install(tag=tag, projects=("tiny-sandbox",), selected="tiny-sandbox")
        self.run_install(projects=("tiny-sandbox",), error="Checksum not found for execlet_")

    def test_historical_quickstart_with_default_destination(self):
        self.run_install(tag="v0.0.3", projects=("tiny-sandbox",), selected="tiny-sandbox", default_destination=True)

    def test_new_name_wins_and_never_downgrades_on_failure(self):
        for fault, error in ((None, None), ("duplicate", "Invalid checksum entry"),
                             ("invalid", "Invalid checksum entry"), ("missing", "Fixture asset missing: execlet_"),
                             ("corrupt", "Checksum mismatch")):
            with self.subTest(fault=fault):
                self.run_install(tag="v0.0.3", projects=("tiny-sandbox", "execlet"),
                                 fault=fault, selected="execlet", error=error)

    def test_missing_checksum_manifest_or_asset_entry(self):
        self.run_install(tag="v0.0.3", projects=("tiny-sandbox",), checksum="missing",
                         error="Fixture asset missing: checksums.txt")
        self.run_install(error="Checksum not found")

    def test_reject_noncanonical_release_url(self):
        for redirect in ("https://github.com/ruhuang2001/tiny-sandbox/releases/tag/v0.0.3",
                         f"{REPO}/releases/latest", f"{REPO}/releases/tag/v01.2.3",
                         f"{REPO}/releases/tag/v0.1.0/other", f"{REPO}/releases/tag/v0.1.0?x=1",
                         f"{REPO}/releases/tag/v0.1.0-rc.1"):
            with self.subTest(redirect=redirect):
                self.run_install(redirect=redirect, error="release URL")


if __name__ == "__main__":
    unittest.main()
