"""Verify the wheel contains exactly this checkout's SDK and distribution metadata."""

import argparse
from email.parser import BytesParser
from pathlib import Path
from zipfile import ZipFile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("wheel", type=Path)
    wheel = parser.parse_args().wheel
    source = Path(__file__).resolve().parents[1] / "sandbox-python" / "src"
    package = source / "execlet"
    expected = {p.relative_to(source).as_posix() for p in package.glob("*.py")} | {"execlet/py.typed"}
    dist, version, tag = wheel.name.split("-", 2)
    if dist != "execlet" or tag != "py3-none-any.whl":
        raise SystemExit(f"unexpected SDK wheel: {wheel.name}")
    info = f"execlet-{version}.dist-info/"
    with ZipFile(wheel) as archive:
        names = archive.namelist()
        payload = {name for name in names if not name.startswith(info)}
        if len(names) != len(set(names)):
            raise SystemExit("duplicate wheel members")
        if payload != expected:
            raise SystemExit(f"unexpected wheel contents: extra={sorted(payload - expected)}, missing={sorted(expected - payload)}")
        for name in expected:
            if archive.read(name) != (source / name).read_bytes():
                raise SystemExit(f"stale wheel member: {name}")
        for member in ("METADATA", "RECORD", "WHEEL", "licenses/LICENSE"):
            if info + member not in names:
                raise SystemExit(f"missing wheel metadata: {member}")
        metadata = BytesParser().parsebytes(archive.read(info + "METADATA"), headersonly=True)
        if metadata["Name"] != dist or metadata["Version"] != version:
            raise SystemExit("wheel filename and distribution metadata disagree")
    print(f"Verified {wheel.name}: {len(expected)} SDK files match source; no other packages")


if __name__ == "__main__":
    main()
