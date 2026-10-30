# SPDX-License-Identifier: Apache-2.0
"""Shared HTTP transfer and daemon resource helpers."""
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request
from support.connect import BASE, HTTP

def status(path):
    try:
        with HTTP.open(BASE + path, timeout=5) as response:
            response.read()
            return response.status
    except HTTPError as error:
        error.close()
        return error.code

def memory(pid):
    values = dict(line.split(':', 1) for line in Path(f'/proc/{pid}/status').read_text().splitlines())
    return int(values['VmRSS'].split()[0])

def post(path, content=b'new payload', query=None):
    params = {'path': str(path), **(query or {})}
    try:
        with HTTP.open(Request(BASE + '/files?' + urlencode(params), content,
                               {'Content-Type': 'application/octet-stream'}), timeout=10) as response:
            return response.status, response.read()
    except HTTPError as error:
        return error.code, error.read()


def resources(pid):
    return len(list(Path(f'/proc/{pid}/fd').iterdir())), len(list(Path(f'/proc/{pid}/task').iterdir()))
