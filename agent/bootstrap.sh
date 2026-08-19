#!/usr/bin/env bash

set -euo pipefail

if [ "$(id -u)" != "0" ]; then
    echo "This script must be run as root" >&2
    exit 1
fi

STATIC_LIBSECCOMP_DIR=/usr/local/lib64/libseccomp
TMP_COMPILE_DIR=/tmp/libseccomp
LIBSECCOMP_VERSION=2.5.1
LIBSECCOMP_URL=https://github.com/seccomp/libseccomp/archive/refs/tags/v${LIBSECCOMP_VERSION}.tar.gz
PACKAGE_MANAGER="yum"

detect_package_manager() {
    if [ "$(uname -s)" = "Darwin" ]; then
        echo "Fatal error: macOS is not a supported build host for libseccomp" >&2
        exit 1
    fi

    if hash 2>/dev/null apt-get; then
        PACKAGE_MANAGER="apt-get"
    elif hash 2>/dev/null dnf; then
        PACKAGE_MANAGER="dnf"
    elif hash 2>/dev/null yum; then
        PACKAGE_MANAGER="yum"
    else
        printf '\e[31;1mFatal error: \e[0;31mUnsupported platform, please open an issue\e[0m\n' >&2
        exit 1
    fi
}

detect_package_manager

if [ ! -d "${STATIC_LIBSECCOMP_DIR}" ]; then
    rm -rf "${TMP_COMPILE_DIR}"
    mkdir -p "${TMP_COMPILE_DIR}"
    (
        cd "${TMP_COMPILE_DIR}"
        wget -c "${LIBSECCOMP_URL}"
        tar zxvf "v${LIBSECCOMP_VERSION}.tar.gz"
        cd "libseccomp-${LIBSECCOMP_VERSION}"
        "${PACKAGE_MANAGER}" -y install gperf libtool
        sh ./autogen.sh
        ./configure CFLAGS="-U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=1 -O2" \
            --enable-shared --enable-static --prefix="${STATIC_LIBSECCOMP_DIR}"
        make -j "$(nproc)"
        mkdir -p "${STATIC_LIBSECCOMP_DIR}"
        make install
    )
    rm -rf "${TMP_COMPILE_DIR}"
fi

echo "Bootstrap complete!"
