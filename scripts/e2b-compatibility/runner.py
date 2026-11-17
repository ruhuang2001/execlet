"""Executable contract runner. No cloud resources without --allow-cloud."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import sys
import threading
import time
import uuid

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
MANIFEST = json.loads((HERE / 'cases.json').read_text())
CASES = {c['id']: c for c in MANIFEST['cases']}
CASE_NAMES = list(CASES)
# The reference is the pinned E2B 2.64.0 implementation, recorded as a
# source-derived contract. A cloud capture is an optional drift check, not
# the basis for compatibility, so it never appears in this file's inputs.
REFERENCE = json.loads((HERE / 'reference.json').read_text())
LANGUAGES = ['python', 'typescript']


class NotRun(Exception):
    pass


def scrub(value):
    text = json.dumps(value, ensure_ascii=False)
    for key in ('E2B_API_KEY', 'SANDBOX_API_KEY', 'CONTRACT_SECRET'):
        secret = os.environ.get(key)
        if secret:
            text = text.replace(secret, '<redacted>')
    return json.loads(text)


def save(path, value):
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(scrub(value), ensure_ascii=False, indent=2) + '\n')
    temporary.replace(path)


def digest():
    h = hashlib.sha256()
    for path in sorted(HERE.iterdir()):
        if path.suffix in {'.py', '.ts', '.json', '.sh', '.txt'} or path.name == 'README.md':
            h.update(path.name.encode() + b'\0' + path.read_bytes())
    return h.hexdigest()


class Worker:
    """One SDK process. The deadline bounds one call, not the whole case."""

    def __init__(self, language, env, out, seconds=None):
        self.out = out
        self.call_budget = seconds or float(os.environ.get('CONTRACT_CALL_SECONDS', 30))
        self.deadline = time.monotonic() + self.call_budget
        self.queue = queue.Queue()
        self.env = env
        command = ([env.get('CONTRACT_PYTHON', str(HERE / '.venv/bin/python')), str(HERE / 'worker.py')]
                   if language == 'python' else [env.get('NODE', 'node'), str(HERE / 'dist/suite.js')])
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                        text=True, encoding='utf-8', env=env, cwd=HERE)
        def output():
            for line in self.process.stdout:
                self.queue.put(line)
            self.queue.put(None)
        def errors():
            with out.with_suffix('.stderr.log').open('w') as f:
                for line in self.process.stderr:
                    f.write(scrub(line)); f.flush()
        self.threads = [threading.Thread(target=output, daemon=True), threading.Thread(target=errors, daemon=True)]
        for thread in self.threads:
            thread.start()
        try:
            self.identity = self.receive()
            if not self.identity.get('ready'):
                if self.identity.get('status') == 'not_run':
                    raise NotRun(self.identity['reason'])
                raise RuntimeError(self.identity)
        except BaseException:
            self.close()
            raise

    def receive(self):
        self.deadline = time.monotonic() + self.call_budget
        try:
            line = self.queue.get(timeout=max(.01, self.deadline - time.monotonic()))
        except queue.Empty:
            raise TimeoutError('SDK worker watchdog expired') from None
        if line is None:
            raise RuntimeError('SDK worker exited without a response')
        return json.loads(line)

    def call(self, op, **kwargs):
        request = dict(op=op, **kwargs)
        self.process.stdin.write(json.dumps(request, ensure_ascii=False) + '\n')
        self.process.stdin.flush()
        reply = self.receive()
        # Payloads are deterministic fixtures, but avoid 10 MiB data dumps.
        def compact(v):
            if isinstance(v, str) and len(v) > 2048:
                return {'characters':len(v), 'sha256':hashlib.sha256(v.encode()).hexdigest()}
            if isinstance(v, list):
                return [compact(i) for i in v] if len(v) <= 32 else {'count':len(v), 'sha256':hashlib.sha256(json.dumps(v).encode()).hexdigest()}
            if isinstance(v, dict):
                return {k:compact(i) for k, i in v.items()}
            return v
        with self.out.open('a') as f:
            f.write(json.dumps(scrub({'at':time.monotonic(), 'request':compact(request), 'response':compact(reply)})) + '\n')
        return reply

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.process.kill(); self.process.wait()
        for pipe in (self.process.stdin, self.process.stdout, self.process.stderr):
            pipe.close()
        for thread in self.threads:
            thread.join(timeout=2)


def observer(provider, op, sid, env, owner=''):
    """Separate process, no connect/renew as a liveness probe."""
    run = subprocess.run([env.get('CONTRACT_PYTHON', str(HERE / '.venv/bin/python')), str(HERE / 'observe.py'),
                          provider, op, sid, owner], env=env, capture_output=True, text=True, timeout=25)
    if run.returncode:
        raise RuntimeError('independent observer failed: ' + scrub(run.stderr[-2000:]))
    return json.loads(run.stdout)


def cleanup(provider, directory, env, owner):
    ids = []
    owned = directory / 'owned.jsonl'
    if owned.exists():
        ids = sorted({json.loads(line)['id'] for line in owned.read_text().splitlines()})
    result = {'confirmed':True, 'ids':[], 'owner':owner}
    for sid in ids:
        try:
            result['ids'].append({'id':sid, **observer(provider, 'cleanup', sid, env, owner)})
        except Exception as e:
            result['ids'].append({'id':sid, 'confirmed':False, 'error':str(e)})
    if provider == 'tiny':
        try:
            result['orphans'] = observer(provider, 'sweep', '-', env, owner)
        except Exception as e:
            result['orphans'] = {'confirmed':False, 'error':str(e)}
    result['confirmed'] = all(i.get('confirmed') is True for i in result['ids']) and result.get('orphans', {'confirmed':True})['confirmed']
    save(directory / 'cleanup.json', result)
    return result


def one_case(args):
    from scenarios import Context, SCENARIOS
    from local import Local
    out = Path(args.out)
    env = dict(os.environ)
    local = None
    context = None
    record = {'status':'failed', 'complete':False, 'checks':{}}
    try:
        if args.provider == 'tiny':
            local = Local(out, env['CONTRACT_OWNER'], env['CONTRACT_SERVER'], env['CONTRACT_IMAGE'])
            local.start()
            env.update(local.environment(args.language))
        context = Context(args.provider, args.language, out, env, CASES[args.case]['budget'], local)
        SCENARIOS[args.case](context)
        record.update(status='passed', complete=True, checks=context.checks, identity=context.worker.identity)
    except NotRun as e:
        record.update(status='not_run', reason=str(e))
    except Exception as e:
        record.update(reason=type(e).__name__ + ': ' + str(e))
    finally:
        if context:
            record['checks'] = context.checks
            context.close()
        if local:
            local.close()
        save(out / 'case.json', record)
    return 0 if record['status'] == 'passed' else 2 if record['status'] == 'not_run' else 1


def run(args):
    selected = list(CASES) if args.cases == 'all' else args.cases.split(',')
    languages = args.languages.split(',')
    if len(set(selected)) != len(selected) or not set(selected) <= CASES.keys():
        raise ValueError('unknown or duplicate case IDs')
    if len(set(languages)) != len(languages) or not set(languages) <= set(LANGUAGES):
        raise ValueError('unknown or duplicate languages')
    if args.provider == 'e2b':
        selected = [i for i in selected if CASES[i]['kind'] == 'parity']
    if not selected or not languages:
        raise ValueError('empty run selection')
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, CONTRACT_PROVIDER=args.provider)
    env.pop('PYTHONOPTIMIZE', None)
    env['PYTHONUNBUFFERED'] = '1'
    env['CONTRACT_PYTHON'] = args.python
    if args.provider == 'tiny':
        env = {k:v for k,v in env.items() if not k.startswith('E2B_')}
    else:
        env = {k:v for k,v in env.items() if not k.startswith('SANDBOX_')}
    env.pop('CONTRACT_OPTIONS', None)
    env.pop('CONTRACT_MODULE', None)
    if args.module:
        if args.provider == 'e2b':
            raise ValueError('--module applies only to the tiny provider')
        env['CONTRACT_MODULE'] = args.module
    rows = {f'{lang}:{case}':{'status':'not_run', 'reason':'not dispatched'} for lang in languages for case in selected}
    report = {'schema':1, 'provider':args.provider, 'baseline':MANIFEST['baseline'], 'digest':digest(),
              'languages':languages, 'selected':selected, 'started':time.time(), 'complete':False, 'rows':rows,
              'source':subprocess.check_output(['git','rev-parse','HEAD'], cwd=ROOT, text=True).strip(),
              'dirty':subprocess.check_output(['git','status','--porcelain'], cwd=ROOT, text=True).splitlines()}
    save(out / 'report.json', report)
    reason = None
    if args.provider == 'e2b' and not args.allow_cloud:
        reason = 'E2B Cloud requires explicit --allow-cloud (billable resources)'
    elif args.provider == 'e2b' and not env.get('E2B_API_KEY'):
        reason = 'E2B_API_KEY is absent'
    elif args.provider == 'tiny' and not args.image:
        reason = 'provide --image with a prebuilt compatibility image digest; no Alpine/root fallback'
    if reason:
        for row in rows.values():
            row['reason'] = reason
        save(out / 'report.json', report); print(reason); return 2
    try:
        if 'typescript' in languages:
            node = env.get('NODE', 'node')
            out_version = subprocess.run([node, '-v'], capture_output=True, text=True, timeout=20)
            parts = out_version.stdout.strip().lstrip('v').split('.')
            ok = out_version.returncode == 0 and (int(parts[0]), int(parts[1])) >= (22, 19)
            if not ok:
                raise RuntimeError(f'Node >=22.19 required by pinned E2B dependencies; got '
                                   f'{out_version.stdout.strip() or "nothing"} (set NODE=...)')
            report['node'] = out_version.stdout.strip()
        # Import-only preflight; no create or other network operation.
        for language in languages:
            worker = Worker(language, dict(env, CONTRACT_OWNED=str(out / 'preflight-owned.jsonl')), out / f'preflight-{language}.jsonl', 60)
            report.setdefault('workers', {})[language] = worker.identity
            worker.close()
        if args.provider == 'tiny':
            if not (args.image.startswith('sha256:') or '@sha256:' in args.image):
                raise ValueError('--image must identify immutable image content')
            check = subprocess.run(['docker','image','inspect',args.image], capture_output=True, text=True, timeout=20)
            if check.returncode:
                raise NotRun('compatibility image is not installed locally')
            images = json.loads(check.stdout)
            report['image'] = [{'id':i['Id'], 'digests':i.get('RepoDigests', [])} for i in images]
            server = out / 'sandboxd'
            with (out / 'build.log').open('w') as log:
                subprocess.run(['go','build','-o',str(server),'./cmd/sandboxd'], cwd=ROOT, stdout=log, stderr=log, check=True, timeout=120)
            env.update(CONTRACT_SERVER=str(server), CONTRACT_IMAGE=args.image)
    except Exception as e:
        status = 'not_run' if isinstance(e, (NotRun, FileNotFoundError)) else 'failed'
        for row in rows.values():
            row.update(status=status, reason=str(e))
        save(out / 'report.json', report); print(status + ': ' + str(e)); return 2 if status == 'not_run' else 1
    for language in languages:
        for case in selected:
            directory = out / f'{language}-{case}'
            directory.mkdir()
            owner = 'e2b-contract-' + uuid.uuid4().hex
            childenv = dict(env, CONTRACT_OWNED=str(directory / 'owned.jsonl'), CONTRACT_OWNER=owner)
            command = [sys.executable, str(HERE / 'runner.py'), '_case', '--provider',args.provider, '--language',language,
                       '--case',case,'--out',str(directory)]
            row = rows[f'{language}:{case}']
            with (directory / 'runner.log').open('w') as log:
                child = subprocess.Popen(command, env=childenv, stdout=log, stderr=log, start_new_session=True)
                try:
                    child.wait(timeout=CASES[case]['budget'] + 25)
                    if (directory / 'case.json').exists():
                        row.update(json.loads((directory / 'case.json').read_text()))
                        if row['status'] == 'passed':
                            row.pop('reason', None)
                    else:
                        row.update(status='failed', reason='missing worker result')
                    # The child's own report is authoritative; the exit code only
                    # has to agree that the result was persisted at all.
                    if child.returncode not in (0, 1, 2):
                        row.update(status='failed', reason=f'case process crashed: {child.returncode}')
                except subprocess.TimeoutExpired:
                    row.update(status='failed', reason='case watchdog expired')
                finally:
                    try:
                        os.killpg(child.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    child.wait()
                    row['cleanup'] = cleanup(args.provider, directory, childenv, owner)
                    if not row['cleanup']['confirmed']:
                        row.update(status='failed', reason='cleanup unconfirmed')
            print(f'{language} {case}: {row["status"]}', flush=True)
            save(out / 'report.json', report)
    report['complete'] = True
    report['finished'] = time.time()
    save(out / 'report.json', report)
    return 1 if any(r['status'] == 'failed' for r in rows.values()) else 2 if any(r['status'] == 'not_run' for r in rows.values()) else 0


def expected_checks(case, language=None):
    """Every assertion the reference contract says this case must prove.

    A plain name is required. An entry with `any` is satisfied by one of its
    names, because some cases prove a bound on either the success or the error
    path. An entry with `language` applies only to that language, since an
    option spelled in milliseconds can only be exercised there.
    """
    expected = []
    for entry in REFERENCE['cases'][case]['checks']:
        if isinstance(entry, dict):
            if entry.get('language') not in (None, language):
                continue
            expected.append(entry)
        else:
            expected.append({'any': [entry]})
    return expected


def missing_checks(expected, have):
    """Names of expectations the report did not satisfy."""
    missing = []
    for entry in expected:
        names = entry['any'] if isinstance(entry, dict) else [entry]
        if not any(name in have for name in names):
            missing.append(' | '.join(names))
    return missing


def check_names(case, language=None):
    """Every concrete name a report may carry for a case, alternatives included."""
    names = []
    for entry in expected_checks(case, language):
        names.extend(entry['any'])
    return names


def compare(tiny_only):
    """Gate on the recorded reference contract. No cloud run is required."""
    report = json.loads(Path(tiny_only).read_text())
    errors = []
    if report.get('provider') != 'tiny' or report.get('schema') != 1 or not report.get('complete'):
        errors.append('incomplete or wrong-provider report')
    if report.get('baseline') != MANIFEST['baseline'] or report.get('digest') != digest():
        errors.append('baseline or suite digest mismatch')
    if report.get('selected') != list(CASES):
        errors.append('not the full case selection')
    if sorted(report.get('languages', [])) != sorted(LANGUAGES):
        errors.append('not both languages')
    rows = report.get('rows', {})
    for case, meta in CASES.items():
        for language in LANGUAGES:
            key = f'{language}:{case}'
            row = rows.get(key)
            if row is None:
                errors.append('missing ' + key)
                continue
            if row.get('status') != 'passed' or not row.get('complete'):
                errors.append(f'{key}: {row.get("status")} {row.get("reason", "")}'.strip())
                continue
            if row.get('cleanup', {}).get('confirmed') is not True:
                errors.append(key + ': cleanup unconfirmed')
            needed = expected_checks(case, language)
            have = row.get('checks') or {}
            missing = missing_checks(needed, have)
            if missing:
                errors.append(f'{key}: checks not executed: {missing}')
            failed = [name for name, value in have.items()
                      if not (value is True or (isinstance(value, dict) and value.get('passed') is True))]
            if failed:
                errors.append(f'{key}: failed or unproven checks: {failed}')
    if errors:
        print(json.dumps({'status': 'failed', 'reference': REFERENCE['reference_kind'],
                          'errors': errors}, ensure_ascii=False, indent=2))
        return 1
    print(f'PASS: recorded E2B {REFERENCE["baseline"]} subset checks '
          f'({len(CASES)} cases x {len(LANGUAGES)} languages, '
          f'{sum(len(expected_checks(c, language)) for c in CASES for language in LANGUAGES)} expectations)')
    print('Not claimed: a released adapter, live E2B drift, native regression gates, or any case outside this suite.')
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--list', action='store_true')
    sub = parser.add_subparsers(dest='action')
    p = sub.add_parser('run')
    p.add_argument('--provider', choices=['e2b','tiny'], required=True)
    p.add_argument('--out', required=True)
    p.add_argument('--languages', default=','.join(LANGUAGES))
    p.add_argument('--cases', default='all')
    p.add_argument('--python', default=str(HERE / '.venv/bin/python'))
    p.add_argument('--allow-cloud', action='store_true')
    p.add_argument('--image')
    p.add_argument('--module', help='tiny-provider adapter module; defaults to tiny_sandbox.e2b')
    p = sub.add_parser('compare')
    p.add_argument('report', help='tiny report.json to gate against reference.json')
    p = sub.add_parser('_case')
    for name in ('provider','language','case','out'):
        p.add_argument('--' + name, required=True)
    args = parser.parse_args()
    if args.list:
        for c in CASES.values():
            print(f'{c["id"]}  {c["kind"]:6}  {c["budget"]:3}s  {c["title"]}')
        return 0
    if args.action == 'run':
        return run(args)
    if args.action == 'compare':
        return compare(args.report)
    if args.action == '_case':
        return one_case(args)
    parser.error('choose --list, run or compare')


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, FileNotFoundError, FileExistsError, KeyError) as e:
        print(str(e), file=sys.stderr); sys.exit(1)
