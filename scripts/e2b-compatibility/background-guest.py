"""Uploaded deterministic workload, not an adapter. Runs only inside the guest."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

config = json.loads(sys.argv[1])
root = Path(config['root'])
root.mkdir(parents=True, exist_ok=True)
counter = root / 'counter'
counter.write_text(str(int(counter.read_text()) + 1) if counter.exists() else '1')


def identity():
    stat = Path('/proc/self/stat').read_text().rsplit(') ', 1)[1].split()
    return {'pid': os.getpid(), 'start_ticks': stat[19], 'group': os.getpgrp()}


(root / 'identity.json').write_text(json.dumps(identity()))


def term(_sig, _frame):
    (root / 'TERM').write_text('received')


signal.signal(signal.SIGTERM, term)
child = None
if config.get('child'):
    child = subprocess.Popen([sys.executable, '-c',
        'import os,time,json,pathlib,sys; p=pathlib.Path(sys.argv[1]); '
        's=pathlib.Path("/proc/self/stat").read_text().rsplit(") ",1)[1].split(); '
        'p.joinpath("child.json").write_text(json.dumps({"pid":os.getpid(),"start_ticks":s[19],"group":os.getpgrp()}));\n'
        'while True:\n p.joinpath("child-heartbeat").write_text(str(time.monotonic())); time.sleep(.05)',
        str(root)])
    while not (root / 'child.json').exists():
        time.sleep(.01)

for stream, text in ((1, config.get('pre_out', 'PRE:世界🌍\n')),
                     (2, config.get('pre_err', 'PRE:err🙂\n'))):
    os.write(stream, text.encode())
(root / 'READY').write_text('ready')


def gate(name):
    deadline = time.monotonic() + 180
    while not (root / name).exists():
        (root / 'heartbeat').write_text(str(time.monotonic()))
        if time.monotonic() >= deadline:
            raise SystemExit(98)
        time.sleep(.02)


gate('GO')
if config.get('oom'):
    blocks = []
    while True:
        blocks.append(bytearray(4 * 1024 * 1024))
if config.get('event_steps'):
    for i in range(config['event_steps']):
        os.write(1, f'event:{i:04}\n'.encode())
        deadline = time.monotonic() + 20
        while not (root / 'ACK').exists() or (root / 'ACK').read_text() != str(i):
            if time.monotonic() >= deadline:
                raise SystemExit(97)
            time.sleep(.001)
for i in range(config.get('lines', 12)):
    for stream, text in ((1, f'out:{i:02}:世界🌍\n'), (2, f'err:{i:02}:不同🙂\n')):
        data = text.encode()
        os.write(stream, data[:7])
        os.write(stream, data[7:])
    time.sleep(config.get('interval', .025))
(root / 'POST').write_text('emitted')
if config.get('second_gate'):
    gate('FINISH')
data = (root / 'input.bin').read_bytes()
(root / 'artifact.bin').write_bytes(data[::-1] + b'\x00built\xff')
(root / 'AFTER').write_text('complete')
if child:
    child.kill()
    child.wait()
raise SystemExit(config.get('exit', 0))
