# SPDX-License-Identifier: Apache-2.0
"""Blocking E2B SDK file transfers against an isolated daemon with a shared-home fixture."""

import gzip as gzip_codec
import inspect
import io
import os
from pathlib import Path
import tempfile
import unittest

import httpx
from httpcore import ConnectionPool
from packaging.version import Version
from e2b.connection_config import ConnectionConfig
from e2b.exceptions import FileNotFoundException
from e2b.sandbox_sync.filesystem.filesystem import Filesystem

from support.connect import BASE
from support.sdk import init


def filesystem_options(pool, api, *, accept_encoding=None):
    config = (
        ConnectionConfig(extra_sandbox_headers={'Accept-Encoding': accept_encoding})
        if accept_encoding else ConnectionConfig()
    )
    options = dict(envd_api_url=BASE, envd_version=Version('0.5.7'), connection_config=config)
    parameters = inspect.signature(Filesystem).parameters
    if 'pool' in parameters:
        options['pool'] = pool
    if 'envd_api' in parameters:
        options['envd_api'] = api
    return options


class FileTransferSDK(unittest.TestCase):
    def test_download(self):
        with ConnectionPool() as pool, httpx.Client(
            base_url=BASE, trust_env=False, headers={'Accept-Encoding': 'identity'},
        ) as api:
            options = filesystem_options(pool, api, accept_encoding='identity')
            files = Filesystem(**options)
            home = Path(os.environ['CUBE_FILESYSTEM_SMOKE_HOME'])
            user = os.environ['CUBE_FILESYSTEM_SMOKE_USER']
            with tempfile.TemporaryDirectory(dir=home) as directory:
                root = Path(directory)
                text = '完整文件下载 — identity / gzip\n' * 4096
                binary = bytes(range(256)) * 4096
                (root / 'text.txt').write_text(text)
                (root / 'binary').write_bytes(binary)
                (root / 'alias').symlink_to('binary')
                # The query override must work even with a different init default user.
                init('root', str(root / 'text.txt'))
                for compressed in (False, True):
                    for name, expected, format in (
                        ('text.txt', text, 'text'), ('text.txt', text.encode(), 'bytes'),
                        ('binary', binary, 'bytes'), ('alias', binary, 'bytes'),
                        ('binary', binary, 'stream'),
                    ):
                        relative = str(Path(root.name) / name)
                        result = files.read(relative, format=format, user=user, gzip=compressed)
                        if format == 'stream':
                            try:
                                result_bytes = b''.join(result)
                            finally:
                                if hasattr(result, 'close'):
                                    result.close()
                            result = result_bytes
                        assert result == expected, (compressed, name, format)
                        # Inspect a real wire request separately: newer SDK versions
                        # own their HTTP client and do not expose response headers.
                        with api.stream('GET', '/files', params={'path': str(root / name)},
                                        headers={'Accept-Encoding': 'gzip' if compressed else 'identity'}) as wire:
                            assert wire.status_code == 200
                            headers = wire.headers
                            assert headers.get('content-encoding') == ('gzip' if compressed else None)
                            assert headers['vary'] == 'Accept-Encoding'
                            assert headers['content-type'] == (
                                'text/plain; charset=utf-8' if name.endswith('.txt') else 'application/octet-stream')
                            assert not any(key in headers for key in ('etag', 'content-range'))
                            if compressed:
                                assert 'accept-ranges' not in headers and 'last-modified' not in headers
                            else:
                                assert headers['accept-ranges'] == 'bytes' and 'last-modified' in headers
                            raw = b''.join(wire.iter_raw())
                            expected_bytes = expected.encode() if isinstance(expected, str) else expected
                            if compressed:
                                assert raw.startswith(b'\x1f\x8b')
                                if len(raw) <= 2048:
                                    assert int(headers['content-length']) == len(raw)
                                else:
                                    assert 'content-length' not in headers
                                assert gzip_codec.decompress(raw) == expected_bytes
                            else:
                                assert raw == expected_bytes
                    assert files.read('', gzip=compressed) == text
                try:
                    files.read(str(root / 'missing'))
                except FileNotFoundException:
                    pass
                else:
                    raise AssertionError('missing download did not map to SDK FileNotFoundException')
                # Leave the shared daemon's init state ready for subsequent acceptance.
                init(user, str(home))

    def test_upload(self):
        with ConnectionPool() as pool, httpx.Client(base_url=BASE, trust_env=False) as api:
            home = Path(os.environ['CUBE_FILESYSTEM_SMOKE_HOME'])
            user = os.environ['CUBE_FILESYSTEM_SMOKE_USER']
            options = filesystem_options(pool, api)
            files = Filesystem(**options)
            with tempfile.TemporaryDirectory(dir=home) as directory:
                root = Path(directory)
                init('root', str(home))
                binary = bytes(range(256)) * 4096
                text = '上传完整内容 gzip / octet / multipart\n' * 4096
                for mode in ('octet', 'gzip', 'multipart'):
                    flags = dict(gzip=mode == 'gzip', use_octet_stream=mode != 'multipart')
                    for name, data in (('text', text), ('binary', binary), ('stream', io.BytesIO(binary))):
                        path = str(root / f'{mode}-{name}')
                        result = files.write(path, data, user=user, **flags)
                        assert result.path == path and result.name == Path(path).name
                        expected = text.encode() if name == 'text' else binary
                        for compressed in (False, True):
                            assert files.read(path, format='bytes', user=user, gzip=compressed) == expected
                        info = files.get_info(path, user=user)
                        assert info.path == path
                paths = [str(root / name) for name in ('batch-a', 'batch-b')]
                written = files.write_files([dict(path=paths[0], data=text), dict(path=paths[1], data=binary)],
                                            user=user, use_octet_stream=False)
                assert [entry.path for entry in written] == paths
                assert files.read(paths[0], user=user) == text
                assert files.read(paths[1], format='bytes', user=user) == binary
                # Exercise the SDK's actual feature threshold as well as its enabled path.
                options['envd_version'] = Version('0.5.6')
                older = Filesystem(**options)
                fallback = str(root / 'below-threshold')
                older.write(fallback, binary, user=user, gzip=True, use_octet_stream=True)
                assert files.read(fallback, format='bytes', user=user) == binary
                init(user, str(home))


if __name__ == '__main__':
    unittest.main()
