#!/usr/bin/env bash
# ============================================================================
# build-pvm-host-kernel-pkg.sh
#
# Build the pvm-host kernel installation package:
#   - Clone https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git (tag: 6.6.69-1.cubesandbox)
#   - Apply the pvm-host kernel .config
#   - Build an RPM or DEB package depending on the host distribution family
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
PVM_BUILD_PROFILE="host"
PVM_BUILD_OUTPUT_LABEL="pvm-host kernel package"
PVM_CONFIG_NAME="pvm_host"
PVM_TARGET_DESC="target host"

REPO_URL="${REPO_URL:-https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git}"
BRANCH="${BRANCH:-6.6.69-1.cubesandbox}"
CONFIG_URL="${CONFIG_URL:-https://raw.githubusercontent.com/virt-pvm/misc/refs/heads/main/pvm-host-6.12.33.config}"
CONFIG_SHA256="${CONFIG_SHA256:-edc1965a48fbe972ee6eb3d1be96de3d0fd00c1edfe665974330dea819545e00}"

# Preferred in-repo kernel config. If present it is used instead of
# CONFIG_URL, so air-gapped / offline build environments work out of the box.
# Set LOCAL_CONFIG_FILE=/dev/null to force the CONFIG_URL path.
LOCAL_CONFIG_FILE="${LOCAL_CONFIG_FILE:-${SCRIPT_DIR}/configs/pvm_host}"

WORK_DIR="${WORK_DIR:-$(pwd)/pvm-host-build}"
SRC_DIR="${SRC_DIR:-${WORK_DIR}/linux}"
CONFIG_FILE="${CONFIG_FILE:-${WORK_DIR}/pvm-host-6.12.33.config}"
OUTPUT_DIR="${OUTPUT_DIR:-${WORK_DIR}/output}"

JOBS="${JOBS:-$(nproc)}"

# Timestamp marker used to filter "freshly built" rpm/deb artifacts.
# It is created right before the actual build step starts.
BUILD_MARKER="${WORK_DIR}/.build-start.stamp"

# ------------------------- Build -------------------------
build_rpm() {
    log "Building RPM kernel package (binrpm-pkg) with ${JOBS} parallel jobs"
    : >"${BUILD_MARKER}"
    (
        cd "${SRC_DIR}"
        make -j"${JOBS}" binrpm-pkg
    )

    log "Collecting RPM artifacts into ${OUTPUT_DIR}"
    # Recent kernels direct binrpm-pkg output into <src>/rpmbuild/RPMS/<arch>/
    # via RPM_BUILD_ROOT, which is the primary location we care about.
    local in_tree_rpm_base="${SRC_DIR}/rpmbuild/RPMS"
    if [[ -d "${in_tree_rpm_base}" ]]; then
        log "Harvesting in-tree RPMs from ${in_tree_rpm_base}"
        collect_artifacts "${in_tree_rpm_base}" '*.rpm'
    fi
    # Legacy layout: binrpm-pkg writes to ~/rpmbuild/RPMS/<arch>/ on some distros.
    local home_rpm_base="${HOME}/rpmbuild/RPMS"
    if [[ -d "${home_rpm_base}" ]]; then
        log "Harvesting HOME-scoped RPMs from ${home_rpm_base}"
        collect_artifacts "${home_rpm_base}" '*.rpm'
    fi
    # Some kernel versions additionally drop rpms next to the source tree.
    collect_artifacts "${SRC_DIR}/.." '*.rpm'

    if ! ls -1 "${OUTPUT_DIR}"/*.rpm >/dev/null 2>&1; then
        err "No RPM artifacts were found. Check the build log above."
        exit 1
    fi

    log "RPM build finished. Artifacts in: ${OUTPUT_DIR}"
    ls -lh "${OUTPUT_DIR}"
}

build_deb() {
    log "Building DEB kernel package (bindeb-pkg) with ${JOBS} parallel jobs"
    : >"${BUILD_MARKER}"
    (
        cd "${SRC_DIR}"
        make -j"${JOBS}" bindeb-pkg
    )

    log "Collecting DEB artifacts into ${OUTPUT_DIR}"
    # bindeb-pkg places the .deb files in the parent directory of the sources.
    collect_artifacts "${SRC_DIR}/.." '*.deb'

    if ! ls -1 "${OUTPUT_DIR}"/*.deb >/dev/null 2>&1; then
        err "No DEB artifacts were found. Check the build log above."
        exit 1
    fi

    log "DEB build finished. Artifacts in: ${OUTPUT_DIR}"
    ls -lh "${OUTPUT_DIR}"
}

# ------------------------- Main -------------------------
main() {
    log "Working directory: ${WORK_DIR}"
    mkdir -p "${WORK_DIR}"

    # Every invocation starts from a clean OUTPUT_DIR so that leftover RPMs /
    # DEBs from a previous build never mix into the current run's results.
    clean_output_dir

    resolve_sudo

    local pkg_type
    pkg_type="$(detect_pkg_type)"
    log "Detected package type: ${pkg_type}"

    if [[ "${SKIP_DEPS:-0}" != "1" ]]; then
        case "${pkg_type}" in
        rpm) install_deps_rpm ;;
        deb) install_deps_deb ;;
        esac
    else
        warn "SKIP_DEPS=1, skipping dependency installation"
    fi

    # Make absolutely sure git/curl are available even when SKIP_DEPS=1 or
    # the platform-specific dep install silently dropped them.
    ensure_core_tools
    # Same idea for make / gcc / bc / bison / flex / rpm-build: without these
    # the build simply cannot run, so we insist on having them regardless of
    # how the previous dep-install step fared.
    ensure_build_tools

    clone_source
    fetch_config

    case "${pkg_type}" in
    rpm) build_rpm ;;
    deb) build_deb ;;
    *)
        err "Unknown package type: ${pkg_type}"
        exit 1
        ;;
    esac

    log "All done."
}

main "$@"
