# SPDX-License-Identifier: Apache-2.0
"""Real 640-second idle timeout; run separately from the short Cargo CI suite."""
import concurrent.futures
import http.client
import json
import socket
import time
import unittest

from support.guest import GuestDaemon, isolate_network


class GuestIdle(unittest.TestCase):
    def test_production_timeout_and_active_stream(self):
        with GuestDaemon() as daemon:
            idle = http.client.HTTPConnection('127.0.0.1', daemon.port, timeout=680)
            silent = socket.create_connection(('127.0.0.1', daemon.port), timeout=680)
            active = http.client.HTTPConnection('127.0.0.1', daemon.port, timeout=680)
            try:
                idle.request('GET', '/health')
                response = idle.getresponse()
                self.assertEqual(response.status, 204)
                response.read()
                started = time.monotonic()
                payload = json.dumps({'process': {'cmd': '/bin/sh', 'args': ['-c', 'sleep 650; printf active']}}).encode()
                active.request('POST', '/process.Process/Start', b'\0' + len(payload).to_bytes(4, 'big') + payload,
                               {'Content-Type': 'application/connect+json'})
                stream = active.getresponse()
                self.assertEqual(stream.status, 200)
                prefix = stream.read(5)
                self.assertEqual(prefix[0], 0)
                first = json.loads(stream.read(int.from_bytes(prefix[1:], 'big')))
                self.assertIn('start', first['event'])
                with concurrent.futures.ThreadPoolExecutor() as workers:
                    closed = workers.submit(idle.sock.recv, 1)
                    remaining = workers.submit(stream.read)
                    self.assertEqual(closed.result(timeout=675), b'')
                    elapsed = time.monotonic() - started
                    self.assertGreaterEqual(elapsed, 630)
                    self.assertLess(elapsed, 670)
                    print(f'Idle connection closed after {elapsed:.2f}s', flush=True)
                    data = remaining.result(timeout=35)
                output = b''
                ended = False
                import base64
                while data:
                    size = int.from_bytes(data[1:5], 'big')
                    frame = json.loads(data[5:5+size])
                    self.assertNotIn('error', frame)
                    event = frame.get('event', {})
                    output += base64.b64decode(event.get('data', {}).get('stdout', ''))
                    if 'end' in event:
                        self.assertEqual(event['end'].get('exitCode', 0), 0)
                        ended = True
                    data = data[5+size:]
                self.assertEqual(output, b'active')
                self.assertTrue(ended)
                silent.sendall(b'GET /health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n')
                self.assertIn(b'204', silent.recv(4096))
                print('Active Process stream and unused connection survived 650s', flush=True)
            finally:
                idle.close()
                silent.close()
                active.close()


if __name__ == '__main__':
    isolate_network()
    unittest.main()
