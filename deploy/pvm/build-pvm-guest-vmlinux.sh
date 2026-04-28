#!/usr/bin/env bash
# ============================================================================
# build-pvm-guest-vmlinux.sh
#
# Clone https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git (tag: 6.6.69-1.cubesandbox),
# apply the pvm-guest kernel .config, and build only the vmlinux target
# (no RPM/DEB packaging, no kernel modules).
#
# The defaults of REPO_URL / BRANCH / CONFIG_URL / CONFIG_SHA256 / WORK_DIR /
# SRC_DIR / CONFIG_FILE / OUTPUT_DIR / JOBS / SKIP_DEPS can all be overridden
# via environment variables.
# ============================================================================

if [ -z "${BASH_VERSION:-}" ]; then
    if command -v bash >/dev/null 2>&1; then
        exec bash "$0" "$@"
    else
        echo "ERROR: this script requires bash, but bash was not found in PATH" >&2
        exit 1
    fi
fi

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd -P)"
# shellcheck source=deploy/pvm/common.sh
source "${SCRIPT_DIR}/common.sh"

# ------------------------- Configurable parameters (env overridable) -------------------------
PVM_BUILD_PROFILE="guest"
PVM_BUILD_OUTPUT_LABEL="pvm-guest vmlinux"
PVM_CONFIG_NAME="pvm_guest"
PVM_DEPS_LABEL="vmlinux only"
PVM_TARGET_DESC="target system"

REPO_URL="${REPO_URL:-https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git}"
BRANCH="${BRANCH:-6.6.69-1.cubesandbox}"
CONFIG_URL="${CONFIG_URL:-https://raw.githubusercontent.com/virt-pvm/misc/refs/heads/main/pvm-guest-6.12.33.config}"
CONFIG_SHA256="${CONFIG_SHA256:-8e579bea756b6dadeff1203a5e4f3bba851a7426e4e50abd436575eddfa019f4}"

# Preferred in-repo kernel config. If present it is used instead of
# CONFIG_URL, so air-gapped / offline build environments work out of the box.
# Set LOCAL_CONFIG_FILE=/dev/null to force the CONFIG_URL path.
LOCAL_CONFIG_FILE="${LOCAL_CONFIG_FILE:-${SCRIPT_DIR}/configs/pvm_guest}"

WORK_DIR="${WORK_DIR:-$(pwd)/pvm-guest-build}"
SRC_DIR="${SRC_DIR:-${WORK_DIR}/linux}"
CONFIG_FILE="${CONFIG_FILE:-${WORK_DIR}/pvm-guest-6.12.33.config}"
OUTPUT_DIR="${OUTPUT_DIR:-${WORK_DIR}/output}"

JOBS="${JOBS:-$(nproc)}"

# ------------------------- Build vmlinux -------------------------
build_vmlinux() {
    log "Building vmlinux with ${JOBS} parallel jobs..."
    (
        cd "${SRC_DIR}"
        make clean && make -j"${JOBS}" vmlinux
        mv vmlinux vmlinux.full
        objcopy -S vmlinux.full vmlinux
    )

    mkdir -p "${OUTPUT_DIR}"
    local vmlinux_src="${SRC_DIR}/vmlinux"
    if [[ ! -f "${vmlinux_src}" ]]; then
        err "Build artifact not found: ${vmlinux_src}"
        exit 1
    fi
    if [[ ! -s "${vmlinux_src}" ]]; then
        err "Build artifact is empty: ${vmlinux_src}"
        exit 1
    fi

    cp -fv "${vmlinux_src}" "${OUTPUT_DIR}/vmlinux"
    log "vmlinux build finished. Artifact: ${OUTPUT_DIR}/vmlinux"
    ls -lh "${OUTPUT_DIR}/vmlinux"
}

# ------------------------- Main -------------------------
main() {
    log "Working directory: ${WORK_DIR}"
    mkdir -p "${WORK_DIR}"

    # Every invocation starts from a clean OUTPUT_DIR so that a stale vmlinux
    # from a previous build can't be mistaken for a fresh artifact.
    clean_output_dir

    resolve_sudo

    if [[ "${SKIP_DEPS:-0}" != "1" ]]; then
        install_deps
    else
        warn "SKIP_DEPS=1, skipping dependency installation"
    fi

    # Make absolutely sure git/curl are available even when SKIP_DEPS=1 or
    # the platform-specific dep install silently dropped them.
    ensure_core_tools
    # Same idea for make / gcc / bc / bison / flex: without these the build
    # simply cannot run, so we insist on having them regardless of how the
    # previous dep-install step fared (or even when SKIP_DEPS=1 was set).
    ensure_build_tools

    clone_source
    fetch_config
    build_vmlinux

    log "All done."
}

main "$@"
