# SPDX-License-Identifier: Apache-2.0
"""Unary SDK acceptance against an isolated development daemon.

ENVD_SMOKE_URL selects the daemon. CUBE_FILESYSTEM_SMOKE_DIR must be mounted at
the same path inside its container. CUBE_FILESYSTEM_SMOKE_USER and
CUBE_FILESYSTEM_SMOKE_HOME identify a guest user whose writable home is inside
that mount. This changes init defaults; use a disposable daemon.
"""
import base64
import importlib.metadata
import inspect
import json
import os
from pathlib import Path
import tempfile
from urllib.request import Request

import e2b_connect
import httpx
from httpcore import ConnectionPool
from packaging.version import Version
from e2b.connection_config import ConnectionConfig
from e2b.envd.filesystem import filesystem_connect, filesystem_pb2 as pb
from e2b.exceptions import FileNotFoundException, InvalidArgumentException, SandboxException
from e2b.sandbox_sync.filesystem.filesystem import Filesystem

from support.sdk import init
from support.connect import BASE, HTTP

def rejected(action, kind):
    try:
        action()
    except kind:
        return
    raise AssertionError(f'expected {kind.__name__}')


def rpc_error(action, code):
    try:
        action()
    except e2b_connect.ConnectException as exc:
        assert exc.status == code, exc
        return
    raise AssertionError(f'expected RPC {code}')


def scenario(files, rpc, root):
    user = os.environ['CUBE_FILESYSTEM_SMOKE_USER']
    home = Path(os.environ['CUBE_FILESYSTEM_SMOKE_HOME'])
    defaults = root / 'defaults'
    defaults.mkdir()
    init(user, str(defaults))
    assert files.get_info('').path == str(defaults)
    assert files.get_info('~').path == str(home)
    assert files.get_info('~', user='root').path == '/root'
    assert files.list('') == []
    # A nonempty relative path anchors home, even with defaultWorkdir set.
    relative = os.path.relpath(root, home)
    assert files.get_info(relative).path == str(root)
    current = files.get_info(relative + '/.')
    assert current.type.value == 'dir' and os.path.samefile(current.path, root), current
    parent = files.get_info(relative + '/defaults/..')
    assert parent.type.value == 'dir' and os.path.samefile(parent.path, root), parent

    directory = root / 'created' / 'child'
    assert files.make_dir(str(directory))
    assert not files.make_dir(str(directory))
    assert not files.make_dir('/')
    for path in (directory.parent, directory):
        entry = files.get_info(str(path))
        assert entry.mode == 0o755 and entry.owner == user, entry

    target = root / 'file'
    target.write_text('fixture')
    target.chmod(0o640)
    rejected(lambda: files.make_dir(str(target)), InvalidArgumentException)
    link = root / 'link'
    link.symlink_to('file')
    broken = root / 'broken'
    broken.symlink_to('missing')
    linkdir = root / 'linkdir'
    linkdir.symlink_to('created', target_is_directory=True)
    entry = files.get_info(str(link))
    assert entry.type.value == 'file' and entry.mode == 0o640, entry
    assert entry.symlink_target == str(target), entry
    assert entry.size == len('file') and entry.permissions.startswith('L'), entry
    raw = rpc.stat(pb.StatRequest(path=str(broken)), request_timeout=10).entry
    assert raw.type == pb.FILE_TYPE_UNSPECIFIED and raw.mode == 0, raw
    assert raw.HasField('symlink_target'), raw
    listed = files.list(str(root), depth=2)
    names = [entry.path for entry in listed]
    assert names == sorted(names), names
    assert str(link) in names and str(linkdir) in names, names
    assert str(broken) not in names  # SDK intentionally filters UNSPECIFIED.
    assert str(linkdir / 'child') not in names, names
    assert [entry.path for entry in files.list(str(linkdir))] == [str(linkdir / 'child')]
    raw_list = rpc.list_dir(pb.ListDirRequest(path=str(root), depth=0), request_timeout=10)
    assert str(broken) in [entry.path for entry in raw_list.entries]
    assert all(Path(entry.path).parent == root for entry in raw_list.entries)
    rejected(lambda: files.list(str(root), depth=0), InvalidArgumentException)

    destination = root / 'destination' / 'parents' / 'moved'
    moved = files.rename(str(target), str(destination))
    assert moved.path == str(destination) and destination.read_text() == 'fixture'
    assert not files.exists(str(target))
    for path in (destination.parent.parent, destination.parent):
        entry = files.get_info(str(path))
        assert entry.mode == 0o755 and entry.owner == user, entry
    files.remove(str(linkdir) + '/')
    assert not linkdir.is_symlink() and directory.is_dir()
    files.remove(str(destination))
    files.remove(str(destination))
    assert not files.exists(str(destination))
    rejected(lambda: files.get_info(str(destination)), FileNotFoundException)
    assert not files.make_dir('')
    files.remove('')
    assert not defaults.exists()
    assert files.make_dir('')
    assert defaults.is_dir()
    try:
        files.get_info('nul\0path')
    except SandboxException as exc:
        assert str(exc) == 'Code.internal: error getting file info: lstat ' + str(home / 'nul\0path') + ': invalid argument', exc
    else:
        raise AssertionError('NUL path must fail')
    headers = {'Authorization': 'Basic ' + base64.b64encode(b'root:').decode()}
    rpc_error(lambda: rpc.make_dir(pb.MakeDirRequest(path='/'), headers=headers,
                                  request_timeout=10), e2b_connect.Code.already_exists)
    moved_default = root / 'moved-default'
    rpc.move(pb.MoveRequest(source='', destination=str(moved_default)), request_timeout=10)
    assert moved_default.is_dir() and not defaults.exists()
    init(user, str(root))
    print('get_info/exists/list/make_dir/rename/remove, init/user/home/workdir, '
          'symlink and SDK-visible errors passed; depth=0 and broken entries use generated RPC')
    print('No aggregate compatibility or 0.6.2 metadata threshold claim; '
          'content transfer and Watch were not exercised')


def main():
    with ConnectionPool() as pool, httpx.Client(trust_env=False) as api:
        options = {'envd_api_url': BASE, 'envd_version': Version('0.5.7'),
                   'connection_config': ConnectionConfig()}
        parameters = inspect.signature(Filesystem).parameters
        if 'pool' in parameters:
            options['pool'] = pool
        if 'envd_api' in parameters:
            options['envd_api'] = api
        files = Filesystem(**options)
        rpc = filesystem_connect.FilesystemClient(BASE, pool=pool, json=True)
        # Keep the plain-relative case free of '..'; test '..' separately above.
        home = Path(os.environ['CUBE_FILESYSTEM_SMOKE_HOME'])
        assert home.is_relative_to(Path(os.environ['CUBE_FILESYSTEM_SMOKE_DIR']))
        with tempfile.TemporaryDirectory(dir=home) as directory:
            root = Path(directory)
            root.chmod(0o777)
            try:
                scenario(files, rpc, root)
            finally:
                # Guest-owned descendants are removed through the public API.
                files.remove(str(root), user='root')
    print(f'e2b {importlib.metadata.version("e2b")} unary filesystem assertions passed')


if __name__ == '__main__':
    main()
