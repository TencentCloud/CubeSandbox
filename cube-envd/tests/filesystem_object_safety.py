# SPDX-License-Identifier: Apache-2.0
"""Public unary RPC object safety in a disposable privileged mount namespace.

Set ENVD_SMOKE_URL and optionally ENVD_SMOKE_PID to record the daemon's real
umask. Run with different daemon launch umasks (e.g. 000 and 077). Requires
root, CAP_SYS_ADMIN, /dev/shm on a separate filesystem, and a nobody account.
The fixtures and daemon must see the same paths and mounts.
"""
import base64
from contextlib import contextmanager
import ctypes
import json
import os
from pathlib import Path
import pwd
import stat
import struct
import tempfile
from urllib.error import HTTPError
from urllib.request import Request

from support.connect import BASE, HTTP, request


def rpc(method, value, user='root'):
    headers = {'Content-Type': 'application/json', 'Connect-Protocol-Version': '1',
               'Authorization': 'Basic ' + base64.b64encode(f'{user}:'.encode()).decode()}
    req = Request(BASE + '/filesystem.Filesystem/' + method,
                  json.dumps(value).encode(), headers)
    try:
        with HTTP.open(req, timeout=10) as response:
            body = response.read()
            return response.status, json.loads(body) if body else {}
    except HTTPError as error:
        return error.code, json.loads(error.read())


def success(method, value, user='root'):
    status, body = rpc(method, value, user)
    assert status == 200, (method, status, body)
    return body


def rejected(method, value, code):
    status, body = rpc(method, value)
    assert status != 200 and body.get('code') == code, (method, status, body)


@contextmanager
def bind(source, destination):
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.mount(os.fsencode(source), os.fsencode(destination), None, 4096, None):
        raise OSError(ctypes.get_errno(), 'bind mount')
    try:
        # Same device and inode are deliberately insufficient to detect the mount.
        assert source.stat().st_dev == destination.stat().st_dev
        assert source.stat().st_ino == destination.stat().st_ino
        yield
    finally:
        if libc.umount2(os.fsencode(destination), 2):
            raise OSError(ctypes.get_errno(), 'unmount private bind fixture')


def list_mount(root):
    source, tree = root / 'source', root / 'tree'
    source.mkdir()
    tree.mkdir()
    (source / 'sentinel').write_text('visible')
    mounted = tree / 'm'
    mounted.mkdir()
    with bind(source, mounted):
        entries = success('ListDir', {'path': str(tree), 'depth': 2})['entries']
        assert [entry['path'] for entry in entries] == [str(mounted), str(mounted / 'sentinel')]
        assert (source / 'sentinel').read_text() == 'visible'


def symlink_remove(root):
    target = root / 'target'
    target.mkdir()
    (target / 'sentinel').write_text('preserve')
    for suffix in ('', '/', '///'):
        link = root / 'link'
        link.symlink_to(target, target_is_directory=True)
        success('Remove', {'path': str(link) + suffix})
        assert not link.is_symlink()
        assert (target / 'sentinel').read_text() == 'preserve'
    success('Remove', {'path': str(root / 'absent')})


def move_errors(root):
    source = root / 'source'
    source.write_text('source')
    with tempfile.TemporaryDirectory(prefix='envd-exdev-', dir='/dev/shm') as directory:
        destination = Path(directory) / 'destination'
        assert source.stat().st_dev != destination.parent.stat().st_dev, 'EXDEV needs real devices'
        rejected('Move', {'source': str(source), 'destination': str(destination)}, 'internal')
        assert source.read_text() == 'source' and not destination.exists()
    destination = root / 'directory'
    destination.mkdir()
    (destination / 'sentinel').write_text('preserve')
    rejected('Move', {'source': str(source), 'destination': str(destination)}, 'internal')
    assert source.read_text() == 'source'
    assert (destination / 'sentinel').read_text() == 'preserve'


def directory_identity(root):
    selected = pwd.getpwnam('nobody')
    root.chmod(0o777)
    existing = root / 'existing'
    existing.mkdir(mode=0o711)
    existing.chmod(0o711)
    before = existing.stat()
    created = existing / 'created' / 'child'
    success('MakeDir', {'path': str(created)}, selected.pw_name)
    for path in (created.parent, created):
        info = path.stat()
        assert (stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid) == (
            0o755, selected.pw_uid, selected.pw_gid), (path, info)
    after = existing.stat()
    assert (after.st_mode, after.st_uid, after.st_gid) == (before.st_mode, before.st_uid, before.st_gid)
    rejected('MakeDir', {'path': str(created)}, 'already_exists')
    source = root / 'source'
    source.write_text('move')
    destination = existing / 'move-parents' / 'child' / 'moved'
    success('Move', {'source': str(source), 'destination': str(destination)}, selected.pw_name)
    assert destination.read_text() == 'move' and not source.exists()
    for path in (destination.parent.parent, destination.parent):
        info = path.stat()
        assert (stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid) == (
            0o755, selected.pw_uid, selected.pw_gid), (path, info)
    after = existing.stat()
    assert (after.st_mode, after.st_uid, after.st_gid) == (before.st_mode, before.st_uid, before.st_gid)


def inherited_attributes(root):
    for kind in ('setgid', 'default_acl'):
        parent = root / kind
        parent.mkdir()
        parent.chmod(0o2777 if kind == 'setgid' else 0o777)
        if kind == 'default_acl':
            acl = struct.pack('<I', 2) + b''.join(
                struct.pack('<HHI', tag, permissions, 0xffffffff)
                for tag, permissions in ((1, 7), (4, 0), (32, 0)))
            os.setxattr(parent, 'system.posix_acl_default', acl)
        before = parent.stat()
        rejected('MakeDir', {'path': str(parent / 'created' / 'child')},
                 'failed_precondition')
        expected = 0o2755 if kind == 'setgid' else 0o700
        assert stat.S_IMODE((parent / 'created').stat().st_mode) == expected
        assert not (parent / 'created' / 'child').exists()
        source = root / f'{kind}-source'
        source.write_text('preserved')
        rejected('Move', {'source': str(source),
                          'destination': str(parent / 'move-parent' / 'moved')},
                 'failed_precondition')
        assert source.read_text() == 'preserved'
        assert stat.S_IMODE((parent / 'move-parent').stat().st_mode) == expected
        after = parent.stat()
        assert (after.st_mode, after.st_uid, after.st_gid) == (
            before.st_mode, before.st_uid, before.st_gid)
        if kind == 'default_acl':
            assert os.getxattr(parent, 'system.posix_acl_default') == acl


def main():
    pid = os.environ.get('ENVD_SMOKE_PID')
    if pid:
        print(next(line for line in Path(f'/proc/{pid}/status').read_text().splitlines()
                   if line.startswith('Umask:')), flush=True)
    failures = []
    with tempfile.TemporaryDirectory(prefix='envd-object-') as directory:
        root = Path(directory)
        root.chmod(0o755)
        request('/init', {'defaultUser': 'root', 'defaultWorkdir': str(root)})
        for scenario in (list_mount, symlink_remove,
                         move_errors, directory_identity, inherited_attributes):
            case = root / scenario.__name__
            case.mkdir()
            try:
                scenario(case)
                print(f'PASS {scenario.__name__}', flush=True)
            except Exception as error:
                failures.append((scenario.__name__, repr(error)))
                print(f'FAIL {scenario.__name__}: {error!r}', flush=True)
    assert not failures, failures


if __name__ == '__main__':
    main()
