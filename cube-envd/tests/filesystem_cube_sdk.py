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
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        files = sandbox.files
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


if __name__ == '__main__':
    main()
