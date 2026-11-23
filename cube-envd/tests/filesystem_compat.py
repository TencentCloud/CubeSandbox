# SPDX-License-Identifier: Apache-2.0
"""Retained filesystem compatibility regressions against a fresh real daemon."""
import http.client
import json
import os
import secrets
import time
import unittest
import tempfile
import subprocess
from pathlib import Path
from urllib.parse import urlsplit
from support.connect import BASE, start


class FilesystemCompatibility(unittest.TestCase):
    def setUp(self):
        os.environ['ENVD_TEST_URL'] = BASE
        self.daemon_pid = int(os.environ['ENVD_SMOKE_PID'])

    def request(self, method, path, body=None, headers=None):
        url = urlsplit(BASE)
        connection = http.client.HTTPConnection(url.hostname, url.port, timeout=10)
        try:
            connection.request(method, path, body, headers or {})
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            connection.close()

    def command(self, command):
        return start({'cmd': '/bin/sh', 'args': ['-c', command]}).finish()['stdout'].decode()

    @staticmethod
    def frame(payload):
        body = json.dumps(payload).encode()
        return b'\0' + len(body).to_bytes(4, 'big') + body

    def test_watched_directory_self_events_keep_the_directory_name(self):
        root = '/tmp/envd-watch-self-' + secrets.token_hex(8)
        self.command('mkdir ' + root)
        url = urlsplit(getattr(self, 'endpoint', os.environ['ENVD_TEST_URL']))
        connection = http.client.HTTPConnection(url.hostname, url.port, timeout=10)
        try:
            connection.request('POST', url.path.rstrip('/') + '/filesystem.Filesystem/WatchDir', self.frame({'path':root,'recursive':True}), {**getattr(self,'proxy_headers',{}),'Content-Type':'application/connect+json'})
            response = connection.getresponse()
            def event():
                prefix = response.read(5)
                self.assertEqual(len(prefix), 5)
                self.assertEqual(prefix[0], 0)
                return json.loads(response.read(int.from_bytes(prefix[1:],'big')))
            self.assertEqual(event(), {'start':{}})
            self.command('mkdir ' + root + '/child')
            self.assertEqual(event(), {'filesystem':{'name':'child','type':'EVENT_TYPE_CREATE'}})
            self.command('chmod 750 ' + root + '/child')
            # Parent and child watches each report ATTRIB; both identify the
            # same directory, without an invented trailing slash.
            for _ in range(2): self.assertEqual(event(), {'filesystem':{'name':'child','type':'EVENT_TYPE_CHMOD'}})
            self.command('chmod 750 ' + root)
            self.assertEqual(event(), {'filesystem':{'name':'.','type':'EVENT_TYPE_CHMOD'}})
            self.assertEqual(self.request('GET', '/health')[0], 204)
        finally:
            connection.close()
            self.command('rm -rf ' + root)

    def test_recursive_watch_root_symlink_matches_walkdir(self):
        root = '/tmp/watch-link-' + secrets.token_hex(8)
        self.addCleanup(lambda: self.command('rm -rf ' + root))
        self.command('mkdir -p ' + root + '/real/sub; ln -s real ' + root + '/alias')
        for suffix in ['/alias', '/alias/.', '/alias/sub']:
            path = root + suffix
            code, _, body = self.request('POST', '/filesystem.Filesystem/CreateWatcher', json.dumps({'path': path, 'recursive': True}), {'Content-Type': 'application/json'})
            if suffix in ['/alias', '/alias/.']:
                self.assertEqual(code, 500, body)
                self.assertEqual(json.loads(body), {'code': 'internal', 'message': 'error adding path ' + path + ' to watcher: fsnotify: not a directory: "' + root + '/alias"'})
            else:
                self.assertEqual(code, 200, body)
                watcher = json.loads(body)['watcherId']
                self.assertEqual(self.request('POST', '/filesystem.Filesystem/RemoveWatcher', json.dumps({'watcherId': watcher}), {'Content-Type': 'application/json'})[0], 200)

    def test_watch_root_errors_report_path(self):
        root = '/tmp/watch-missing-' + secrets.token_hex(8)
        for path, status, message in [(root, 404, 'path ' + root + ' not found: stat ' + root + ': no such file or directory'),
                                       ('/etc/passwd', 400, 'path /etc/passwd not a directory: %!w(<nil>)')]:
            code, _, body = self.request('POST', '/filesystem.Filesystem/CreateWatcher', json.dumps({'path': path}), {'Content-Type': 'application/json'})
            self.assertEqual(code, status, body)
            self.assertEqual(json.loads(body)['message'], message)

    def test_unknown_watcher_errors_include_id(self):
        for method in ['GetWatcherEvents', 'RemoveWatcher']:
            for watcher in ['', 'missing', 'unknown watcher']:
                code, _, body = self.request('POST', '/filesystem.Filesystem/' + method, json.dumps({'watcherId': watcher}), {'Content-Type': 'application/json'})
                self.assertEqual(code, 404, body)
                self.assertEqual(json.loads(body), {'code': 'not_found', 'message': 'watcher with id ' + watcher + ' not found'})

    def test_deleted_child_self_event_is_not_replayed_after_parent_remove(self):
        import signal
        self.assertTrue(hasattr(self, 'daemon_pid'), 'requires an isolated local daemon')
        with tempfile.TemporaryDirectory(prefix='envd-watch-delete-order-') as directory:
            root = Path(directory)
            url = urlsplit(os.environ['ENVD_TEST_URL'])
            connection = http.client.HTTPConnection(url.hostname, url.port, timeout=10)
            try:
                connection.request('POST', '/filesystem.Filesystem/WatchDir', self.frame({'path':directory,'recursive':True}), {'Content-Type':'application/connect+json'})
                response = connection.getresponse()
                def event():
                    prefix = response.read(5)
                    self.assertEqual(len(prefix), 5)
                    self.assertEqual(prefix[0], 0)
                    return json.loads(response.read(int.from_bytes(prefix[1:],'big')))
                self.assertEqual(event(), {'start':{}})
                child = root / 'child'; child.mkdir()
                self.assertEqual(event(), {'filesystem':{'name':'child','type':'EVENT_TYPE_CREATE'}})
                descriptor = os.open(child, os.O_PATH)
                os.kill(self.daemon_pid, signal.SIGSTOP)
                try:
                    deadline = time.monotonic() + 5
                    while 'T' not in Path('/proc/%d/status' % self.daemon_pid).read_text().split('State:')[1].splitlines()[0]:
                        self.assertLess(time.monotonic(), deadline)
                        time.sleep(.01)
                    child.rmdir()
                    # The held inode delays DELETE_SELF until after its parent's
                    # DELETE. Resume only after both are queued in the kernel.
                    os.close(descriptor); descriptor = None
                finally:
                    if descriptor is not None: os.close(descriptor)
                    os.kill(self.daemon_pid, signal.SIGCONT)
                self.assertEqual(event(), {'filesystem':{'name':'child','type':'EVENT_TYPE_REMOVE'}})
                (root / 'after').touch()
                self.assertEqual(event(), {'filesystem':{'name':'after','type':'EVENT_TYPE_CREATE'}})
                self.assertEqual(self.request('GET', '/health')[0], 204)
            finally: connection.close()

    def test_rust_dynamic_directory_gone_reports_error_without_upstream_deadlock(self):
        import signal
        self.assertTrue(hasattr(self, 'daemon_pid'), 'requires an isolated local daemon')
        with tempfile.TemporaryDirectory(prefix='envd-watch-gone-') as directory:
            url = urlsplit(os.environ['ENVD_TEST_URL'])
            connection = http.client.HTTPConnection(url.hostname, url.port, timeout=10)
            self.addCleanup(connection.close)
            connection.request('POST', '/filesystem.Filesystem/WatchDir', self.frame({'path': directory, 'recursive': True}), {'Content-Type':'application/connect+json'})
            response = connection.getresponse()
            prefix = response.read(5)
            self.assertEqual(json.loads(response.read(int.from_bytes(prefix[1:],'big'))), {'start':{}})
            os.kill(self.daemon_pid, signal.SIGSTOP)
            try:
                deadline = time.monotonic() + 5
                while 'T' not in Path('/proc/%d/status' % self.daemon_pid).read_text().split('State:')[1].splitlines()[0]:
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.01)
                # Queue both kernel events before the worker can register the child.
                child = Path(directory) / 'gone'; child.mkdir(); child.rmdir()
            finally: os.kill(self.daemon_pid, signal.SIGCONT)
            prefix = response.read(5)
            self.assertEqual(prefix[0], 2)
            self.assertEqual(json.loads(response.read(int.from_bytes(prefix[1:],'big'))), {'error':{'code':'internal','message':'watcher error: no such file or directory'}})
            self.assertEqual(response.read(), b'')
            self.assertEqual(self.request('GET', '/health')[0], 204)

    def test_rust_remove_preserves_bound_root_aliases(self):
        # Rust safety regression only; not an oracle parity case. A nonrecursive
        # read-only bind has no writable child mounts, even with a broken guard.
        with tempfile.TemporaryDirectory(prefix='envd-root-alias-') as directory:
            mount = Path(directory) / 'root'; mount.mkdir()
            subprocess.run(['mount', '--bind', '/', str(mount)], check=True)
            try:
                subprocess.run(['mount', '-o', 'remount,bind,ro', str(mount)], check=True)
                self.assertTrue(os.statvfs(mount).f_flag & os.ST_RDONLY)
                with open('/proc/self/mountinfo') as mounts:
                    nested = [line.split()[4] for line in mounts if line.split()[4] == str(mount) or line.split()[4].startswith(str(mount) + '/')]
                self.assertEqual(nested, [str(mount)], 'root alias must have no child mounts')
                for path in [str(mount), directory]:
                    code, _, body = self.request('POST', '/filesystem.Filesystem/Remove', json.dumps({'path': path}), {'Content-Type': 'application/json'})
                    self.assertEqual(code, 400, body)
                    self.assertEqual(json.loads(body), {'code': 'invalid_argument', 'message': 'root mutation is forbidden'})
            finally:
                subprocess.run(['umount', str(mount)], check=True)

    def test_remove_crosses_bind_mount_and_reports_partial_failure(self):
        for remove_mount_itself in [False, True]:
            with tempfile.TemporaryDirectory(prefix='envd-remove-mount-') as directory:
                root = Path(directory)
                source = root / 'source'; source.mkdir()
                parent = root / 'parent'; parent.mkdir()
                mount = parent / 'mounted'; mount.mkdir()
                (source / 'nested').mkdir()
                (source / 'nested' / 'marker').write_bytes(b'delete through mount')
                outside = root / 'outside'; outside.write_bytes(b'keep symlink target')
                (source / 'link').symlink_to(outside)
                subprocess.run(['mount', '--bind', str(source), str(mount)], check=True)
                try:
                    sibling = parent / 'zz-after-mount'; sibling.write_bytes(b'delete sibling too')
                    path = mount if remove_mount_itself else parent
                    code, _, body = self.request('POST', '/filesystem.Filesystem/Remove', json.dumps({'path': str(path)}), {'Content-Type': 'application/json'})
                    self.assertEqual(code, 500, body)
                    self.assertEqual(json.loads(body), {'code': 'internal', 'message': 'error removing file or directory: unlinkat ' + str(mount) + ': device or resource busy'})
                    for deleted in [source / 'nested', source / 'link'] + ([] if remove_mount_itself else [sibling]):
                        self.assertEqual(self.request('POST', '/filesystem.Filesystem/Stat', json.dumps({'path': str(deleted)}), {'Content-Type': 'application/json'})[0], 404, str(deleted))
                    self.assertEqual(outside.read_bytes(), b'keep symlink target')
                    self.assertEqual(self.request('POST', '/filesystem.Filesystem/Stat', json.dumps({'path': str(mount)}), {'Content-Type': 'application/json'})[0], 200)
                finally:
                    subprocess.run(['umount', str(mount)], check=True)

if __name__ == '__main__':
    unittest.main()
