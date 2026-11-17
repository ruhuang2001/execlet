"""Harness self-check. Proves the gate rejects, not that E2B passed."""
from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import runner  # noqa: E402

python = sys.executable
failures = []


def check(name, condition, evidence=None):
    print(('ok   ' if condition else 'FAIL ') + name + ('' if condition else f'  {evidence}'))
    if not condition:
        failures.append(name)


def invoke(*args, env=None):
    return subprocess.run([python, str(HERE / 'runner.py'), *args], capture_output=True, text=True,
                          env={**os.environ, **(env or {})}, timeout=120)


# 1. Manifest and fixtures are internally consistent and asymmetric.
listed = invoke('--list')
check('--list exits 0', listed.returncode == 0, listed.stderr)
ids = [line.split()[0] for line in listed.stdout.splitlines()]
check('--list covers exactly the manifest', ids == list(runner.CASES), ids)
fixtures = json.loads((HERE / 'fixtures.json').read_text())
check('exit code is not 0/1', 'SystemExit(37)' in fixtures['output'])
check('stdout and stderr differ', fixtures['stdout'] != fixtures['stderr'])
check('env value looks like injection', '$(touch' in fixtures['env_value'])
check('listing crosses the 1000-entry page', '1007' in fixtures['listing'])


# 2. The digest is reproducible and changes with content.
first = runner.digest()
check('digest is stable', first == runner.digest())
temp = HERE / '.digest-probe.txt'
temp.write_text('probe')
try:
    check('digest covers fixtures', runner.digest() != first)
finally:
    temp.unlink()


# 3. Missing prerequisites are not_run, never pass.
with tempfile.TemporaryDirectory() as tmp:
    out = Path(tmp) / 'cloud'
    result = invoke('run', '--provider', 'e2b', '--out', str(out))
    report = json.loads((out / 'report.json').read_text())
    check('cloud without --allow-cloud is not_run', result.returncode == 2, result.stdout + result.stderr)
    check('cloud refusal is recorded per row', all(r['status'] == 'not_run' for r in report['rows'].values()))
    out = Path(tmp) / 'noimage'
    result = invoke('run', '--provider', 'tiny', '--out', str(out), '--cases', 'A01', '--languages', 'python')
    check('tiny without an image is not_run', result.returncode == 2, result.stdout + result.stderr)
    out = Path(tmp) / 'stale'
    out.mkdir()
    result = invoke('run', '--provider', 'tiny', '--out', str(out), '--cases', 'A01', '--image', 'x')
    check('an existing evidence directory is refused', result.returncode == 1 and 'exists' in result.stderr.lower(),
          result.stderr)
    for args in (['--cases', 'A99'], ['--languages', 'rust'], ['--cases', 'A01,A01']):
        result = invoke('run', '--provider', 'tiny', '--out', str(Path(tmp) / ('bad' + args[-1])), *args,
                        '--image', 'x')
        check(f'rejects {args[0]}={args[1]}', result.returncode == 1, result.stdout + result.stderr)


# 4. The comparison gate is driven by reference.json, not by a cloud capture.
def good(**mutate):
    rows = {}
    for case in runner.CASES:
        for language in runner.LANGUAGES:
            checks = {name: True for name in runner.check_names(case, language)}
            rows[f'{language}:{case}'] = {'status': 'passed', 'complete': True, 'checks': checks,
                                          'cleanup': {'confirmed': True}}
    report = {'schema': 1, 'provider': 'tiny', 'baseline': runner.MANIFEST['baseline'],
              'digest': runner.digest(), 'languages': list(runner.LANGUAGES), 'complete': True,
              'selected': list(runner.CASES), 'rows': rows}
    mutate.get('apply', lambda r: None)(report)
    return report


def compare_case(name, expect_pass, report=None):
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / 'tiny.json'
        path.write_text(json.dumps(report if report is not None else good()))
        result = subprocess.run([python, str(HERE / 'runner.py'), 'compare', str(path)],
                                capture_output=True, text=True, timeout=120)
        check(name, (result.returncode == 0) == expect_pass, result.stdout + result.stderr)


reference = json.loads((HERE / 'reference.json').read_text())
check('reference covers every case', set(reference['cases']) == set(runner.CASES))
check('reference derives from pinned 2.64.0', reference['baseline'] == '2.64.0'
      and 'source-derived' in reference['reference_kind'])
check('reference demands real checks', all(c['checks'] for c in reference['cases'].values()))
check('reference names no cloud requirement', 'cloud' not in json.dumps(reference['cases']).lower())

compare_case('gate accepts a fully proven report', True)
compare_case('gate rejects a failed row', False,
             report=good(apply=lambda r: r['rows']['python:A04'].update(status='failed')))
compare_case('gate rejects a partial selection', False,
             report=good(apply=lambda r: r['rows'].pop('typescript:W03')))
compare_case('gate rejects a skipped check', False,
             report=good(apply=lambda r: r['rows']['python:A10']['checks'].pop('symlink target')))
compare_case('gate rejects a false check value', False,
             report=good(apply=lambda r: r['rows']['python:A02'].update(checks={'bash pipeline output': False})))
compare_case('gate rejects a check reported as failed', False,
             report=good(apply=lambda r: r['rows']['python:A01'].update(
                 checks={**r['rows']['python:A01']['checks'], 'kill returns true': {'passed': False}})))
for unproven in (None, {}, {'evidence': 'no verdict'}, {'passed': 'true'}, 1):
    compare_case(f'gate rejects unproven check {unproven!r}', False,
                 report=good(apply=lambda r: r['rows']['python:A01']['checks'].update(
                     {'kill returns true': unproven})))
compare_case('gate rejects missing cleanup evidence', False,
             report=good(apply=lambda r: r['rows']['python:A01'].update(cleanup={'confirmed': False})))
compare_case('gate rejects an unfinished run', False,
             report=good(apply=lambda r: r.update(complete=False)))
compare_case('gate rejects a stale suite digest', False,
             report=good(apply=lambda r: r.update(digest='0' * 64)))
compare_case('gate rejects a narrowed selection', False,
             report=good(apply=lambda r: r.update(selected=['A01'])))
compare_case('gate rejects one language', False,
             report=good(apply=lambda r: r.update(languages=['python'])))
compare_case('gate rejects the reference provider', False,
             report=good(apply=lambda r: r.update(provider='e2b')))
# An alternative check is satisfied only when one of its names is present, so a
# case that proved neither path of an either/or bound must still fail the gate.
def drop_both_bound_checks(report):
    checks = report['rows']['python:A13']['checks']
    checks.pop('event-capped success keeps every line')
    checks.pop('event cap is explicit')


compare_case('gate accepts either alternative of a bound', True,
             report=good(apply=lambda r: r['rows']['python:A13']['checks'].pop(
                 'event-capped success keeps every line')))
compare_case('gate rejects a bound with no alternative proven', False,
             report=good(apply=drop_both_bound_checks))
# The millisecond spelling exists only in TypeScript, so python must not need it.
compare_case('gate scopes a language-only check', True,
             report=good(apply=lambda r: r['rows']['python:A06']['checks'].pop('1501 ms becomes 2 s', None)))
compare_case('gate rejects a missing check in its own language', False,
             report=good(apply=lambda r: r['rows']['typescript:A06']['checks'].pop('1501 ms becomes 2 s')))

if failures:
    print(json.dumps({'selfcheck': 'failed', 'failures': failures}, indent=2))
    sys.exit(1)
print('SELFCHECK PASS: harness mechanics only; no case or E2B behavior is claimed.')
