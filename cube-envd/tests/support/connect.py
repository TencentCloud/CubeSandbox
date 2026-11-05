# SPDX-License-Identifier: Apache-2.0
"""Connect black-box assertions executed inside the exact development image."""
import base64
import json
import os
import struct
import time
import urllib.request

BASE = os.environ['ENVD_SMOKE_URL']
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def request(path, value, stream=False):
    payload = json.dumps(value).encode()
    if stream:
        payload = b'\0' + struct.pack('>I', len(payload)) + payload
    req = urllib.request.Request(BASE + path, payload, {
        'Content-Type': 'application/connect+json' if stream else 'application/json',
        'Connect-Protocol-Version': '1',
    })
    response = HTTP.open(req, timeout=10)
    if stream:
        return Stream(response)
    with response:
        body = response.read()
    return json.loads(body) if body else None


class Stream:
    def __init__(self, response):
        self.response = response
        self.first = self.read()

    def read(self):
        header = self.response.read(5)
        assert len(header) == 5, header
        flag, length = struct.unpack('>BI', header)
        body = self.response.read(length)
        assert len(body) == length
        return flag, json.loads(body)

    def pid(self):
        assert self.first[0] == 0 and 'start' in self.first[1]['event'], self.first
        return self.first[1]['event']['start']['pid']

    def close(self):
        self.response.close()

    def output(self, channel='stdout'):
        frame = self.read()
        assert frame[0] == 0 and channel in frame[1]['event'].get('data', {}), frame
        return base64.b64decode(frame[1]['event']['data'][channel])

    def finish(self, code=0):
        self.pid()
        output = {'stdout': bytearray(), 'stderr': bytearray()}
        ended = False
        try:
            while True:
                flag, frame = self.read()
                if flag == 2:
                    assert frame == {} and ended, frame
                    assert self.response.read() == b''
                    return {key: bytes(value) for key, value in output.items()}
                event = frame['event']
                assert not ended and 'start' not in event, event
                if 'end' in event:
                    assert event['end'].get('exitCode', 0) == code, event
                    assert event['end']['exited'], event
                    ended = True
                for channel, data in event.get('data', {}).items():
                    output[channel].extend(base64.b64decode(data))
        finally:
            self.close()


def start(config, **fields):
    return request('/process.Process/Start', dict(process=config, **fields), True)


def live():
    return request('/process.Process/List', {}).get('processes', [])


def success(stream):
    try:
        return stream.pid()
    finally:
        stream.close()


def error(stream, code):
    try:
        assert stream.first[0] == 2, stream.first
        assert stream.first[1]['error']['code'] == code, stream.first
        assert stream.response.read() == b''
    finally:
        stream.close()


def connect(**selector):
    return request('/process.Process/Connect', {'process': selector}, True)


def until(predicate):
    end = time.monotonic() + 5
    while time.monotonic() < end:
        if predicate():
            return
        time.sleep(.01)
    raise AssertionError('condition did not become true')
