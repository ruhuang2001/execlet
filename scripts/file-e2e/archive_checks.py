"""Tar scenarios through real Docker; no host extraction."""
import io
import json
import os
import pathlib
import subprocess
import tarfile
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen


def tar_bytes(entries):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode='w', format=tarfile.PAX_FORMAT) as writer:
        for name, kind, data, target in entries:
            h = tarfile.TarInfo(name)
            h.type, h.mode, h.mtime = kind, 0o640 if kind in (tarfile.REGTYPE,tarfile.LNKTYPE) else 0o755, 1700000000
            h.linkname = target
            h.size = len(data) if kind == tarfile.REGTYPE else 0
            writer.addfile(h, io.BytesIO(data))
    return output.getvalue()


def check_archives(client, sid, out):
    assert client.exec(sid, ['sh', '-c', 'mkdir -p /archives/source /archives/python /archives/go /archives/typescript /archives/cli /archives/reject /outside; printf keep >/outside/sentinel; ln -s /outside /archives/escape']).exit_code == 0
    content = b'\x00\xffbinary\n'
    entries = [('tree',tarfile.DIRTYPE,b'',''),('tree/empty',tarfile.DIRTYPE,b'',''),('tree/data',tarfile.REGTYPE,content,''),('tree/zero',tarfile.REGTYPE,b'',''),('tree/é space',tarfile.REGTYPE,b'utf8',''),('tree/link',tarfile.SYMTYPE,b'','data'),('tree/hard',tarfile.LNKTYPE,b'','tree/data')]
    fixture = tar_bytes(entries)
    (out/'archive-fixture.tar').write_bytes(fixture)
    records = []

    def call(data, path='/archives/reject', status=400):
        url = os.environ['SANDBOX_DOMAIN'] + '/v1/sandboxes/' + sid + '/files/archive?' + urlencode({'path':path})
        req = Request(url, data=data, method='PUT', headers={'Content-Type':'application/x-tar','Authorization':'Bearer '+os.environ['SANDBOX_API_KEY']})
        try:
            with urlopen(req) as response: code,body = response.status,response.read()
        except HTTPError as exc: code,body=exc.code,exc.read()
        records.append({'path':path,'bytes':len(data),'status':code,'response':body.decode()})
        (out/'archive-responses.json').write_text(json.dumps(records,indent=2))
        assert code==status,(code,status,body)

    call(b'\0'*1024,status=204)
    client.write_archive(sid,'/archives/source',fixture)
    downloaded=client.read_archive(sid,'/archives/source/tree')
    (out/'archive-download.tar').write_bytes(downloaded)
    with tarfile.open(fileobj=io.BytesIO(downloaded),mode='r:') as archive:
        assert archive.extractfile('tree/data').read()==content
        assert archive.getmember('tree/data').mode==0o640
        assert archive.getmember('tree/empty').isdir()
        assert archive.getmember('tree/link').issym()
        assert archive.extractfile('tree/hard').read()==content
    client.write_archive(sid,'/archives/python',downloaded)
    assert client.read_file(sid,'/archives/python/tree/data')==content
    assert client.exec(sid,['sh','-c','cmp /archives/python/tree/link /archives/python/tree/data']).exit_code==0
    assert client.exec(sid,['sh','-c','test "$(stat -c %i /archives/python/tree/data)" = "$(stat -c %i /archives/python/tree/hard)"']).exit_code==0
    for sdk,command in [('go',[str(out/'go-files'),sid,'archive',str(out/'archive-fixture.tar')]),('typescript',['node','scripts/file-e2e/typescript.mjs',sid,'archive',str(out/'archive-fixture.tar')]),('cli',[str(out/'sandboxctl'),'files','archive','upload',sid,'/archives/cli',str(out/'archive-fixture.tar')])]:
        result=subprocess.run(command,capture_output=True,text=True)
        (out/(sdk+'-archive.stdout')).write_text(result.stdout)
        (out/(sdk+'-archive.stderr')).write_text(result.stderr)
        assert result.returncode==0,(sdk,result.stderr)
        assert client.read_file(sid,'/archives/'+sdk+'/tree/data')==content
    result=subprocess.run([str(out/'sandboxctl'),'files','archive','download',sid,'/archives/cli/tree',str(out/'cli-download.tar')],capture_output=True,text=True)
    assert result.returncode==0,result.stderr
    with tarfile.open(out/'cli-download.tar') as archive: assert archive.extractfile('tree/data').read()==content
    safe=('good',tarfile.REGTYPE,b'not-written','')
    hostile=[('/absolute',tarfile.REGTYPE,b'x',''),('../outside',tarfile.REGTYPE,b'x',''),('x/../../escape',tarfile.REGTYPE,b'x',''),('device',tarfile.CHRTYPE,b'',''),('fifo',tarfile.FIFOTYPE,b'',''),('sparse',tarfile.GNUTYPE_SPARSE,b'',''),('link',tarfile.SYMTYPE,b'','/outside'),('link',tarfile.SYMTYPE,b'','../outside'),('link',tarfile.LNKTYPE,b'','/outside'),('link',tarfile.LNKTYPE,b'','../outside'),('dangling',tarfile.SYMTYPE,b'','absent')]
    for entry in hostile:
        call(tar_bytes([safe,entry]))
        assert client.list_files(sid,'/archives/reject').entries==[],entry
    for bad in [b'not tar',fixture[:-100],tar_bytes([('zeros',tarfile.REGTYPE,b'\0'*1024,'')])[:1536],fixture+b'garbage'+b'\0'*(10240-7),tar_bytes([safe,safe]),tar_bytes([('x',tarfile.REGTYPE,b'x',''),('x/child',tarfile.REGTYPE,b'x','')])]:
        call(bad)
        assert client.list_files(sid,'/archives/reject').entries==[]
    call(tar_bytes([('dir',tarfile.DIRTYPE,b'',''),('link',tarfile.SYMTYPE,b'','dir'),('link/file',tarfile.REGTYPE,b'x','')]))
    call(tar_bytes([('target',tarfile.LNKTYPE,b'','later'),('later',tarfile.REGTYPE,b'x','')]))
    for metadata in [{'GNU.sparse.size':'1'}, {'SCHILY.xattr.user.test':'bad'}]:
        buffer=io.BytesIO()
        with tarfile.open(fileobj=buffer,mode='w',format=tarfile.PAX_FORMAT) as writer:
            h=tarfile.TarInfo('bad');h.pax_headers=metadata;writer.addfile(h)
        call(buffer.getvalue())
    call(tar_bytes([('x',tarfile.REGTYPE,b'x','')]),'/archives/escape')
    call(tar_bytes([('escape/sentinel',tarfile.REGTYPE,b'bad','')]),'/archives')
    assert client.read_file(sid,'/outside/sentinel')==b'keep'
    call(fixture,'/not-created',404)
    call(fixture,'/outside/sentinel',400)
    call(tar_bytes([('huge',tarfile.REGTYPE,b'x'*(16*1024*1024+1),'')]),status=413)
    call(tar_bytes([(str(i),tarfile.REGTYPE,b'','') for i in range(4097)]),status=413)
    call(b'\0'*(32*1024*1024+1),status=413)
    assert client.list_files(sid,'/archives/reject').entries==[]
    client.pause_sandbox(sid)
    call(tar_bytes([safe,hostile[0]]))
    assert client.get_sandbox(sid).state=='paused'
    assert client.read_archive(sid,'/archives/source/tree')
    assert client.get_sandbox(sid).state=='running'
    result=subprocess.run([str(out/'sandboxctl'),'files','archive','download',sid,'/large',str(out/'should-not-exist.tar')],capture_output=True,text=True)
    assert result.returncode!=0 and not (out/'should-not-exist.tar').exists()
    (out/'archive-results.json').write_text(json.dumps({'status':'passed','invalid_or_limited_uploads':len(records),'sdks':['go','python','typescript'],'cli':['upload','download']},indent=2))
