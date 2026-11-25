#!/usr/bin/env python3
"""Sync the Mintlify OpenAPI copy with api/openapi.yaml.

Omit the maintainers' note about the repository prose contract. Check mode
fails if the copy is stale.
"""

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SOURCE = ROOT / "api" / "openapi.yaml"
TARGET = ROOT / "mintlify" / "openapi.yaml"

SOURCE_NOTE = (
    "    docs/api.md is the prose source of this contract; when the two disagree,\n"
    "    fix one of them in the same change.\n"
)


def render() -> str:
    return SOURCE.read_text(encoding="utf-8").replace(SOURCE_NOTE, "", 1)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true",
                        help="verify the copy is current instead of writing it")
    args = parser.parse_args()

    expected = render()
    current = TARGET.read_text(encoding="utf-8") if TARGET.exists() else ""

    if args.check:
        if current != expected:
            print("sync-docs-openapi: mintlify/openapi.yaml is stale; "
                  "run 'make docs-openapi'", file=sys.stderr)
            return 1
        return 0

    if current != expected:
        TARGET.parent.mkdir(parents=True, exist_ok=True)
        TARGET.write_text(expected, encoding="utf-8")
        print(f"sync-docs-openapi: wrote {TARGET.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
