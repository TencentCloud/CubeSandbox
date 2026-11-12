#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Apply opt-in retention after rcow_start moves the previous run's log."""
import argparse
from pathlib import Path
import re
import stat
import sys


def positive(value):
    result = int(value)
    if result <= 0:
        raise argparse.ArgumentTypeError('retention must be positive')
    return result


def prune_restart_archives(source, retain):
    pattern = re.compile(re.escape(source.name) + r'\.[0-9]{8}-[0-9]{6}~*$')
    candidates = []
    for path in source.parent.iterdir():
        if not pattern.fullmatch(path.name):
            continue
        try:
            info = path.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISREG(info.st_mode) and info.st_nlink == 1:
            candidates.append((path, info))
    candidates.sort(key=lambda item: item[0].name, reverse=True)
    for path, original in candidates[retain:]:
        try:
            current = path.lstat()
            if (stat.S_ISREG(current.st_mode) and current.st_nlink == 1
                    and (current.st_dev, current.st_ino)
                    == (original.st_dev, original.st_ino)):
                path.unlink()
        except FileNotFoundError:
            pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('log', type=Path)
    parser.add_argument('retain', type=positive)
    args = parser.parse_args()
    try:
        prune_restart_archives(args.log, args.retain)
    except OSError as exc:
        print('s3lvol log retention: {}'.format(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
