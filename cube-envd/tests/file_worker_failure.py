# SPDX-License-Identifier: Apache-2.0
"""File worker creation failure in an explicitly isolated daemon-only cgroup.

The runner supplies ENVD_WORKER_CGROUP, enables pids in its private namespace,
and moves only the disposable daemon there. Never point this at host cgroups.
"""
import json
import os
from pathlib import Path
import tempfile
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request
from support.connect import BASE, HTTP


def request(method, path, body=None, content_type='application/json'):
    try:
        with HTTP.open(Request(BASE + path, body, {'Content-Type': content_type},
                               method=method), timeout=5) as response:
            return response.status, response.read()
    except HTTPError as error:
        with error:
            return error.code, error.read()


def main():
    pid = int(os.environ['ENVD_SMOKE_PID'])
    group = Path(os.environ['ENVD_WORKER_CGROUP'])
    assert set((group / 'cgroup.procs').read_text().split()) == {str(pid)}
    maximum = group / 'pids.max'
    original = maximum.read_text()
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        source = root / 'source'
        source.write_bytes(b'source')
        target = root / 'target'
        target.write_bytes(b'preserved')
        path = '/files?' + urlencode({'path': str(target)})
        compose = json.dumps({'source_paths': [str(source)], 'destination': str(target)}).encode()
        assert request('GET', path) == (200, b'preserved')
        try:
            maximum.write_text((group / 'pids.current').read_text())
            for _ in range(3):
                for method, route, body, content_type in [
                    ('GET', path, None, 'application/json'),
                    ('POST', path, b'rejected', 'application/octet-stream'),
                    ('POST', '/files/compose', compose, 'application/json'),
                ]:
                    status, body = request(method, route, body, content_type)
                    assert status == 503, (method, route, status, body)
                    assert json.loads(body)['code'] == 503
                    assert request('GET', '/health')[0] == 204
                assert target.read_bytes() == b'preserved'
                assert source.read_bytes() == b'source'
                assert sorted(p.name for p in root.iterdir()) == ['source', 'target']
        finally:
            maximum.write_text(original)
        assert request('POST', path, b'recovered', 'application/octet-stream')[0] == 200
        assert request('GET', path) == (200, b'recovered')
        assert request('POST', '/files/compose', compose)[0] == 200
        assert request('GET', path) == (200, b'source')
        assert not source.exists()
    print('kernel pids exhaustion: GET/upload/compose return 503 without mutation; health and recovery passed')


if __name__ == '__main__':
    main()
