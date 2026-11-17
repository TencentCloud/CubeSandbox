# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Render the storage log policy and check space before copytruncate."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import stat
import sys


def positive(value):
    result = int(value)
    if result <= 0:
        raise argparse.ArgumentTypeError('value must be positive')
    return result


def regular_log(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise ValueError('log must be a singly linked regular file')
    return info


def check_space(log, archive, reserve=None):
    source = regular_log(log)
    # Keep a floor on both filesystems, with more headroom when the copy
    # competes with the storage backend and cubelet on the source device.
    floor = 2**30 if source.st_dev == archive.stat().st_dev else 512 * 2**20
    reserve = max(floor, reserve or 0)
    # firstaction runs before delaycompress writes the previous archive;
    # prerotate checks again immediately before the active-log copy.
    previous = archive / (log.name + '.1')
    try:
        compression = regular_log(previous).st_size
    except FileNotFoundError:
        compression = 0
    required = source.st_size + compression + reserve
    if shutil.disk_usage(archive).free < required:
        raise ValueError('insufficient archive space; source left untouched '
                         '(need {} free bytes)'.format(required))


def render(template, helper):
    raw_log = Path(os.environ.get('RCOW_LOG') or os.path.join(
        os.environ.get('RCOW_LOG_DIR') or '/data/log/rcow', 's3lvol_tgt.log'))
    if not raw_log.is_absolute():
        raise ValueError('RCOW_LOG must be absolute')
    if raw_log.is_symlink():
        raise ValueError('log file itself must not be a symlink')
    if raw_log.exists():
        regular_log(raw_log)
    log = raw_log.resolve()
    archive = Path(os.environ.get('RCOW_LOG_ARCHIVE_DIR') or log.parent / 'rotated')
    if not archive.is_absolute():
        raise ValueError('RCOW_LOG_ARCHIVE_DIR must be absolute')
    archive = archive.resolve()
    if archive == log.parent:
        raise ValueError('use a separate directory for rotation archives')
    identity = hashlib.sha256(os.fsencode(log)).hexdigest()[:24]
    archive = archive / identity
    for path in (log, archive, helper):
        if any(ord(c) < 32 or ord(c) == 127 for c in str(path)):
            raise ValueError('control characters in path')
    if any(c in '*?[]{}' for c in str(log)):
        raise ValueError('log path must not contain glob characters')
    # olddir is an unquoted token on supported logrotate versions.
    if any(c.isspace() or c in '\"\'\\#{}*?[]' for c in str(archive)):
        raise ValueError('archive directory contains unsupported characters')
    reserve = os.environ.get('RCOW_LOG_RESERVE_BYTES')
    command = ['python3', str(helper), 'check-space', str(log), str(archive)]
    if reserve:
        command += ['--reserve-bytes', str(positive(reserve))]
    policy = template.read_text(encoding='utf-8')
    policy = policy.replace('@LOG@', json.dumps(str(log), ensure_ascii=False))
    policy = policy.replace('@ARCHIVE@', str(archive))
    policy = policy.replace('@SPACE_CHECK@', ' '.join(shlex.quote(x) for x in command))
    # logrotate validates olddir even in --debug mode. No log contents or
    # rotation state are touched when rendering the candidate policy.
    archive.mkdir(mode=0o700, parents=True, exist_ok=True)
    return policy


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    generate = commands.add_parser('render')
    generate.add_argument('template', type=Path)
    check = commands.add_parser('check-space')
    check.add_argument('log', type=Path)
    check.add_argument('archive', type=Path)
    check.add_argument('--reserve-bytes', type=positive)
    args = parser.parse_args()
    try:
        if args.command == 'render':
            print(render(args.template, Path(__file__).resolve()), end='')
        else:
            check_space(args.log, args.archive, args.reserve_bytes)
    except (OSError, ValueError, argparse.ArgumentTypeError) as exc:
        print('s3lvol logrotate: {}'.format(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
