# SPDX-License-Identifier: Apache-2.0
"""Public polling adapter for shared real-filesystem and syscall-fault scenarios."""
from collections import deque
import http.client
import json
import time
from urllib.parse import urlsplit
from watch_stream import BASE


def rpc(method, body):
    address = urlsplit(BASE)
    connection = http.client.HTTPConnection(address.hostname, address.port, timeout=10)
    try:
        connection.request('POST', '/filesystem.Filesystem/' + method, json.dumps(body),
                           {'Content-Type': 'application/json', 'Connect-Protocol-Version': '1'})
        response = connection.getresponse()
        return response.status, json.loads(response.read())
    finally:
        connection.close()


class Watch:
    def __init__(self, root, recursive=True):
        self.status, self.created = rpc('CreateWatcher', {'path': str(root), 'recursive': recursive})
        self.id = self.created.get('watcherId')
        self.pending = deque()

    def start(self):
        assert self.status == 200 and self.id.startswith('w'), self.created

    def read(self):
        if not self.id:
            return 2, {'error': self.created}
        deadline = time.monotonic() + 10
        while True:
            if self.pending:
                return 0, {'filesystem': self.pending.popleft()}
            status, body = rpc('GetWatcherEvents', {'watcherId': self.id})
            if status != 200:
                return 2, {'error': body}
            self.pending.extend(body.get('events', []))
            assert time.monotonic() < deadline, 'polling event deadline'
            if not self.pending:
                time.sleep(.001)

    def event(self, name, kind):
        observed = self.read()
        assert observed == (0, {'filesystem': {'name': name, 'type': 'EVENT_TYPE_' + kind}}), observed

    def terminal(self, code):
        flag, body = self.read()
        assert flag == 2 and body['error']['code'] == code, (flag, body)

    def close(self):
        if self.id:
            status, body = rpc('RemoveWatcher', {'watcherId': self.id})
            assert status == 200, body
            self.id = None
