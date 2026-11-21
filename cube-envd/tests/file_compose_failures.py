# SPDX-License-Identifier: Apache-2.0
"""Compose failures and races in a disposable mount namespace with /dev/fuse."""
import concurrent.futures
import errno
import http.client
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import unittest
from urllib.parse import urlencode, urlsplit
from support.daemon import DaemonTestCase
from support.download import DownloadFilesystem


class ComposeFailures(DaemonTestCase):
    def test_compose_read_error_preserves_destination_and_removes_temporary(self):
        with tempfile.TemporaryDirectory(prefix='envd-compose-fault-') as directory:
            root = Path(directory)
            mount = root / 'source'; mount.mkdir()
            target = str(root / 'target')
            self.assertEqual(self.request('POST', '/files?' + urlencode({'path': target}), b'original', {'Content-Type': 'application/octet-stream'})[0], 200)
            filesystem = DownloadFilesystem(mount)
            try:
                with concurrent.futures.ThreadPoolExecutor(1) as pool:
                    pending = pool.submit(self.request, 'POST', '/files/compose', json.dumps({'source_paths': [str(mount / 'file')], 'destination': target}))
                    unique, _ = filesystem.until_read()
                    filesystem.reply(unique, error=errno.EIO)
                    code, _, body = pending.result(timeout=10)
                    self.assertEqual(code, 500, body)
                    source = str(mount / 'file')
                    self.assertEqual(json.loads(body), {'code':500,'message':'error composing source ' + source + ': read ' + source + ': input/output error'})
                self.assertEqual(self.request('GET', '/files?' + urlencode({'path': target}))[2], b'original')
                self.assertEqual(self.command('find ' + directory + " -maxdepth 1 -name '*.e2b-compose.*.tmp' -print"), b'')
            finally:
                filesystem.close()

    def test_compose_enospc_preserves_destination_and_sources(self):
        with tempfile.TemporaryDirectory(prefix='envd-compose-space-') as directory:
            root = Path(directory)
            mount = root / 'destination'; mount.mkdir()
            subprocess.run(['mount', '-t', 'tmpfs', '-o', 'size=1m', 'tmpfs', str(mount)], check=True)
            try:
                source = str(root / 'source'); target = str(mount / 'target')
                self.command('dd if=/dev/zero of=' + source + ' bs=1M count=2 2>/dev/null; printf original > ' + target)
                code, _, body = self.request('POST', '/files/compose', json.dumps({'source_paths': [source], 'destination': target}))
                self.assertEqual(code, 507, body)
                self.assertEqual(json.loads(body)['message'], 'not enough disk space available')
                self.assertEqual(self.request('GET', '/files?' + urlencode({'path': target}))[2], b'original')
                self.command('test -f ' + source)
                self.assertEqual(self.command('find ' + str(mount) + " -name '*.e2b-compose.*.tmp' -print"), b'')
            finally:
                subprocess.run(['umount', str(mount)], check=True)

    def test_compose_disconnect_finishes_accepted_copy_and_cleans_sources(self):
        with tempfile.TemporaryDirectory(prefix='envd-compose-cancel-') as directory:
            root = Path(directory)
            mount = root / 'mount'; mount.mkdir()
            source = root / 'source'; source.write_bytes(b'tail')
            target = root / 'target'; target.write_bytes(b'original')
            filesystem = DownloadFilesystem(mount)
            url = urlsplit(os.environ['ENVD_TEST_URL'])
            connection = http.client.HTTPConnection(url.hostname, url.port, timeout=10)
            try:
                connection.request('POST', '/files/compose', json.dumps({'source_paths': [str(mount / 'file'), str(source)], 'destination': str(target)}), {'Content-Type': 'application/json'})
                unique, (offset, size) = filesystem.until_read()
                connection.close()
                self.assertEqual(self.request('GET', '/health')[0], 204)
                self.assertEqual(target.read_bytes(), b'original')
                data = b'accepted'*8192
                filesystem.content = data
                filesystem.reply(unique, data[offset:offset+size])
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline and (source.exists() or target.read_bytes() == b'original'):
                    time.sleep(.01)
                self.assertEqual(target.read_bytes(), data + b'tail')
                self.assertFalse(source.exists())
                self.assertEqual(list(root.glob('*.e2b-compose.*.tmp')), [])
                self.assertEqual(self.request('GET', '/health')[0], 204)
            finally:
                connection.close(); filesystem.close()

    def test_compose_target_replacement_and_publication_failure_cleanup(self):
        for replacement in ['symlink', 'directory']:
            with self.subTest(replacement=replacement), tempfile.TemporaryDirectory(prefix='envd-compose-publish-') as directory:
                root = Path(directory)
                mount = root / 'mount'; mount.mkdir()
                source = root / 'source'; source.write_bytes(b'tail')
                target = root / 'target'; target.write_bytes(b'original')
                outside = root / 'outside'; outside.write_bytes(b'keep')
                filesystem = DownloadFilesystem(mount)
                try:
                    with concurrent.futures.ThreadPoolExecutor(1) as pool:
                        pending = pool.submit(self.request, 'POST', '/files/compose', json.dumps({'source_paths': [str(mount / 'file'), str(source)], 'destination': str(target)}))
                        unique, (offset, size) = filesystem.until_read()
                        target.unlink()
                        if replacement == 'symlink': target.symlink_to(outside)
                        else:
                            target.mkdir(); (target / 'keep').write_bytes(b'keep directory')
                        data = b'A'*65536
                        filesystem.content = data; filesystem.reply(unique, data[offset:offset+size])
                        code, _, body = pending.result(timeout=10)
                    if replacement == 'symlink':
                        self.assertEqual(code, 200, body)
                        self.assertFalse(target.is_symlink())
                        self.assertEqual(target.read_bytes(), data + b'tail')
                        self.assertFalse(source.exists())
                    else:
                        self.assertEqual(code, 500, body)
                        error = json.loads(body)
                        self.assertEqual(error['code'], 500)
                        self.assertRegex(error['message'], '^error finalizing compose: rename ' + re.escape(str(target)) + r'\.e2b-compose\.[^/ ]+\.tmp ' + re.escape(str(target)) + ': file exists$')
                        self.assertEqual((target / 'keep').read_bytes(), b'keep directory')
                        self.assertEqual(source.read_bytes(), b'tail')
                    self.assertEqual(outside.read_bytes(), b'keep')
                    self.assertEqual(list(root.glob('*.e2b-compose.*.tmp')), [])
                finally: filesystem.close()

    def test_rust_compose_rejects_replaced_unread_sources(self):
        for replacement in ['file', 'fifo']:
            with self.subTest(replacement=replacement), tempfile.TemporaryDirectory(prefix='envd-compose-source-bind-') as directory:
                root = Path(directory)
                mount = root / 'mount'; mount.mkdir()
                source = root / 'source'; source.write_bytes(b'original source')
                target = root / 'target'; target.write_bytes(b'original target')
                filesystem = DownloadFilesystem(mount)
                try:
                    with concurrent.futures.ThreadPoolExecutor(1) as pool:
                        pending = pool.submit(self.request, 'POST', '/files/compose', json.dumps({'source_paths': [str(mount / 'file'), str(source)], 'destination': str(target)}))
                        unique, (offset, size) = filesystem.until_read()
                        source.rename(root / 'original-source')
                        if replacement == 'file': source.write_bytes(b'keep replacement')
                        else: os.mkfifo(source)
                        data = b'A'*65536
                        filesystem.content = data; filesystem.reply(unique, data[offset:offset+size])
                        code, _, body = pending.result(timeout=10)
                    self.assertEqual(code, 409, body)
                    self.assertEqual(target.read_bytes(), b'original target')
                    self.assertTrue(source.exists())
                    if replacement == 'file': self.assertEqual(source.read_bytes(), b'keep replacement')
                    self.assertEqual((root / 'original-source').read_bytes(), b'original source')
                    self.assertEqual(list(root.glob('*.e2b-compose.*.tmp')), [])
                finally: filesystem.close()

    def test_rust_compose_parent_bindings_preserve_replacement_trees(self):
        for replaced in ['source', 'destination']:
            with self.subTest(replaced=replaced), tempfile.TemporaryDirectory(prefix='envd-compose-parent-bind-') as directory:
                root = Path(directory)
                mount = root / 'mount'; mount.mkdir()
                source_parent = root / 'sources'; source_parent.mkdir()
                source = source_parent / 'source'; source.write_bytes(b'first')
                destination_parent = root / 'destination'; destination_parent.mkdir()
                target = destination_parent / 'target'; target.write_bytes(b'old')
                filesystem = DownloadFilesystem(mount)
                try:
                    with concurrent.futures.ThreadPoolExecutor(1) as pool:
                        pending = pool.submit(self.request, 'POST', '/files/compose', json.dumps({'source_paths': [str(source), str(mount / 'file')], 'destination': str(target)}))
                        unique, (offset, size) = filesystem.until_read()
                        parent = source_parent if replaced == 'source' else destination_parent
                        parent.rename(root / 'bound'); parent.mkdir()
                        replacement = source if replaced == 'source' else target
                        replacement.write_bytes(b'keep replacement')
                        data = b'A'*65536
                        filesystem.content = data; filesystem.reply(unique, data[offset:offset+size])
                        code, _, body = pending.result(timeout=10)
                    self.assertEqual(code, 200, body)
                    self.assertEqual(replacement.read_bytes(), b'keep replacement')
                    result = target if replaced == 'source' else root / 'bound' / 'target'
                    self.assertEqual(result.read_bytes(), b'first' + data)
                    self.assertEqual(list(destination_parent.glob('*.e2b-compose.*.tmp')) + list((root / 'bound').glob('*.e2b-compose.*.tmp')), [])
                finally: filesystem.close()

if __name__ == '__main__':
    unittest.main()
