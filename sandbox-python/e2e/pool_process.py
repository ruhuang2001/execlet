"""Single-threaded subprocess probes against the E2E runner's real daemon."""
import json
import os
import resource
import select
import signal
import sys
import traceback

from execlet import Client, Error


def high_fd():
    soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    if soft != resource.RLIM_INFINITY and soft < 1100:
        resource.setrlimit(resource.RLIMIT_NOFILE, (1100, hard))
    fds = []
    try:
        while not fds or fds[-1] < 1024:
            fds.append(os.open(os.devnull, os.O_RDONLY))
        try:
            select.select([fds[-1]], [], [], 0)
        except ValueError:
            pass
        else:
            raise AssertionError('fixture did not reach the select() FD limit')
        with Client(max_retries=0) as client:
            first = client.get_capabilities().capabilities
            assert 'files.list.v1' in first and client.get_capabilities().capabilities == first
        return {'highest_reserved_fd': fds[-1], 'successful_requests': 2}
    finally:
        for fd in fds:
            os.close(fd)


def forked():
    with Client(max_retries=0) as client:
        expected = client.get_capabilities().capabilities
        assert 'files.list.v1' in expected
        pid = os.fork()
        if pid == 0:
            signal.alarm(8)
            try:
                for call in (client.get_capabilities, client.close):
                    try:
                        call()
                    except Error as exc:
                        assert exc.code == 'forked_client', exc
                    else:
                        raise AssertionError('inherited client was accepted')
                with Client(max_retries=0) as fresh:
                    assert fresh.get_capabilities().capabilities == expected
            except BaseException:
                traceback.print_exc()
                os._exit(1)
            os._exit(0)
        _, status = os.waitpid(pid, 0)
        assert os.waitstatus_to_exitcode(status) == 0, status
        assert client.get_capabilities().capabilities == expected
    return {'inherited_request_and_close': 'forked_client', 'fresh_child_and_parent': 'ok'}


if __name__ == '__main__':
    print(json.dumps({'high-fd': high_fd, 'fork': forked}[sys.argv[1]]()))
