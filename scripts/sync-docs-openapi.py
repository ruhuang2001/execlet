#!/usr/bin/env python3
"""Keep the Mintlify OpenAPI copy byte-comparable with api/openapi.yaml.

The published site needs the specification inside its docs directory, but the
normative file lives in api/. This copies it, rewriting only the description
lines that point at repository paths a docs reader cannot open, and fails in
check mode when the copy has drifted.
"""

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SOURCE = ROOT / "api" / "openapi.yaml"
TARGET = ROOT / "mintlify" / "openapi.yaml"

REWRITES = (
    ("Normative HTTP contract for the tiny-sandbox reference server.",
     "Normative HTTP contract for the execlet reference server."),
    ("docs/api.md is the prose source of this contract; when the two disagree,\n"
     "    fix one of them in the same change.",
     "This page is generated from the specification the server validates its "
     "router against."),
)


def render() -> str:
    text = SOURCE.read_text(encoding="utf-8")
    for old, new in REWRITES:
        if old not in text:
            raise SystemExit(f"sync-docs-openapi: source no longer contains {old!r}")
        text = text.replace(old, new)
    return text


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
