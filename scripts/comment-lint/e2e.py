import json
import subprocess
import sys
import tempfile
from pathlib import Path

LINT = Path(__file__).with_name("check.py").resolve()
evidence = Path(sys.argv[1]).resolve()
results = {}


def run(*args, cwd, check=True):
    return subprocess.run(args, cwd=cwd, text=True, capture_output=True, check=check)


def require(condition, details):
    if not condition:
        raise AssertionError(details)


with tempfile.TemporaryDirectory() as tmp:
    repo = Path(tmp)
    run("git", "init", "-q", cwd=repo)
    old = repo / "old.go"
    old.write_text("package example\n// inherited\nvar N = 1\n")
    run("git", "add", ".", cwd=repo)
    run("git", "-c", "user.name=E2E", "-c", "user.email=e2e@example.invalid",
        "commit", "-qm", "fixture", cwd=repo)

    def check(name, expected, paths=()):
        result = run(sys.executable, str(LINT), "--base", "HEAD", cwd=repo, check=False)
        results[name] = {"exit": result.returncode, "diagnostics": result.stdout.splitlines()}
        require(result.returncode == expected, (name, result))
        for path in paths:
            require(any(line.startswith(path + ":") for line in result.stdout.splitlines()), (name, path))

    missing_base = run(sys.executable, str(LINT), cwd=repo, check=False)
    require(missing_base.returncode == 2 and "--base" in missing_base.stderr, missing_base)
    results["missing comparison revision"] = {"exit": missing_base.returncode, "diagnostic": missing_base.stderr.strip()}
    check("unchanged legacy comment", 0)
    old.write_text(old.read_text().replace("// inherited\n", ""))
    check("deletion", 0)
    old.write_text(old.read_text().replace("var N", "// replacement\nvar N"))
    check("one deletion cannot pay for a different comment", 1, ["old.go"])
    old.write_text("package example\n// inherited\n// inherited\nvar N = 1\n")
    check("duplicates are counted", 1, ["old.go"])
    old.unlink()
    (repo / "other.go").write_text("package example\n// inherited\n")
    check("moving old debt to a new file", 1, ["other.go"])
    (repo / "other.go").unlink()

    clean = {
        "go.mod": "module example.test/demo\nrequire example.test/lib v1.0.0 // indirect\n",
        "guest.go": '//go:build live\n\npackage guest\nconst source = `// data`\n',
        "guest.py": '"""API documentation."""\nvalue = "a#b"\n',
        "client.ts": 'const url = `https://example.test/#fragment`;\n',
        "script.sh": "#!/bin/sh\ncat <<'EOF'\n# literal\nEOF\nprintf '%s' \"$#\"\n",
        "README.md": "# A heading\n\n```sh\necho '# example'\n```\n",
        "Makefile": "target:\n\t@echo '# text'\n",
        ".github/workflows/test.yml": "jobs:\n  sample:\n    steps:\n      - run: |\n          echo '# data'\n",
    }
    for path, content in clean.items():
        target = repo / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
    check("literals, heredoc payload, docstring, heading and directives", 0)

    dirty = {
        "guest.go": "var N = 1 /* inline */\n",
        "guest.py": "N = 1  # inline\n",
        "client.ts": "/* multiple\nlines */\n",
        "script.sh": "echo done # trailing\n",
        "README.md": "~~~ts\nconst n = 1; // example\n~~~\n",
        "Makefile": "\t@echo done # recipe\n",
        ".github/workflows/test.yml": "          echo done # step\n",
        ".gitignore": "# ignore list\n",
        "Dockerfile": "FROM scratch\n# rationale\n",
        "go.mod": "module example.com/demo\n// note\n",
        "project.toml": "name = 'example' # setting\n",
    }
    for path, extra in dirty.items():
        with (repo / path).open("a") as f:
            f.write(extra)
    check("new comments in all supported code surfaces", 1, dirty)

    run("git", "clean", "-fdq", cwd=repo)
    run("git", "restore", ".", cwd=repo)
    (repo / "typed.ts").write_text("// @ts-expect-error legacy reason\nconst n: number = '';\n")
    (repo / "late.py").write_text("import sys  # noqa: E402\n")
    run("git", "add", ".", cwd=repo)
    run("git", "-c", "user.name=E2E", "-c", "user.email=e2e@example.invalid",
        "commit", "-qm", "existing tool suppressions", cwd=repo)
    check("grandfathered tool suppressions", 0)
    (repo / "typed.ts").write_text("// @ts-expect-error new rationale\nconst n: number = '';\n")
    (repo / "late.py").write_text("import sys  # noqa: E402 new rationale\n")
    check("changed suppression prose", 1, ["typed.ts", "late.py"])
    run("git", "restore", ".", cwd=repo)

    samples = {
        "docs/sdk.md": (
            "```\n// unlabelled Go\nCreateSandbox(ctx, Config) (*Sandbox, error)\n```\n"
            "\n    # indented command\n    go run ./demo\n",
            ["// unlabelled Go", "# indented command"]),
        "embedded.sh": (
            "python - <<'PY'\n# direct stdin\nprint('# data')\nPY\n"
            '"${PYTHON:-python3}" - "$OUT" <<\'PY\'\n# configured Python\nPY\n'
            "cat <<'PY' | python3 -\n# piped program\nPY\n"
            '"$BIN/execletctl" run --image demo -- python3 -c \'\n# guest code\n\'\n'
            "python3 -c \"print('test') # quoted code\"\n"
            "python3 -c 'print(\"# data\")'\n"
            "python3 other.py -c '# script argument'\n"
            "cat <<'PY'\n# plain data\nPY\n"
            "python3 3<<'PY'\n# data on another fd\nPY\n"
            "ruby - <<'PY'\n# unrelated\nPY\n",
            ["# direct stdin", "# configured Python", "# piped program", "# guest code", "# quoted code"]),
        "python-options.sh": (
            "python3 -X dev -W ignore -c 'print(1) # separate options'\n"
            "python3 -Xdev -Wignore -c 'print(2) # attached options'\n"
            "python3 -W ignore -m trace -c '# module argument'\n"
            "python3 -X dev script.py -c '# script argument'\n"
            "execletctl run -- code 'print(\"# quoted\") # shorthand'\n"
            '"$BIN/execletctl" run --rm -- code "print(2) # wrapped shorthand"\n'
            "execletctl run -- code 'print(\"# just data\")'\n"
            "execletctl run -- code 'print(3) # invalid arity' extra\n"
            "execletctl run -- printf '%s' code '# printed argument'\n"
            "code '# different program'\n",
            ["# separate options", "# attached options", "# shorthand", "# wrapped shorthand"]),
        "complex.py": (
            "name = 'hello'\ntext = f\"\"\"{name}: # fixture\n# data\n\"\"\"\n"
            "box = f\"\"\"{(1  # expression\n)}\"\"\"\n# eof",
            ["# expression", "# eof"]),
        ".github/workflows/quoted.yml": (
            "defaults:\n  run:\n    shell: bash\njobs:\n  sample:\n    steps:\n"
            "      - run: 'echo \\#tag'\n"
            "      - run: 'echo ''# inner'' # quoted'\n"
            '      - run: "echo \\"# inner\\" # escaped"\n'
            "      - run: | # block header\n          cat <<'EOF'\n          # payload\n"
            "          EOF\n          echo done # block\n"
            "      - shell: python\n        run: |\n          s = \"\"\"\n"
            "          # inside string\n          \"\"\"\n          print(s) # Python step\n",
            ["# quoted", "# escaped", "# block header", "# block", "# Python step"]),
        ".github/workflows/default.yml": (
            "env:\n  run: 'ENVIRONMENT # data'\njobs:\n  sample:\n    defaults:\n"
            "      run:\n        shell: python\n    steps:\n      - run: |\n"
            "          s = f\"\"\"inside {23}\n          # literal\n          \"\"\"\n"
            "          print(s) # actual\n",
            ["# actual"]),
        "Dockerfile": (
            "# syntax=docker/dockerfile:1\nFROM scratch\nRUN echo '# literal' # shell\n"
            "COPY <<END /text\n# payload\nEND\nRUN <<END\necho ok # docker heredoc\nEND\n",
            ["# syntax=docker/dockerfile:1", "# shell", "# docker heredoc"]),
        "Makefile": (
            ".ONESHELL:\ntarget:\n\t@cat <<'EOF'\n\t# literal\n\tEOF\n"
            "\t@echo '# data' # recipe\n", ["# recipe"]),
        "multi.ts": ("const v = 0; /* multiple\nlines */", ["/* multiple\nlines */"]),
        "internal/dockerbackend/file_list.go": (
            "package guest\nconst listDirScript = `echo x\n# guest code\n`\n"
            "const sample = `# literal data`\n", ["# guest code"]),
    }
    for path, (source, _) in samples.items():
        target = repo / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(source)
    (repo / "alias.go").symlink_to("old.go")
    (repo / "binary.go").write_bytes(b"\x00// image bytes")
    run("git", "add", ".", cwd=repo)
    inventory = run(sys.executable, str(LINT), "--inventory", cwd=repo)
    rows = [json.loads(line) for line in inventory.stdout.splitlines()]
    for path, (source, expected) in samples.items():
        subset = [r for r in rows if r["path"] == path]
        require([r["text"] for r in subset] == expected, (path, subset))
        for row in subset:
            lines = source.split("\n")
            original = lines[row["start"] - 1].encode()[row["column"]:]
            require(original.startswith(row["text"].splitlines()[0].encode()), row)
    directives = {r["path"] for r in rows if r["directive"]}
    require({"late.py", "typed.ts", "Dockerfile"} <= directives, directives)
    unsupported = {"alias.go", "binary.go"} & {r["path"] for r in rows}
    require(not unsupported, unsupported)
    run("git", "-c", "user.name=E2E", "-c", "user.email=e2e@example.invalid",
        "commit", "-qm", "inventory fixtures", cwd=repo)
    previous = run(sys.executable, str(LINT), "--inventory", "--revision", "HEAD", cwd=repo)
    require(inventory.stdout == previous.stdout, "revision and worktree inventories differ")
    results["worktree and revision report precise inventory"] = {"files": sorted(samples),
                                                                  "comments": len(rows)}

evidence.parent.mkdir(parents=True, exist_ok=True)
evidence.write_text(json.dumps(results, indent=2) + "\n")
print(f"{len(results)} scenarios passed. Evidence in {evidence}")
