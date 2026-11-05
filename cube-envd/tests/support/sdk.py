# SPDX-License-Identifier: Apache-2.0
"""Shared init fixture for filesystem SDK checks."""
import json
from urllib.request import Request
from support.connect import BASE, HTTP

def init(user, workdir):
    payload = json.dumps({'defaultUser': user, 'defaultWorkdir': workdir}).encode()
    with HTTP.open(Request(BASE + '/init', payload,
                           {'Content-Type': 'application/json'})) as response:
        assert response.status == 204
