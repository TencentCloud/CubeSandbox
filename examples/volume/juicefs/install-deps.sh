#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# install-deps.sh — install host tools for the JuiceFS Volume Plugin.
#
#   --juicefs      JuiceFS client from GitHub Releases, checksum verified
#   --fuse         FUSE userspace tools (fusermount)
#   --jq           JSON output of the plugin script
#   --all          everything above (every CubeMaster and Cubelet node)
#   --check-only   verify, install nothing
#
#   JUICEFS_VERSION=1.4.1   which release to install (default: the pinned one below)
#
# Usage:
#   sudo ./install-deps.sh --all
#   ./install-deps.sh --all --check-only     # no root needed

set -euo pipefail

WANT_JUICEFS=0
WANT_FUSE=0
WANT_JQ=0
CHECK_ONLY=0

log()  { printf '[juicefs-deps] %s\n' "$*"; }
die()  { printf '[juicefs-deps] ERROR: %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --juicefs)    WANT_JUICEFS=1; shift ;;
        --fuse)       WANT_FUSE=1;    shift ;;
        --jq)         WANT_JQ=1;      shift ;;
        --all)        WANT_JUICEFS=1; WANT_FUSE=1; WANT_JQ=1; shift ;;
        --check-only) CHECK_ONLY=1;   shift ;;
        -h|--help)    sed -n '4,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)            die "unknown argument: $1" ;;
    esac
done

if [[ "$WANT_JUICEFS$WANT_FUSE$WANT_JQ" == "000" ]]; then
    die "nothing selected; pass --juicefs / --fuse / --jq / --all (see --help)"
fi

if [[ "$CHECK_ONLY" -eq 0 && "$(id -u)" -ne 0 ]]; then
    die "must run as root to install (or pass --check-only)"
fi

pkg_install() {
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y -q "$@"
    elif command -v yum >/dev/null 2>&1; then
        yum install -y -q "$@"
    else
        die "no supported package manager (apt/dnf/yum); install manually: $*"
    fi
}

# The release this plugin is written and tested against. Pinned rather than
# "latest" so a node installed months from now gets the same binary; set
# JUICEFS_VERSION to move deliberately.
JUICEFS_DEFAULT_VERSION="1.4.1"

install_juicefs() {
    local arch
    case "$(uname -m)" in
        x86_64)  arch=amd64 ;;
        aarch64) arch=arm64 ;;
        *)       die "unsupported architecture: $(uname -m)" ;;
    esac

    local version="${JUICEFS_VERSION:-$JUICEFS_DEFAULT_VERSION}"

    local base="https://github.com/juicedata/juicefs/releases/download/v${version}"
    local tarball="juicefs-${version}-linux-${arch}.tar.gz"
    local tmp
    tmp="$(mktemp -d)"

    log "downloading JuiceFS ${version} (${arch})"
    curl -fsSL -o "${tmp}/${tarball}" "${base}/${tarball}"
    curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt"
    (cd "$tmp" && grep " ${tarball}\$" checksums.txt | sha256sum -c -) \
        || { rm -rf "$tmp"; die "checksum mismatch for ${tarball}"; }
    tar -xzf "${tmp}/${tarball}" -C "$tmp" juicefs
    install -m 0755 "${tmp}/juicefs" /usr/local/bin/juicefs
    rm -rf "$tmp"
}

missing=0

check() {
    local name="$1"
    if command -v "$name" >/dev/null 2>&1; then
        log "ok: $name ($(command -v "$name"))"
        return 0
    fi
    log "missing: $name"
    return 1
}

# The plugin needs these at runtime and the installer needs them to fetch and
# verify the release. Without this, --check-only can report success on a node
# that then fails at the first attach.
for tool in flock mountpoint curl tar sha256sum; do
    check "$tool" || missing=1
done

if [[ "$WANT_JQ" -eq 1 || "$WANT_JUICEFS" -eq 1 ]]; then
    if ! check jq; then
        if [[ "$CHECK_ONLY" -eq 1 ]]; then missing=1; else pkg_install jq; fi
    fi
fi

if [[ "$WANT_FUSE" -eq 1 ]]; then
    if ! check fusermount && ! check fusermount3; then
        if [[ "$CHECK_ONLY" -eq 1 ]]; then missing=1; else pkg_install fuse3 || pkg_install fuse; fi
    fi
    [[ -e /dev/fuse ]] || { log "missing: /dev/fuse (load the fuse kernel module)"; missing=1; }
fi

if [[ "$WANT_JUICEFS" -eq 1 ]]; then
    want="${JUICEFS_VERSION:-$JUICEFS_DEFAULT_VERSION}"
    if ! check juicefs; then
        if [[ "$CHECK_ONLY" -eq 1 ]]; then missing=1; else install_juicefs; fi
    # Version token with a boundary, so a request for 1.4.1 is not satisfied by
    # 1.4.10, while the build suffix (1.4.1+2026-...) still matches.
    elif ! juicefs version | grep -qE "(^|[^0-9.])${want//./\.}([^0-9]|$)"; then
        # An existing binary of another version is replaced, so asking for a
        # version means something on a node that already has one.
        log "juicefs present but not ${want}: $(juicefs version)"
        if [[ "$CHECK_ONLY" -eq 1 ]]; then missing=1; else install_juicefs; fi
    fi
    command -v juicefs >/dev/null 2>&1 && log "$(juicefs version)"
fi

if [[ "$missing" -ne 0 ]]; then
    die "some dependencies are missing"
fi
log "done"
