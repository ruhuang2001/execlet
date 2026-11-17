"""Independent read/cleanup observer. Never creates, connects or renews."""
import json
import os
import re
import subprocess
import sys

provider, operation, sid, owner = sys.argv[1:]


def docker(*args):
    return subprocess.run(['docker', *args], text=True, capture_output=True, check=True, timeout=15).stdout.strip()


if provider == 'tiny':
    assert re.fullmatch(r'e2b-contract-[a-f0-9]{32}', owner), 'refuse an unscoped owner'
    ids = docker('ps', '-aq', '--filter', 'label=tiny-sandbox.owner=' + owner).split()
    matches = []
    for cid in ids:
        # A container can disappear between the listing and the inspect, and a
        # vanished container is simply not a match: treating that race as a
        # failure would turn a correct expiry into an observer error.
        try:
            obj = json.loads(docker('inspect', cid))[0]
        except subprocess.CalledProcessError as exc:
            if 'No such object' in (exc.stderr or ''):
                continue
            raise
        assert obj['Config']['Labels'].get('tiny-sandbox.owner') == owner
        if operation in ('sweep', 'count') or obj['Name'] == '/tiny-sandbox-' + sid:
            matches.append(obj)
    if operation == 'count':
        answer = {'count':len(matches)}
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
