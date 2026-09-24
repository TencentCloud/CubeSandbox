# SPDX-License-Identifier: Apache-2.0
"""Public Connect streaming watch helpers; no polling API or private queue access."""
import http.client
import json
import os
import struct
from urllib.parse import urlsplit

BASE = os.environ.get('ENVD_SMOKE_URL', 'http://127.0.0.1:49983')

class Watch:
    def __init__(self, path, recursive=True):
        address = urlsplit(BASE)
        self.connection = http.client.HTTPConnection(address.hostname, address.port, timeout=10)
        payload = json.dumps({'path': str(path), 'recursive': recursive}).encode()
        self.connection.request('POST', '/filesystem.Filesystem/WatchDir',
                                b'\0' + struct.pack('>I', len(payload)) + payload,
                                {'Content-Type': 'application/connect+json', 'Connect-Protocol-Version': '1'})
        self.response = self.connection.getresponse()
        assert self.response.status == 200, self.response.status

    def read(self):
        header = self.response.read(5)
        assert len(header) == 5, header
        flag, length = struct.unpack('>BI', header)
        return flag, json.loads(self.response.read(length))

    def start(self):
        assert self.read() == (0, {'start': {}})

    def event(self, name, kind):
        actual = self.read()
        assert actual == (0, {'filesystem': {'name': name, 'type': 'EVENT_TYPE_' + kind}}), actual

    def terminal(self, code):
        flag, body = self.read()
        assert flag == 2 and body['error']['code'] == code, (flag, body)

    def close(self):
        self.response.close()
        self.connection.close()
