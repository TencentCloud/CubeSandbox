# SPDX-License-Identifier: Apache-2.0
"""Permanent blocking SDK upload format / 0.5.7 threshold acceptance.

Runs against the isolated development image using the shared-home fixture.
"""
import importlib.metadata
import inspect
import io
import os
from pathlib import Path
import tempfile

import httpx
from httpcore import ConnectionPool
from packaging.version import Version
from e2b.connection_config import ConnectionConfig
from e2b.sandbox_sync.filesystem.filesystem import Filesystem

from support.sdk import init
from support.connect import BASE


def main():
    with ConnectionPool() as pool, httpx.Client(base_url=BASE, trust_env=False) as api:
        home = Path(os.environ['CUBE_FILESYSTEM_SMOKE_HOME'])
        user = os.environ['CUBE_FILESYSTEM_SMOKE_USER']
        options = dict(envd_api_url=BASE, envd_version=Version('0.5.7'),
                       connection_config=ConnectionConfig())
        parameters = inspect.signature(Filesystem).parameters
        if 'pool' in parameters:
            options['pool'] = pool
        if 'envd_api' in parameters:
            options['envd_api'] = api
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
    print(f'e2b {importlib.metadata.version("e2b")} octet/gzip/multipart text/binary/stream, '
          'batch order, GET identity/gzip read-back and 0.5.7 threshold passed')


if __name__ == '__main__':
    main()
