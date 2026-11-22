"""Independent read/cleanup observer. Never creates, connects or renews."""
import json
import os
import re
import subprocess
import sys

provider, operation, sid, owner = sys.argv[1:5]


def docker(*args):
    return subprocess.run(['docker', *args], text=True, capture_output=True, check=True, timeout=15).stdout.strip()


if provider == 'tiny':
    assert re.fullmatch(r'e2b-contract-[a-f0-9]{32}', owner), 'refuse an unscoped owner'
    ids = docker('ps', '-aq', '--filter', 'label=tiny-sandbox.owner=' + owner).split()
    matches = []
    for cid in ids:
        try:
            obj = json.loads(docker('inspect', cid))[0]
        except subprocess.CalledProcessError as exc:
            if any(message in (exc.stderr or '').lower() for message in ('no such object', 'no such container')):
                continue
            raise
        assert obj['Config']['Labels'].get('tiny-sandbox.owner') == owner
        if operation in ('sweep', 'count') or obj['Name'] == '/tiny-sandbox-' + sid:
            matches.append(obj)
    if operation == 'count':
        answer = {'count':len(matches)}
    elif operation == 'process':
        identity = json.loads(sys.argv[5])
        assert type(identity['pid']) is int and identity['pid'] > 0
        answer = {'alive': False, 'identity': identity}
        for obj in matches:
            if obj['State'].get('Paused'):
                raise RuntimeError('paused container cannot establish process death')
            if not obj['State']['Running']:
                continue
            probe = ('import pathlib,json,sys; p=pathlib.Path("/proc/"+sys.argv[1]+"/stat");\n'
                     'try: s=p.read_text().rsplit(") ",1)[1].split()\n'
                     'except FileNotFoundError: s=None\n'
                     'print(json.dumps({"alive":bool(s and s[0] not in ("Z","X") and s[19]==sys.argv[2]),'
                     '"state":s[0] if s else None,"start_ticks":s[19] if s else None}))')
            try:
                answer.update(json.loads(docker('exec', obj['Id'], 'python3', '-c', probe,
                                                str(identity['pid']), str(identity['start_ticks']))))
            except subprocess.CalledProcessError as exc:
                if 'No such container' not in (exc.stderr or '') and 'is not running' not in (exc.stderr or ''):
                    raise
            answer['container'] = obj['Id']
    elif operation in ('cleanup','sweep'):
        for obj in matches:
            docker('rm','-f',obj['Id'])
        remaining = docker('ps','-aq','--filter','label=tiny-sandbox.owner=' + owner).split()
        assert not set(remaining) & {o['Id'][:12] for o in matches}
        answer = {'confirmed':True, 'removed':len(matches)}
    else:
        answer = {'alive':any(o['State']['Running'] for o in matches), 'exists':bool(matches)}
else:
    from e2b import Sandbox, SandboxNotFoundException
    if operation == 'cleanup':
        Sandbox.kill(sid, request_timeout=8)
    try:
        info = Sandbox.get_info(sid, request_timeout=8)
        answer = {'alive':str(info.state).lower().endswith('running'), 'exists':True,
                  'template':info.template_id, 'envd':info.envd_version, 'end':info.end_at.timestamp()}
    except SandboxNotFoundException:
        answer = {'alive':False, 'exists':False}
    if operation == 'cleanup':
        answer['confirmed'] = not answer['exists']
print(json.dumps(answer))
