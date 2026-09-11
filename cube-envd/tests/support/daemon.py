# SPDX-License-Identifier: Apache-2.0
"""Shared isolated real-daemon HTTP fixture. Requires a private writable cgroup."""
import base64
import http.client
import json
import os
import socket
import subprocess
import tempfile
import time
import unittest
from urllib.parse import urlsplit


class DaemonTestCase(unittest.TestCase):
    def setUp(self):
        binary = getattr(self, 'daemon_binary', os.environ.get('ENVD_TEST_BINARY'))
        if binary:
            with open('/proc/self/status') as status:
                caps = dict(line.split(':', 1) for line in status if line.startswith('Cap'))
            for name in ('CapEff', 'CapBnd', 'CapInh', 'CapAmb'):
                self.assertFalse(int(caps[name], 16) & (1 << 25), name + ' contains SYS_TIME')
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            os.environ['ENVD_TEST_URL'] = f'http://127.0.0.1:{port}'
            log = tempfile.TemporaryFile()
            self.addCleanup(log.close)
            child = subprocess.Popen([*getattr(self, 'daemon_prefix', []), binary, '-isnotfc', '-port', str(port)], stdout=log, stderr=log, umask=getattr(self, 'daemon_umask', -1))
            self.daemon_pid = child.pid
            def stop():
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
            self.addCleanup(stop)
        deadline = time.monotonic() + 15
        while True:
            try:
                if self.request('GET', '/health')[0] == 204:
                    break
            except (OSError, http.client.HTTPException):
                pass
            if time.monotonic() >= deadline:
                self.fail('daemon did not become ready')
            time.sleep(0.05)

    def request(self, method, path, body=None, headers=None):
        url = urlsplit(getattr(self, 'endpoint', os.environ.get('ENVD_TEST_URL', 'http://127.0.0.1:49983')))
        connection = http.client.HTTPConnection(url.hostname, url.port, timeout=getattr(self, 'request_timeout', 15))
        try:
            connection.request(method, url.path.rstrip('/') + path, body, {**getattr(self, 'proxy_headers', {}), **(headers or {})})
            response = connection.getresponse()
            return response.status, response.headers, response.read()
        finally:
            connection.close()

    def command(self, command, token=None):
        headers = {'Content-Type':'application/connect+json'}
        if token:
            headers['X-Access-Token'] = token
        data = json.dumps({'process':{'cmd':'/bin/sh', 'args':['-c', command]}}).encode()
        code, _, body = self.request('POST', '/process.Process/Start', b'\0' + len(data).to_bytes(4,'big') + data, headers)
        self.assertEqual(code, 200)
        stdout = b''
        ended = False
        while body:
            length = int.from_bytes(body[1:5], 'big')
            frame = json.loads(body[5:5+length])
            body = body[5+length:]
            self.assertNotIn('error', frame)
            event = frame.get('event', {})
            stdout += base64.b64decode(event.get('data', {}).get('stdout', ''))
            if 'end' in event:
                self.assertEqual(event['end'].get('exitCode', 0), 0)
                ended = True
        self.assertTrue(ended)
        return stdout
