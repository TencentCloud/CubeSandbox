# SPDX-License-Identifier: Apache-2.0
"""Repository CubeSandbox SDK against a local daemon using its IP override config.

Set PYTHONPATH=sdk/python and ENVD_SMOKE_URL. This exercises the data client;
it does not create a platform sandbox or test proxy routing.
"""
import os
from pathlib import Path
import tempfile
from urllib.parse import urlsplit
from cubesandbox import Sandbox, Config


def main():
    endpoint = urlsplit(os.environ['ENVD_SMOKE_URL'])
    sandbox = Sandbox({'sandboxID': 'filesystem-component', 'templateID': 'local'},
                      Config(proxy_node_ip=endpoint.hostname, proxy_port=endpoint.port))
    files = sandbox.files
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        created = root / 'created'
        moved = root / 'moved'
        assert files.make_dir(str(created))['path'] == str(created)
        assert files.stat(str(created))['type'] == 'FILE_TYPE_DIRECTORY'
        assert [entry['name'] for entry in files.list(str(root))] == ['created']
        assert files.rename(str(created), str(moved))['path'] == str(moved)
        assert not files.exists(str(created)) and files.exists(str(moved))
        with files.watch_dir(str(root)) as watcher:
            target = root / 'observed'
            target.write_bytes(b'content')
            for kind in ('EVENT_TYPE_CREATE', 'EVENT_TYPE_WRITE'):
                event = next(watcher)
                assert event['name'] == 'observed' and event['type'] == kind, event
            files.remove(str(target))
            event = next(watcher)
            assert event['name'] == 'observed' and event['type'] == 'EVENT_TYPE_REMOVE', event
        files.remove(str(moved))
        assert files.list(str(root)) == []
    print('repository CubeSandbox SDK stat/list/mkdir/rename/remove and streaming watch passed')

    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        for name, data in [('empty', ''), ('text', '文件读写\n' * 4096),
                           ('bytes', b'byte payload\x00\n' * 8192),
                           ('large', b'x' * (33 * 1024 * 1024))]:
            target = str(root / 'nested' / name)
            files.write(target, data)
            expected = data.decode() if isinstance(data, bytes) else data
            assert files.read(target) == expected
            assert (root / 'nested' / name).read_bytes() == expected.encode()
        files.write(str(root / 'nested' / 'text'), 'replacement')
        assert files.read(str(root / 'nested' / 'text')) == 'replacement'
    print('repository CubeSandbox SDK empty/text/bytes/33 MiB write/read and overwrite passed')


if __name__ == '__main__':
    main()
