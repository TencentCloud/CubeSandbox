#!/bin/bash
# One-shot runner for the NBD integration tests:
#   1. prepare nbdkit into $WORKLOADS_DIR (skipped if already installed)
#   2. build cube-hypervisor and ch-remote, which the tests spawn from
#      target/$BUILD_TARGET/release/
#   3. run common_sequential::nbd::* sequentially
#
# Usage: scripts/run_nbd_integration_tests.sh [extra cargo test args]
# Guest images (focal, kernel) must already be in $WORKLOADS_DIR, as for any
# other integration test.

set -e

cd "$(dirname "$0")/.."

WORKLOADS_DIR="${WORKLOADS_DIR:-$HOME/workloads}"
export BUILD_TARGET="${BUILD_TARGET:-$(uname -m)-unknown-linux-gnu}"
export RUST_BACKTRACE=1

scripts/prepare_nbdkit.sh "$WORKLOADS_DIR"

echo "building cube-hypervisor and ch-remote ($BUILD_TARGET)"
cargo build --release --target "$BUILD_TARGET" \
    --bin cube-hypervisor --bin ch-remote

cargo test --release --target "$BUILD_TARGET" --test integration "$@" -- \
    --exact --test-threads=1 --nocapture \
    common_sequential::nbd::test_nbd_cold_boot \
    common_sequential::nbd::test_nbd_pause_resume \
    common_sequential::nbd::test_nbd_snapshot_restore \
    common_sequential::nbd::test_nbd_reconnect
