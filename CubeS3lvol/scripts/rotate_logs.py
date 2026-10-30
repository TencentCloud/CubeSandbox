#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Bound active S3lvol logs without restarting the storage process."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile


def positive(value):
    result = int(value)
    if result <= 0:
        raise argparse.ArgumentTypeError('value must be positive')
    return result


def configuration(log, archive, size, retain):
    for path in (log, archive):
        if any(ord(c) < 32 for c in str(path)):
            raise ValueError('control characters in log path')
    # olddir uses a single unquoted directory token on deployed logrotate
    # versions; quotes become part of the directory name there.
    if any(c.isspace() or c in '\"\'\\#{}*?[]' for c in str(archive)):
        raise ValueError('archive directory contains unsupported characters')
    if any(c in '*?[]{}' for c in str(log)):
        raise ValueError('log path must not contain glob characters')
    return '''%s {
    size %d
    rotate %d
    missingok
    notifempty
    compress
    delaycompress
    copytruncate
    su root root
    olddir %s
}
''' % (json.dumps(str(log), ensure_ascii=False), size, retain, str(archive))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    default_log = os.environ.get('RCOW_LOG', os.path.join(
        os.environ.get('RCOW_LOG_DIR', '/data/log/rcow'), 's3lvol_tgt.log'))
    parser.add_argument('--log', default=default_log)
    parser.add_argument('--archive-dir', default=os.environ.get('RCOW_LOG_ARCHIVE_DIR'))
    parser.add_argument('--state-dir', default='/var/lib/cube-sandbox-logrotate')
    parser.add_argument('--size', type=positive, default=256 * 2**20)
    parser.add_argument('--retain', type=positive, default=8)
    parser.add_argument('--bootstrap-limit', type=positive, default=2**30)
    parser.add_argument('--reserve-bytes', type=positive, default=8 * 2**30)
    parser.add_argument('--dry-run', action='store_true')
    args = parser.parse_args(argv)
    source = Path(args.log)
    if not source.exists():
        return 0
    if source.is_symlink():
        raise RuntimeError('log file itself must not be a symlink')
    source = source.resolve()
    identity = hashlib.sha256(os.fsencode(source)).hexdigest()[:24]
    info = source.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise RuntimeError('log must be a singly linked regular file')
    archive = Path(args.archive_dir) if args.archive_dir else source.parent / 'rotated'
    archive = archive.resolve()
    if archive == source.parent:
        raise RuntimeError('use a separate archive directory to preserve legacy archives')
    archive = archive / identity
    policy = configuration(source, archive, args.size, args.retain)
    state = Path(args.state_dir)
    state_file = state / (identity + '.state')
    if info.st_size > args.bootstrap_limit and not state_file.is_file():
        raise RuntimeError('oversized legacy log: archive and verify before enabling rotation')
    if args.dry_run:
        print(policy, end='')
        return 0
    # Different original paths may resolve to one log. A canonical-path state
    # and lock prevent two timers from rotating the same file concurrently.
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (state / (identity + '.lock')).open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return 0
        archive.mkdir(mode=0o700, parents=True, exist_ok=True)
        if source.stat().st_size >= args.size and not args.dry_run:
            if shutil.disk_usage(archive).free < source.stat().st_size + args.reserve_bytes:
                raise RuntimeError('insufficient archive space; source left untouched')
        executable = shutil.which('logrotate')
        if not executable:
            raise RuntimeError('logrotate is required')
        with tempfile.NamedTemporaryFile(mode='w', dir=state, suffix='.conf') as config:
            config.write(policy)
            config.flush()
            command = [executable, '--state', str(state_file)]
            subprocess.run(command + [config.name], check=True, timeout=300)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
