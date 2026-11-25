"""Private Linux command supervisor. Invoked by the backend, not a guest API."""
import json
import os
import select
import signal
import socket
import sys
import time


def record(path, value):
    temporary = path + '.tmp'
    with open(temporary, 'x') as output:
        json.dump(value, output)
    os.replace(temporary, path)


def send(path, token, action):
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(5)
        try:
            conn.connect(path + '.sock')
        except (FileNotFoundError, ConnectionRefusedError):
            with open(path + '.result') as result:
                json.load(result)
            print('{"signaled":false}')
            return
        conn.sendall(json.dumps({'token': token, 'action': action}).encode() + b'\n')
        data = conn.makefile('rb').readline(4097)
        if len(data) > 4096:
            raise RuntimeError('oversized control response')
        print(data.decode(), end='')


def live_members(pid):
    members = []
    for name in os.listdir('/proc'):
        if not name.isdecimal():
            continue
        try:
            with open('/proc/' + name + '/stat') as entry:
                fields = entry.read().rsplit(') ', 1)[1].split()
            if int(fields[2]) == pid and fields[0] not in ('Z', 'X'):
                members.append(fields[0])
        except (OSError, ValueError, IndexError):
            pass
    return members


def kill_group(pid):
    os.killpg(pid, signal.SIGSTOP)
    until = time.monotonic() + 1
    while True:
        members = live_members(pid)
        if not members:
            return False
        if all(state in ('T', 't') for state in members):
            os.killpg(pid, signal.SIGKILL)
            return True
        if time.monotonic() >= until:
            os.killpg(pid, signal.SIGKILL)
            raise RuntimeError('signal delivery unconfirmed')
        time.sleep(.001)


def supervise(path, token, config):
    listener = socket.socket(socket.AF_UNIX)
    listener.bind(path + '.sock')
    os.chmod(path + '.sock', 0o600)
    listener.listen(8)
    reader, writer = os.pipe()
    pid = os.fork()
    if pid == 0:
        try:
            listener.close()
            os.close(reader)
            os.setsid()
            os.write(writer, b'1')
            os.close(writer)
            devnull = os.open('/dev/null', os.O_RDONLY)
            os.dup2(devnull, 0)
            if devnull > 2:
                os.close(devnull)
            if config.get('cwd'):
                try:
                    os.chdir(config['cwd'])
                except OSError as exc:
                    os.write(2, ('execlet: cwd: ' + str(exc) + '\n').encode())
                    os._exit(126)
            env = dict(os.environ)
            env.update(config.get('env', {}))
            os.execvpe(config['command'][0], config['command'], env)
        except BaseException as exc:
            os.write(2, ('execlet: exec: ' + str(exc) + '\n').encode())
            os._exit(127)
    os.close(writer)
    if os.read(reader, 1) != b'1':
        raise RuntimeError('command identity handshake failed')
    os.close(reader)
    record(path, {'pid': pid})
    deadline = time.monotonic() + config['timeout_ms'] / 1000 if config['timeout_ms'] else None
    reason = 'exited'
    try:
        while True:
            exited = os.waitid(os.P_PID, pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
            live = exited is None or bool(live_members(pid))
            if not live:
                break
            if deadline is not None and time.monotonic() >= deadline:
                if kill_group(pid) and reason == 'exited':
                    reason = 'timeout'
                deadline = None
            if not select.select([listener], [], [], .01)[0]:
                continue
            conn, _ = listener.accept()
            with conn:
                conn.settimeout(1)
                try:
                    request = json.loads(conn.makefile('rb').readline(4097))
                    if request.get('token') != token:
                        raise ValueError('invalid control identity')
                    action = request['action']
                    if action == 'kill':
                        signaled = kill_group(pid)
                    elif action == 'term':
                        signaled = bool(live_members(pid))
                        if signaled:
                            os.killpg(pid, signal.SIGTERM)
                    else:
                        raise ValueError('unknown action')
                    if signaled and reason == 'exited':
                        reason = 'canceled'
                    conn.sendall(json.dumps({'signaled': signaled}).encode() + b'\n')
                except (OSError, ValueError, KeyError, RuntimeError):
                    try:
                        conn.sendall(b'{"error":"signal_unconfirmed"}\n')
                    except OSError:
                        pass
        exited = os.waitid(os.P_PID, pid, os.WEXITED | os.WNOWAIT)
        code = exited.si_status if exited.si_code == os.CLD_EXITED else 128 + exited.si_status
        record(path + '.result', {'reason': reason, 'exit_code': code})
    finally:
        listener.close()
        os.unlink(path + '.sock')
    os.waitpid(pid, 0)
    sys.exit(code)


if __name__ == '__main__':
    if sys.argv[1] == 'signal':
        send(*sys.argv[2:])
    else:
        directory = sys.argv[4]
        with open(directory + '/input') as source:
            config = json.load(source)
        os.unlink(directory + '/input')
        os.rmdir(directory)
        supervise(sys.argv[2], sys.argv[3], config)
