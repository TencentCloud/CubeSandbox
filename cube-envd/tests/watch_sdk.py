# SPDX-License-Identifier: Apache-2.0
"""Per-SDK high-level polling and independent streaming RPC evidence."""
import importlib.metadata
import inspect
import os
from pathlib import Path
import tempfile
import time
import httpx
from httpcore import ConnectionPool
from packaging.version import Version
from e2b.connection_config import ConnectionConfig
from e2b.envd.filesystem import filesystem_connect, filesystem_pb2 as pb
from e2b.exceptions import SandboxException, TemplateException
from e2b.sandbox_sync.filesystem.filesystem import Filesystem
from support.connect import BASE


def scenario(rpc, root, recursive):
    (root / 'deep' / 'child').mkdir(parents=True)
    stream = rpc.watch_dir(pb.WatchDirRequest(path=str(root), recursive=recursive), request_timeout=10)
    try:
        assert next(stream).WhichOneof('event') == 'start'
        def event(name, kind):
            response = next(stream)
            assert response.WhichOneof('event') == 'filesystem', response
            assert response.filesystem.name == name and response.filesystem.type == kind, response
        path = 'deep/child/file' if recursive else 'file'
        (root / path).write_bytes(b'contents')
        event(path, pb.EVENT_TYPE_CREATE)
        event(path, pb.EVENT_TYPE_WRITE)
        if recursive:
            (root / 'dynamic').mkdir()
            event('dynamic', pb.EVENT_TYPE_CREATE)
            (root / 'dynamic' / 'after-observation').write_bytes(b'covered')
            event('dynamic/after-observation', pb.EVENT_TYPE_CREATE)
            event('dynamic/after-observation', pb.EVENT_TYPE_WRITE)
        (root / path).unlink()
        event(path, pb.EVENT_TYPE_REMOVE)
    finally:
        stream.close()


def main():
    version = importlib.metadata.version('e2b')
    with ConnectionPool() as pool, httpx.Client(trust_env=False) as api:
        options = {'envd_api_url': BASE, 'envd_version': Version('0.1.3'), 'connection_config': ConnectionConfig()}
        parameters = inspect.signature(Filesystem).parameters
        if 'pool' in parameters:
            options['pool'] = pool
        if 'envd_api' in parameters:
            options['envd_api'] = api
        files = Filesystem(**options)
        try:
            files.watch_dir('/unused', recursive=True)
        except TemplateException:
            print(version, 'high-level recursive 0.1.3 rejection passed', flush=True)
        else:
            raise AssertionError('missing recursive gate')
        assert 'create_watcher(' in inspect.getsource(Filesystem.watch_dir)
        at_gate = Filesystem(**dict(options, envd_version=Version('0.1.4')))
        from e2b.sandbox_sync.filesystem.watch_handle import WatchHandle
        assert 'get_watcher_events(' in inspect.getsource(WatchHandle.get_new_events)
        assert 'remove_watcher(' in inspect.getsource(WatchHandle.stop)
        for recursive in (False, True):
            with tempfile.TemporaryDirectory(dir=os.environ['CUBE_FILESYSTEM_SMOKE_DIR']) as directory:
                root = Path(directory)
                (root / 'deep' / 'child').mkdir(parents=True)
                handle = at_gate.watch_dir(str(root), recursive=recursive, request_timeout=10)
                try:
                    assert handle.get_new_events() == []
                    pending = []
                    def event(name, kind):
                        deadline = time.monotonic() + 10
                        while not pending:
                            pending.extend(handle.get_new_events())
                            assert time.monotonic() < deadline, 'SDK polling event deadline'
                        actual = pending.pop(0)
                        assert actual.name == name and actual.type.value == kind, actual
                    path = 'deep/child/file' if recursive else 'file'
                    (root / path).write_bytes(b'content')
                    event(path, 'create')
                    event(path, 'write')
                    if recursive:
                        (root / 'dynamic').mkdir()
                        event('dynamic', 'create')
                        (root / 'dynamic' / 'covered').write_bytes(b'covered')
                        event('dynamic/covered', 'create')
                        event('dynamic/covered', 'write')
                    (root / path).unlink()
                    event(path, 'remove')
                finally:
                    handle.stop()
                try:
                    handle.get_new_events()
                except SandboxException:
                    pass
                else:
                    raise AssertionError('removed polling watcher remained accessible')
        print(version, 'high-level watch_dir / get_new_events / stop normal + recursive passed at 0.1.4', flush=True)
        for json_mode in (False, True):
            rpc = filesystem_connect.FilesystemClient(BASE, pool=pool, json=json_mode)
            for recursive in (False, True):
                with tempfile.TemporaryDirectory(dir=os.environ['CUBE_FILESYSTEM_SMOKE_DIR']) as directory:
                    scenario(rpc, Path(directory), recursive)
            print(version, 'actual /filesystem.Filesystem/WatchDir', 'json' if json_mode else 'protobuf',
                  'normal + recursive post-observed coverage passed', flush=True)
        print(version, '0.1.4 polling and streaming capability evidence; no aggregate or 0.6.3/0.6.4 claim', flush=True)

if __name__ == '__main__':
    main()
