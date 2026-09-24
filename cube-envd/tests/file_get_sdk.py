# SPDX-License-Identifier: Apache-2.0
"""Blocking SDK full-file downloads against an isolated development image.

Uses the same ENVD_SMOKE_URL and guest/shared-home variables as unary acceptance.
Fixtures are real guest-visible files; no upload capability is assumed.
"""
import gzip as gzip_codec
import importlib.metadata
import inspect
import os
from pathlib import Path
import tempfile

import httpx
from httpcore import ConnectionPool
from packaging.version import Version
from e2b.connection_config import ConnectionConfig
from e2b.exceptions import FileNotFoundException
from e2b.sandbox_sync.filesystem.filesystem import Filesystem

from support.sdk import init
from support.connect import BASE


def main():
    with ConnectionPool() as pool, httpx.Client(
        base_url=BASE, trust_env=False, headers={'Accept-Encoding': 'identity'},
    ) as api:
        options = {'envd_api_url': BASE, 'envd_version': Version('0.5.7'),
                   'connection_config': ConnectionConfig(extra_sandbox_headers={'Accept-Encoding': 'identity'})}
        parameters = inspect.signature(Filesystem).parameters
        if 'pool' in parameters:
            options['pool'] = pool
        if 'envd_api' in parameters:
            options['envd_api'] = api
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
    print(f'e2b {importlib.metadata.version("e2b")} identity/gzip text/bytes/stream, '
          'binary, symlink, query user/default path and response headers passed')


if __name__ == '__main__':
    main()
