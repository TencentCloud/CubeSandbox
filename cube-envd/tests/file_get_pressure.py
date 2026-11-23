# SPDX-License-Identifier: Apache-2.0
"""Measure stalled GET state and cancellation on an isolated running daemon."""
import http.client
import os
from pathlib import Path
import tempfile
import time
from urllib.parse import urlencode, urlsplit

from support.files import memory, status, resources
from support.connect import BASE





def main():
    pid = int(os.environ['ENVD_SMOKE_PID'])
    url = urlsplit(BASE)
    with tempfile.TemporaryDirectory(prefix='file-get-pressure-') as directory:
        path = Path(directory) / 'large'
        with path.open('wb') as file:
            block = os.urandom(1024 * 1024)
            for _ in range(64):
                file.write(block)
        baseline = memory(pid)
        baseline_resources = resources(pid)
        for encoding in ('identity', 'gzip'):
            peers = []
            try:
                for _ in range(8):
                    connection = http.client.HTTPConnection(url.hostname, url.port, timeout=5)
                    connection.request('GET', '/files?' + urlencode({'path': str(path)}),
                                       headers={'Accept-Encoding': encoding})
                    response = connection.getresponse()
                    assert response.status == 200
                    peers.append((connection, response))
                time.sleep(.5)
                saturated = memory(pid)
                assert status('/files?' + urlencode({'path': str(path)})) == 200
                assert status('/health') == 204
                # Buffering eight 64 MiB files would violate this by a wide margin;
                # the allowance includes allocators, thread stacks and gzip state.
                assert saturated - baseline < 64 * 1024, (baseline, saturated)
                print(encoding, 'eight stalled 64 MiB files; RSS KiB', baseline, saturated, flush=True)
            finally:
                for connection, response in peers:
                    response.close()
                    connection.close()
            deadline = time.monotonic() + 5
            while resources(pid)[1] > baseline_resources[1] or resources(pid)[0] > baseline_resources[0] + 1:
                assert time.monotonic() < deadline, ('resources leaked after disconnect', baseline_resources, resources(pid))
                time.sleep(.02)
        print('bounded stalled-client memory, health and cancellation passed', flush=True)


if __name__ == '__main__':
    main()
