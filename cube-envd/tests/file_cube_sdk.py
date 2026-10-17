# SPDX-License-Identifier: Apache-2.0
"""Repository SDK file read/write against a real daemon using IP override.

Set PYTHONPATH=sdk/python and ENVD_SMOKE_URL. No platform sandbox is created.
"""
import os
from pathlib import Path
import tempfile
from urllib.parse import urlsplit
from cubesandbox import Sandbox, Config


def main():
    endpoint = urlsplit(os.environ['ENVD_SMOKE_URL'])
    sandbox = Sandbox({'sandboxID': 'file-transfer-component', 'templateID': 'local'},
                      Config(proxy_node_ip=endpoint.hostname, proxy_port=endpoint.port))
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        for name, data in [('empty', ''), ('text', '文件读写\n' * 4096),
                           ('bytes', b'byte payload\x00\n' * 8192),
                           ('large', b'x' * (33 * 1024 * 1024))]:
            target = str(root / 'nested' / name)
            sandbox.files.write(target, data)
            expected = data.decode() if isinstance(data, bytes) else data
            assert sandbox.files.read(target) == expected
            assert (root / 'nested' / name).read_bytes() == expected.encode()
        sandbox.files.write(str(root / 'nested' / 'text'), 'replacement')
        assert sandbox.files.read(str(root / 'nested' / 'text')) == 'replacement'
    print('repository CubeSandbox SDK empty/text/bytes/33 MiB write/read and overwrite passed')


if __name__ == '__main__':
    main()
