#!/bin/bash
# Download, build and install nbdkit into $WORKLOADS_DIR/nbdkit so that the
# NBD integration tests (common_sequential::nbd::*) can spawn it.
#
# Usage: scripts/prepare_nbdkit.sh [workloads_dir]
#
# Only the core server, the `file` plugin and the `delay` filter are needed,
# so every language binding is disabled to keep the build small and fast.
# Nothing is done if the binary is already installed; delete
# $WORKLOADS_DIR/nbdkit to force a rebuild. Build output goes to
# $WORKLOADS_DIR/nbdkit_build.log.

set -e

WORKLOADS_DIR="${1:-$HOME/workloads}"
NBDKIT_VER="${NBDKIT_VER:-1.34.4}"
NBDKIT_SERIES="${NBDKIT_VER%.*}"
NBDKIT_PREFIX="$WORKLOADS_DIR/nbdkit"
NBDKIT_BIN="$NBDKIT_PREFIX/sbin/nbdkit"
NBDKIT_TARBALL="nbdkit-$NBDKIT_VER.tar.gz"
NBDKIT_URL="https://download.libguestfs.org/nbdkit/$NBDKIT_SERIES-stable/$NBDKIT_TARBALL"
NBDKIT_LOG="$WORKLOADS_DIR/nbdkit_build.log"

if [ -x "$NBDKIT_BIN" ]; then
    echo "nbdkit already installed: $("$NBDKIT_BIN" --version)"
    exit 0
fi

mkdir -p "$WORKLOADS_DIR"
BUILD_DIR=$(mktemp -d "$WORKLOADS_DIR/nbdkit_build.XXXXXX")
trap 'rm -rf "$BUILD_DIR"' EXIT
: >"$NBDKIT_LOG"

fail() {
    echo "nbdkit: $1 failed, see $NBDKIT_LOG" >&2
    tail -n 20 "$NBDKIT_LOG" >&2
    exit 1
}

echo "[1/4] downloading $NBDKIT_URL"
wget --progress=dot:mega --timeout=30 --tries=3 \
    -O "$BUILD_DIR/$NBDKIT_TARBALL" "$NBDKIT_URL" || {
    echo "nbdkit: download failed (check network/proxy, or put" \
        "$NBDKIT_TARBALL into $BUILD_DIR manually)" >&2
    exit 1
}
tar xf "$BUILD_DIR/$NBDKIT_TARBALL" -C "$BUILD_DIR"
SRC_DIR="$BUILD_DIR/nbdkit-$NBDKIT_VER"

echo "[2/4] configuring (log: $NBDKIT_LOG)"
(cd "$SRC_DIR" && ./configure --prefix="$NBDKIT_PREFIX" \
    --disable-perl --disable-python --disable-ocaml --disable-rust \
    --disable-ruby --disable-tcl --disable-lua --disable-golang \
    --disable-libguestfs-tests --without-libvirt --without-libguestfs) \
    >>"$NBDKIT_LOG" 2>&1 || fail configure

echo "[3/4] building with $(nproc) jobs, usually 1-3 minutes"
(cd "$SRC_DIR" && make -j"$(nproc)") >>"$NBDKIT_LOG" 2>&1 || fail make

echo "[4/4] installing to $NBDKIT_PREFIX"
(cd "$SRC_DIR" && make install) >>"$NBDKIT_LOG" 2>&1 || fail "make install"

# Sanity check: the tests rely on these two modules.
"$NBDKIT_BIN" file --version >/dev/null
"$NBDKIT_BIN" --filter=delay null --version >/dev/null
echo "nbdkit installed: $("$NBDKIT_BIN" --version)"
