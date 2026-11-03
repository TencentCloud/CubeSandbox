#!/usr/bin/env bash
set -euo pipefail

# Usage: bash deploy/pvm/tests/test_host_kernel_patch.sh /path/to/kernel-repo
# The input repository must contain the default PVM host source tag.
TEST_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KERNEL_REPO="$(cd -- "${1:?pass a local PVM kernel repository}" && pwd)"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT
SRC_DIR="${WORK_DIR}/linux"
OUTPUT_DIR="${WORK_DIR}/output"
CONFIG_FILE="${WORK_DIR}/pvm_host.config"
LOCAL_CONFIG_FILE="${TEST_DIR}/../configs/pvm_host"
JOBS=1

# shellcheck source=deploy/pvm/build-pvm-host-kernel-pkg.sh
source "${TEST_DIR}/../build-pvm-host-kernel-pkg.sh"

git clone --quiet --shared --no-checkout --branch "${BRANCH}" "${KERNEL_REPO}" "${SRC_DIR}"
git -C "${SRC_DIR}" restore --source HEAD --staged --worktree -- \
    net/ipv4/af_inet.c net/ipv4/tcp_ipv4.c scripts/config

assert_rcu_free() {
    grep -Fq 'kfree_rcu(rcu_dereference_protected(inet->inet_opt, 1), rcu);' \
        "${SRC_DIR}/net/ipv4/af_inet.c" &&
    grep -Fq 'kfree_rcu(rcu_dereference_protected(inet_rsk(req)->ireq_opt, 1), rcu);' \
        "${SRC_DIR}/net/ipv4/tcp_ipv4.c"
}

if assert_rcu_free; then
    echo 'FAIL: baseline already contains the fix' >&2
    exit 1
fi

if (PVM_KERNEL_COMMIT=0000000000000000000000000000000000000000; patch_source); then
    echo 'FAIL: accepted the wrong source commit' >&2
    exit 1
fi
git -C "${SRC_DIR}" diff --exit-code -- net/ipv4/af_inet.c net/ipv4/tcp_ipv4.c

patch_source
assert_rcu_free
git -C "${SRC_DIR}" apply --reverse --check \
    "${SCRIPT_DIR}/patches/0001-ipv4-free-inet_opt-and-ireq_opt-after-an-RCU-grace-period.patch"

if patch_source; then
    echo 'FAIL: accepted an already-patched source tree' >&2
    exit 1
fi
assert_rcu_free

# A fresh build resets the source before applying the patch again.
git -C "${SRC_DIR}" restore -- net/ipv4/af_inet.c net/ipv4/tcp_ipv4.c
patch_source
assert_rcu_free

# Exercise the real main/config path without installing tools or building packages.
SKIP_DEPS=1
resolve_sudo() { :; }
ensure_core_tools() { :; }
ensure_build_tools() { :; }
detect_pkg_type() { echo "${TEST_PKG_TYPE}"; }
clone_source() {
    git -C "${SRC_DIR}" restore -- net/ipv4/af_inet.c net/ipv4/tcp_ipv4.c
}
make() { [[ "$*" == olddefconfig ]]; }
check_package_input() {
    assert_rcu_free
    [[ "${LOCALVERSION}" == -rcu742f73df544e ]]
    [[ "$("${SRC_DIR}/scripts/config" --file "${SRC_DIR}/.config" --state LOCALVERSION_AUTO)" == n ]]
}
build_deb() { check_package_input; }
build_rpm() { check_package_input; }
for TEST_PKG_TYPE in deb rpm; do
    main
done
echo 'PASS: source pin, RCU backport, duplicate rejection, and DEB/RPM build inputs'
