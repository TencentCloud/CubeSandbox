#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Entry point used by rcow_srpc in both the source checkout and the release
#  package. Applies the 3.8 stdlib shim, then runs SPDK's rpc.py unchanged:
#
#    package:  scripts/spdk_rpc.py   (copied by make_release.sh)
#    repo:     $SPDK_ROOT/scripts/rpc.py
#
#  argv is forwarded as-is so existing `rpc.py -s sock -t 5 spdk_get_version`
#  callers do not change.
#
#  S3LVOL_RPC_DISABLE_FALLBACKS: test-only. When set to a non-empty value,
#  skip the machine-level search (deps/spdk, /opt/s3lvol-spdk, ../spdk,
#  /data/home/cow/spdk) so a suite can assert the missing-upstream error
#  even inside the builder image. Sibling, SPDK_RPC_PY, and SPDK_ROOT
#  still win. Do not set this in a deployment.

import os
import runpy
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

_packaged_python = os.path.join(HERE, 'python')
if os.path.isdir(_packaged_python):
    sys.path.insert(0, _packaged_python)

import rpc_compat  # noqa: E402  # patches argparse before SPDK rpc.py imports it


def _existing(path):
    return path if path and os.path.isfile(path) else ''


def find_upstream_rpc_py():
    sibling = _existing(os.path.join(HERE, 'spdk_rpc.py'))
    if sibling:
        return sibling

    env_rpc = _existing(os.environ.get('SPDK_RPC_PY', ''))
    if env_rpc:
        return env_rpc

    spdk_root = os.environ.get('SPDK_ROOT', '')
    from_root = _existing(os.path.join(spdk_root, 'scripts', 'rpc.py')) if spdk_root else ''
    if from_root:
        return from_root

    if not os.environ.get('S3LVOL_RPC_DISABLE_FALLBACKS'):
        repo_root = os.path.dirname(HERE)
        for candidate in (
                os.path.join(repo_root, 'deps', 'spdk', 'scripts', 'rpc.py'),
                '/opt/s3lvol-spdk/scripts/rpc.py',
                os.path.join(repo_root, '..', 'spdk', 'scripts', 'rpc.py'),
                '/data/home/cow/spdk/scripts/rpc.py',
        ):
            found = _existing(os.path.abspath(candidate))
            if found:
                return found

    sys.stderr.write(
        'cannot find SPDK rpc.py: expected scripts/spdk_rpc.py in a package, '
        'or SPDK_ROOT/scripts/rpc.py in a checkout\n')
    sys.exit(1)


if __name__ == '__main__':
    runpy.run_path(find_upstream_rpc_py(), run_name='__main__')
