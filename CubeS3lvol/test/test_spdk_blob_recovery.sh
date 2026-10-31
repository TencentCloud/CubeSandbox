#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Builder prebuilts omit test sources. Use a dedicated complete checkout;
# never reverse the fix in the storage library used by other tests.
export SPDK_DIR="${ROOT}/deps/spdk-blob-regression"
"${ROOT}/setup_dep.sh" --jobs "$(nproc)" spdk
UNIT="${SPDK_DIR}/test/unit/lib/blob/blob.c"
PATCH="${ROOT}/patches/0007-blob-register-parent-before-recovery-delete.patch"
restore_fix() {
  if git -C "${SPDK_DIR}" apply --check --include=lib/blob/blobstore.c "${PATCH}" 2>/dev/null; then
    git -C "${SPDK_DIR}" apply --include=lib/blob/blobstore.c "${PATCH}"
  fi
}
trap restore_fix EXIT

# Keep the regression fixture while removing only the production repair.
git -C "${SPDK_DIR}" apply --reverse --include=lib/blob/blobstore.c "${PATCH}"
touch "${UNIT}/blob_ut.c"
make -C "${UNIT}" -j"$(nproc)"
python3 - "${UNIT}/blob_ut" <<'PY'
import signal
import subprocess
import sys

for suite in ('blob_nocopy_noextent', 'blob_nocopy_extent'):
    result = subprocess.run([sys.argv[1], '-s', suite, '-t', 'blob_recover_incomplete_nested_snapshot'],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    print(result.stdout, flush=True)
    if result.returncode != -signal.SIGABRT or 'blob_get_snapshot_and_clone_entries' not in result.stdout:
        raise SystemExit(f'{suite}: expected the original parent/clone assertion, got {result.returncode}')
    print(f'{suite}: reproduced the original assertion', flush=True)
PY
restore_fix
touch "${UNIT}/blob_ut.c"
make -C "${UNIT}" -j"$(nproc)"
# The full binary includes extent/no-extent and device-copy configurations.
"${UNIT}/blob_ut"
