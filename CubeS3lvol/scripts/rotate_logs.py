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


def rotation_reserve(source_device, archive_device, override):
    if override is not None:
        return override
    return 2**20 if source_device == archive_device else 8 * 2**30


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


def resolve_logrotate():
    override = os.environ.get('ONE_CLICK_LOGROTATE_BIN')
    candidates = [override] if override else [shutil.which('logrotate'), '/usr/sbin/logrotate', '/usr/bin/logrotate']
    for candidate in candidates:
        if candidate and os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    raise RuntimeError('logrotate is required')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    default_log = os.environ.get('RCOW_LOG') or os.path.join(
        os.environ.get('RCOW_LOG_DIR') or '/data/log/rcow', 's3lvol_tgt.log')
    parser.add_argument('--log', default=default_log)
    parser.add_argument('--archive-dir', default=os.environ.get('RCOW_LOG_ARCHIVE_DIR'))
    parser.add_argument('--state-dir', default='/var/lib/cube-sandbox-logrotate')
    parser.add_argument('--size', type=positive, default=256 * 2**20)
    parser.add_argument('--retain', type=positive, default=8)
    parser.add_argument('--bootstrap-limit', type=positive, default=2**30)
    parser.add_argument('--reserve-bytes', type=positive, default=os.environ.get('RCOW_LOG_RESERVE_BYTES'))
    parser.add_argument('--archive-oversized', action='store_true',
                        help='accept an oversized legacy log after checking archive space')
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
        accepted = state / (identity + '.accepted')
        if source.stat().st_size > args.bootstrap_limit and not args.archive_oversized and not (state_file.is_file() or accepted.is_file()):
            raise RuntimeError('oversized legacy log: pause the rotation timer and run this helper with --archive-oversized; verify the archive before restarting the timer')
        # Acceptance survives a failed first rotation; it is separate from
        # logrotate's state format and never created by dry-run.
        accepted.touch(mode=0o600, exist_ok=True)
        archive.mkdir(mode=0o700, parents=True, exist_ok=True)
        reserve = rotation_reserve(source.stat().st_dev, archive.stat().st_dev, args.reserve_bytes)
        if source.stat().st_size >= args.size:
            if shutil.disk_usage(archive).free < source.stat().st_size + reserve:
                raise RuntimeError('insufficient archive space; source left untouched')
        executable = resolve_logrotate()
        with tempfile.NamedTemporaryFile(mode='w', dir=state, suffix='.conf') as config:
            config.write(policy)
            config.flush()
            command = [executable, '--state', str(state_file)]
            subprocess.run(command + [config.name], check=True, timeout=300)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
