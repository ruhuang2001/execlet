import argparse
import json
import os
import re
import subprocess
from bisect import bisect_right
from collections import Counter
from functools import cache
from pathlib import Path

from tree_sitter_language_pack import get_parser


LANGUAGES = {
    ".go": "go", ".py": "python", ".ts": "typescript", ".tsx": "tsx",
    ".js": "javascript", ".mjs": "javascript", ".cjs": "javascript",
    ".sh": "bash", ".bash": "bash", ".yaml": "yaml", ".yml": "yaml",
    ".toml": "toml", ".mk": "make", ".md": "markdown", ".json": "json",
    "Makefile": "make", "Dockerfile": "dockerfile", "go.mod": "gomod",
    ".gitignore": "gitignore", "requirements.txt": "requirements",
}
FENCES = {**{key.lstrip("."): value for key, value in LANGUAGES.items()},
          "shell": "bash", "python": "python", "typescript": "typescript"}


def git(*args):
    return subprocess.check_output(["git", *args])


def paths(raw):
    return {Path(os.fsdecode(item)) for item in raw.split(b"\0") if item}


def language(path):
    if path.parts[0] == ".githooks":
        return "bash"
    return LANGUAGES.get(path.name) or LANGUAGES.get(path.suffix)


SUPPRESSION = re.compile(r"(?:/{2}\s*@ts-expect-error\b|#\s*noqa\b)", re.I)
SHELLS = {"bash": "bash", "sh": "bash", "python": "python", "python3": "python",
          "pwsh": "powershell", "powershell": "powershell"}
GO_DIRECTIVES = re.compile(
    r"//go:(?:build|embed|generate|linkname|debug|wasmimport|wasmexport) [^\n]+|"
    r"//go:(?:noescape|nosplit|noinline|norace|nointerface|uintptrescapes)|"
    r"// \+build [\w!., /-]+|//line \S+:\d+(?::\d+)?"
)


def is_directive(lang, text, row, col):
    if row == col == 0 and lang in {"bash", "python", "javascript", "typescript"}:
        if re.fullmatch(r"#!\s*/\S+(?:\s+[-\w./]+)*", text):
            return True
    if lang == "dockerfile" and row == col == 0:
        return bool(re.fullmatch(r"#\s*syntax\s*=\s*[\w./:@+-]+\s*", text, re.I))
    if lang in {"typescript", "tsx", "javascript", "python"} and SUPPRESSION.match(text):
        return True
    return lang == "go" and bool(GO_DIRECTIVES.fullmatch(text))


def mapping_value(mapping, key):
    if mapping:
        for pair in mapping.named_children:
            name = pair.child_by_field_name("key")
            if name and name.text.strip(b"'\x22") == key:
                return pair.child_by_field_name("value")
    return None


def in_workflow_step(pair):
    parent = pair.parent
    while parent:
        if parent.type == "block_mapping_pair":
            return parent.child_by_field_name("key").text.strip(b"'\x22") == b"steps"
        parent = parent.parent
    return False


def workflow_shell(pair):
    shell = mapping_value(pair.parent, b"shell")
    ancestor = pair.parent
    while shell is None and ancestor:
        defaults = mapping_value(ancestor, b"defaults")
        for name in (b"run", b"shell"):
            defaults = mapping_value(defaults.named_children[0], name) if defaults else None
        shell, ancestor = defaults, ancestor.parent
    command = shell.text.strip(b"'\x22").split()[0].decode() if shell and shell.text else "bash"
    return SHELLS.get(command)


def python_source(command):
    if not command or command.type != "command":
        return None, None
    args = list(command.children_by_field_name("argument"))
    name = command.child_by_field_name("name").text.strip(b"'\x22")
    if name.split(b"/")[-1] == b"execletctl" and args and args[0].text == b"run":
        separator = next((i for i, arg in enumerate(args) if arg.text == b"--"), len(args))
        args = args[separator + 1:]
        if not args:
            return None, None
        name = args.pop(0).text.strip(b"'\x22")
        if name == b"code" and len(args) == 1:
            return "code", args[0]
    if (name.split(b"/")[-1] not in {b"python", b"python3"}
            and name not in {b"$PYTHON", b"${PYTHON:-python3}"}):
        return None, None
    while args:
        flag = args[0].text
        if flag == b"-c" and len(args) >= 2:
            return "code", args[1]
        if flag == b"-":
            return "stdin", None
        if flag in {b"-X", b"-W", b"--check-hash-based-pycs"} and len(args) >= 2:
            args = args[2:]
        elif flag.startswith((b"-X", b"-W")) and len(flag) > 2:
            args = args[1:]
        elif re.fullmatch(rb"-[bBdEiIOPqRsStuvx]+", flag):
            args = args[1:]
        else:
            return None, None
    return "stdin", None


def unlabelled_example(source):
    if re.search(rb"(?m)^\s*(?:package \w+|func \w+\(|[A-Z]\w+\(ctx[, )])", source):
        return "go"
    if re.search(rb"(?m)^\s*(?:\$ |go (?:run|build|test) |(?:bash|curl|docker|env|npm|pip|python3?) )", source):
        return "bash"
    return None


parser_for = cache(get_parser)


def scan(path, data):
    if b"\x00" in data:
        return []
    starts = [0, *(m.end() for m in re.finditer(b"\n", data))]
    lines = data.split(b"\n")
    found = []

    def point(offset):
        row = bisect_right(starts, offset) - 1
        return row, offset - starts[row]

    def region(lang, source, where):
        def body(syntax, node, left=0, right=0):
            if syntax:
                region(syntax, node.text[left:len(node.text)-right],
                       lambda i: where(node.start_byte + left + i))

        def mapped(syntax, parts):
            parts = [(chunk, offset) for chunk, offset in parts if chunk]
            if not syntax or not parts:
                return
            positions = [offset + i for chunk, offset in parts for i in range(len(chunk))]
            positions.append(parts[-1][1] + len(parts[-1][0]))
            region(syntax, b"".join(chunk for chunk, _ in parts), lambda i: where(positions[i]))

        def workflow(value, shell):
            raw, offset = value.text, value.start_byte
            if raw.startswith((b"|", b">")):
                block = value.named_children[0]
                for child in block.named_children:
                    visit(child)
                body_lines = raw.splitlines(keepends=True)[1:]
                shift = min((len(s) - len(s.lstrip(b" ")) for s in body_lines if s.strip()), default=0)
                offset += raw.index(b"\n") + 1 if body_lines else len(raw)
                parts = []
                for i, line in enumerate(body_lines):
                    skip = min(shift, len(line) - len(line.lstrip(b" ")))
                    chunk = line[skip:]
                    if (raw.startswith(b">") and chunk.strip() and not chunk.startswith(b" ")
                            and i + 1 < len(body_lines) and body_lines[i + 1][shift:].strip()
                            and not body_lines[i + 1][shift:].startswith(b" ")):
                        chunk = chunk.rstrip(b"\r\n") + b" "
                    parts.append((chunk, offset + skip))
                    offset += len(line)
                mapped(shell, parts)
            elif raw.startswith((b"'", b'"')):
                parts = []
                pattern = rb"''|." if raw.startswith(b"'") else rb'\\(?:u[0-9a-fA-F]{4}|.)|.'
                for match in re.finditer(pattern, raw[1:-1], re.S):
                    token = match[0]
                    if token == b"''":
                        token = b"'"
                    elif raw.startswith(b'"') and token.startswith(b"\\"):
                        try:
                            token = json.loads(b'"' + token + b'"').encode()
                        except ValueError:
                            token = token[1:]
                    parts.append((token, offset + 1 + match.start()))
                mapped(shell, parts)
            else:
                body(shell, value)

        def visit(node):
            kind = node.type
            if kind in {"comment", "line_comment", "block_comment", "hash_bang_line"}:
                row, col = point(where(node.start_byte))
                end, end_col = point(where(node.end_byte))
                value = node.text.decode(errors="replace")
                if lang == "gomod" and value == "// indirect" and node.parent.type == "require_spec":
                    previous = node.prev_named_sibling
                    if previous and previous.type == "version" and previous.end_point.row == node.start_point.row:
                        return
                found.append(dict(path=str(path), start=row + 1, end=end + 1, column=col,
                                  end_column=end_col, text=value,
                                  standalone=not lines[row][:col].strip() and not lines[end][end_col:].strip(),
                                  directive=is_directive(lang, value, row, col)))
                return
            if lang == "bash" and kind == "command":
                mode, program = python_source(node)
                if mode == "code":
                    if program.type == "raw_string":
                        body("python", program, 1, 1)
                    elif program.type == "string" and all(c.type == "string_content" for c in program.named_children):
                        parts = []
                        for match in re.finditer(rb'\\([$"`\\\n])|[^\\]+|\\', program.text[1:-1]):
                            if match[1] != b"\n":
                                start = match.start(1) if match[1] else match.start()
                                parts.append((match[1] or match[0], program.start_byte + 1 + start))
                        mapped("python", parts)
            if lang == "bash" and kind == "heredoc_redirect" and node.text.startswith((b"<<", b"0<<")):
                command = node.parent.child_by_field_name("body")
                fields = {child.type: child for child in node.named_children}
                if command and command.text == b"cat" and "pipeline" in fields:
                    command = fields["pipeline"].named_children[0]
                mode, _ = python_source(command)
                payload = fields.get("heredoc_body")
                if (mode == "stdin" and payload
                        and all(c.type == "heredoc_content" for c in payload.named_children)):
                    body("python", payload)
            if lang == "markdown" and kind in {"fenced_code_block", "indented_code_block"}:
                fields = {child.type: child for child in node.named_children}
                content = fields.get("code_fence_content") if kind == "fenced_code_block" else node
                if content:
                    info = fields.get("info_string")
                    syntax = FENCES.get(info.text.decode().split()[0]) if info else unlabelled_example(content.text)
                    body(syntax, content)
                return
            if lang == "make" and kind == "recipe" and re.search(rb"(?m)^\.ONESHELL\s*:", source):
                parts = []
                for line in node.named_children:
                    if line.type == "recipe_line":
                        skip = len(line.text) - len(line.text.lstrip(b"@+-"))
                        parts.append((line.text[skip:], line.start_byte + skip))
                        parts.append((b"\n", line.end_byte))
                mapped("bash", parts)
                return
            if lang == "make" and kind == "recipe_line":
                shift = len(node.text) - len(node.text.lstrip(b"@+-"))
                body("bash", node, shift)
                return
            if lang == "dockerfile" and kind == "shell_command":
                body("bash", node)
                return
            if lang == "dockerfile" and kind == "heredoc_block":
                command = node.prev_named_sibling
                if command and command.type == "shell_command" and re.fullmatch(rb"<<-?['\x22]?\w+['\x22]?", command.text):
                    end = node.named_children[-1]
                    body("bash", node, right=node.end_byte-end.start_byte)
                return
            if (lang == "go" and kind == "const_spec" and path.parts[:2] in
                    {("internal", "dockerbackend"), ("internal", "dockerapi")}):
                name = node.child_by_field_name("name")
                expr = node.child_by_field_name("value")
                if name and name.text.endswith(b"Script") and expr and expr.text.startswith(b"`"):
                    body("bash", expr, 1, 1)
                    return
            if lang == "yaml" and path.parts[:2] == (".github", "workflows") and kind == "block_mapping_pair":
                key, value = node.child_by_field_name("key"), node.child_by_field_name("value")
                if key and key.text.strip(b"'\x22") == b"run" and in_workflow_step(node) and value and value.named_children:
                    if value.named_children[0].type in {"plain_scalar", "single_quote_scalar",
                                                        "double_quote_scalar", "block_scalar"}:
                        workflow(value, workflow_shell(node))
                        return
            for child in node.named_children:
                visit(child)

        visit(parser_for(lang).parse(source).root_node)

    if syntax := language(path):
        region(syntax, data, lambda i: i)
    return found


def revision_paths(revision):
    result = set()
    for record in git("ls-tree", "-rz", revision).split(b"\0"):
        if record.startswith((b"100644 ", b"100755 ")):
            result.add(Path(os.fsdecode(record.split(b"\t", 1)[1])))
    return result


def main():
    cli = argparse.ArgumentParser(description="Reject new prose comments relative to a Git revision.")
    cli.add_argument("--base", help="comparison revision (default: merge base with origin/main)")
    cli.add_argument("--inventory", action="store_true", help="emit all comments as JSON Lines")
    cli.add_argument("--revision", help="read the inventory from this commit instead of the worktree")
    args = cli.parse_args()
    if args.revision and not args.inventory:
        cli.error("--revision requires --inventory")
    os.chdir(os.fsdecode(git("rev-parse", "--show-toplevel")).strip())
    if args.inventory:
        files = (revision_paths(args.revision) if args.revision
                 else paths(git("ls-files", "-coz", "--exclude-standard")))
        for path in sorted(files):
            if not language(path):
                continue
            if args.revision:
                data = git("show", f"{args.revision}:{path}")
            elif path.is_file() and not path.is_symlink():
                data = path.read_bytes()
            else:
                continue
            for comment in scan(path, data):
                print(json.dumps(comment, ensure_ascii=False))
        return 0

    base = args.base
    if base is None:
        merge = subprocess.run(["git", "merge-base", "HEAD", "origin/main"], capture_output=True)
        if merge.returncode:
            cli.error("cannot resolve the merge base with origin/main; provide --base REVISION")
        base = merge.stdout.decode().strip()
    base = git("rev-parse", "--verify", base + "^{commit}").decode().strip()
    changed = paths(git("diff", "--no-renames", "--name-only", "-z", base, "--"))
    changed.update(paths(git("ls-files", "--others", "--exclude-standard", "-z")))
    previous_files = revision_paths(base)
    additions = 0
    for path in sorted(changed):
        if not language(path) or not path.is_file() or path.is_symlink():
            continue
        before = scan(path, git("show", f"{base}:{path}")) if path in previous_files else []
        allowed = Counter(c["text"] for c in before if not c["directive"] or SUPPRESSION.match(c["text"]))
        for c in scan(path, path.read_bytes()):
            if c["directive"] and not SUPPRESSION.match(c["text"]):
                continue
            if allowed[c["text"]]:
                allowed[c["text"]] -= 1
            else:
                print(f"{path}:{c['start']}: new code comment: {c['text'].splitlines()[0]}")
                additions += 1
    return int(additions > 0)


if __name__ == "__main__":
    raise SystemExit(main())
