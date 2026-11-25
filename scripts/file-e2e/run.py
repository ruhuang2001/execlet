"""Real-Docker file operation contract checks; outputs replayable evidence."""
import json
import os
import pathlib
import subprocess
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen
from execlet import Client, CreateSandboxRequest
from execlet.e2b import Sandbox as E2BSandbox
from archive_checks import check_archives

out = pathlib.Path(os.environ['E2E_ARTIFACT_DIR'])
client = Client()
results = []

def raw(sid, query, status=200):
    url = os.environ['SANDBOX_DOMAIN'] + '/v1/sandboxes/' + sid + '/files/list?' + query
    req = Request(url, headers={'Authorization': 'Bearer ' + os.environ['SANDBOX_API_KEY']})
    try:
        with urlopen(req,timeout=40) as response:
            code, data = response.status, response.read()
    except HTTPError as exc:
        code, data = exc.code, exc.read()
    payload = json.loads(data)
    results.append({'url': url, 'status': code, 'response': payload})
    (out / 'responses.json').write_text(json.dumps(results, indent=2))
    assert code == status, (url, code, payload, status)
    return payload

sb = client.create_sandbox(CreateSandboxRequest(image='alpine:3.21', timeout_seconds=300))
(out / 'sandbox-id.txt').write_text(sb.id)
try:
    command = ['sh', '-c', "mkdir -p /listing/empty /listing/nested; printf xyz > '/listing/a space'; chmod 640 '/listing/a space'; touch /listing/.hidden /listing/z /listing/é /listing/nested/child; ln -s nested /listing/link"]
    (out / 'fixture-command.json').write_text(json.dumps(command))
    ex = client.exec(sb.id, command)
    assert ex.exit_code == 0
    names, cursor = [], ''
    while True:
        page = client.list_files(sb.id, '/listing', limit=2, cursor=cursor)
        names.extend(e.name for e in page.entries)
        cursor = page.next_cursor
        if not cursor: break
    expected = ['.hidden', 'a space', 'empty', 'link', 'nested', 'z', 'é']
    assert names == expected, names
    (out / 'python.json').write_text(json.dumps({'sdk':'python','names':names}))
    listing = raw(sb.id, urlencode({'path':'/listing'}))
    entries = {e['name']:e for e in listing['entries']}
    assert entries['a space']['size'] == 3 and entries['a space']['mode'] == 0o640
    assert entries['empty']['is_dir'] and entries['nested']['is_dir']
    assert not entries['link']['is_dir'] and entries['link']['size']==len('nested')
    assert client.exec(sb.id,['sh','-c','mkdir -p /hardlinks/0nested; printf nonempty >/hardlinks/0nested/original; ln /hardlinks/0nested/original /hardlinks/zcopy; ln /hardlinks/0nested/original /hardlinks/acopy']).exit_code==0
    linked=raw(sb.id,'path=/hardlinks&limit=100')['entries']
    assert {e['name']:e['size'] for e in linked}=={'0nested':0,'acopy':8,'zcopy':8},linked
    assert all(e['mtime'].endswith('Z') for e in entries.values())
    assert raw(sb.id, urlencode({'path':'/listing/empty'})) == {'entries':[]}
    for params,status in [({'path':'/absent'},404),({'path':'/listing/z'},400),({'path':'relative'},400),({'path':'/listing/../listing'},400),({'path':'/listing\0'},400),({'path':'/'+'x'*4097},400)]:
        raw(sb.id,urlencode(params),status)
    for limit in ['0','-1','1001','abc','']:
        raw(sb.id,urlencode({'path':'/listing','limit':limit}),400)
    raw(sb.id,'path=/listing&limit=1&limit=2',400)
    raw(sb.id,'path=/listing&cursor=not-a-cursor',400)
    cursor = raw(sb.id,'path=/listing&limit=1')['next_cursor']
    raw(sb.id,urlencode({'path':'/listing/empty','cursor':cursor}),400)
    raw(sb.id,'path=%ZZ',400)
    client.pause_sandbox(sb.id)
    assert len(client.list_files(sb.id,'/listing').entries)==7
    assert client.get_sandbox(sb.id).state=='running'
    for name,cmd in [('go',[str(out/'go-files'),sb.id]),('typescript',['node','scripts/file-e2e/typescript.mjs',sb.id]),('cli',[str(out/'execletctl'),'files','list',sb.id,'/listing','--limit','2'])]:
        result=subprocess.run(cmd,text=True,capture_output=True,timeout=40)
        (out/(name+'.stdout')).write_text(result.stdout)
        (out/(name+'.stderr')).write_text(result.stderr)
        assert result.returncode==0,(name,result.stderr)
        payload=json.loads(result.stdout)
        if name=='cli': assert len(payload['entries'])==2 and payload['next_cursor']
    assert client.exec(sb.id,['sh','-c','mkdir /wide; i=0; while [ "$i" -lt 10001 ]; do touch /wide/$i; i=$((i+1)); done']).exit_code==0
    raw(sb.id,'path=/wide&limit=1',413)
    wide_info = E2BSandbox(sb, client, 30.0).files.write('/wide/python.txt', 'ok')
    assert (wide_info.path, wide_info.name, wide_info.type.value) == ('/wide/python.txt', 'python.txt', 'file')
    ts = subprocess.run(['node','scripts/file-e2e/typescript.mjs',sb.id,'wide'],capture_output=True,text=True,timeout=40,check=True)
    assert client.exec(sb.id,['sh','-c','printf original >/wide/target; ln -s target /wide/link']).exit_code == 0
    replacement = client.write_file(sb.id, '/wide/link', b'updated')
    assert (replacement.name,replacement.type,replacement.size) == ('link','file',7)
    assert client.read_file(sb.id, '/wide/target') == b'original'
    assert client.read_file(sb.id, '/wide/link') == b'updated'
    (out/'write-metadata.json').write_text(json.dumps({'initial_siblings':10001,'python_e2b':wide_info.path,'typescript_e2b':json.loads(ts.stdout.splitlines()[-1]),'replaced_symlink':replacement.type,'target_content':'original'}, indent=2))
    assert client.exec(sb.id,['sh','-c','mkdir -p /large/nested; dd if=/dev/zero of=/large/big bs=1048576 count=65 2>/dev/null; dd if=/dev/zero of=/large/nested/deep bs=1048576 count=65 2>/dev/null']).exit_code==0
    large_listing = raw(sb.id,'path=/large&limit=10',200)
    assert {e['name']: e['size'] for e in large_listing['entries']} == {'big': 65 << 20, 'nested': 0}, large_listing
    e2b_written = E2BSandbox(sb, client, 30.0).files.write('/large/e2b-meta.txt', 'ok')
    assert e2b_written.name == 'e2b-meta.txt' and e2b_written.path == '/large/e2b-meta.txt'
    check_archives(client, sb.id, out)
    client.kill_sandbox(sb.id)
    raw(sb.id,'path=/listing',409)
    (out/'results.json').write_text(json.dumps({'status':'passed','listing_http_cases':len(results),'archive_report':'archive-results.json','memory_report':'memory-results.json','names':names},indent=2))
finally:
    client.kill_sandbox(sb.id)
