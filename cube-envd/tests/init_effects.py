# SPDX-License-Identifier: Apache-2.0
"""Run with ENVD_TEST_BINARY in a disposable container (modifies guest trust/hosts).

python3 -B -m unittest discover -s cube-envd/tests -p init_effects.py -v
NFS assertions cover command invocation and retry; they do not certify an NFS mount.
"""
import http.client
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest


class InitEffects(unittest.TestCase):
    def test_guest_init_effects(self):
        self.assertTrue(Path('/.dockerenv').exists(), 'requires a disposable container')
        status = dict(line.split(':', 1) for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
        for key in ['CapInh', 'CapPrm', 'CapEff', 'CapBnd', 'CapAmb']:
            self.assertFalse(int(status[key], 16) & (1 << 25), key)
        binary = os.environ['ENVD_TEST_BINARY']
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            calls = root / 'calls'
            # Only init's external mount commands use these fixture executables.
            for program in ['mkdir', 'mount']:
                script = root / program
                script.write_text('#!/usr/bin/python3\nimport json,sys\nfrom pathlib import Path\n'
                                  f'with open({str(calls)!r}, "a") as f: f.write(json.dumps(sys.argv)+"\\n")\n'
                                  f'failure=Path({str(root / "fail")!r})\n'
                                  'sys.exit(1 if sys.argv[0].endswith("mount") and failure.exists() else 0)\n')
                script.chmod(0o755)
            log = tempfile.TemporaryFile(mode='w+')
            self.addCleanup(log.close)
            child = subprocess.Popen([binary, '--port', '0', '--isnotfc', '--log-format', 'json'],
                                     stdout=log, stderr=log,
                                     env={**os.environ, 'PATH': f'{root}:/usr/bin:/bin'})
            def stop():
                if child.poll() is None:
                    child.terminate()
                    try:
                        child.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        child.kill()
                        child.wait()
            self.addCleanup(stop)
            deadline = time.monotonic() + 15
            port = None
            while time.monotonic() < deadline:
                self.assertIsNone(child.poll(), 'daemon exited before listening')
                log.seek(0)
                for line in log:
                    event = json.loads(line)
                    if event.get('fields', {}).get('message') == 'envd listening':
                        port = int(event['fields']['addr'].rsplit(':', 1)[1])
                if port is not None:
                    break
                time.sleep(0.02)
            self.assertIsNotNone(port, 'daemon listening deadline')

            def init(payload):
                connection = http.client.HTTPConnection('127.0.0.1', port, timeout=10)
                try:
                    connection.request('POST', '/init', json.dumps(payload))
                    response = connection.getresponse()
                    body = response.read()
                    self.assertEqual(response.status, 204, body)
                finally:
                    connection.close()

            def eventually(predicate):
                end = time.monotonic() + 5
                while time.monotonic() < end:
                    if predicate():
                        return
                    time.sleep(0.02)
                self.fail('init side effect did not finish')

            bundle = Path('/etc/ssl/certs/ca-certificates.crt')
            original = bundle.read_bytes()
            self.addCleanup(bundle.write_bytes, original)
            cert = 'CUBE_ENVD_TEST_CA_FIRST'
            init({'caBundle': cert})
            eventually(lambda: cert in bundle.read_text())
            init({'caBundle': cert})
            self.assertEqual(bundle.read_text().count(cert), 1)
            replacement = 'CUBE_ENVD_TEST_CA_SECOND'
            init({'caBundle': replacement})
            eventually(lambda: replacement in bundle.read_text() and cert not in bundle.read_text())
            self.assertIn(replacement, Path('/usr/local/share/ca-certificates/e2b-ca.crt').read_text())

            hosts = Path('/etc/hosts')
            original_hosts = hosts.read_bytes()
            self.addCleanup(hosts.write_bytes, original_hosts)
            init({'hyperloopIP': '192.0.2.10'})
            eventually(lambda: '192.0.2.10\tevents.e2b.local' in hosts.read_text())

            mounts = {'volumeMounts': [{'nfs_target': '192.0.2.1:/export', 'path': '/mnt/c1-test'}]}
            (root / 'fail').touch()
            init(mounts)
            (root / 'fail').unlink()
            init(mounts)
            count = len(calls.read_text().splitlines())
            self.assertEqual(count, 4)
            init(mounts)
            self.assertEqual(len(calls.read_text().splitlines()), count)
            invocation = json.loads(calls.read_text().splitlines()[-1])
            self.assertEqual(invocation[1:4], ['-v', '-t', 'nfs'])
            self.assertEqual(invocation[-2:], ['192.0.2.1:/export', '/mnt/c1-test'])
            self.assertIn('nfsvers=3', invocation[5])
            stop()
            self.assertEqual(child.returncode, 0)
