#!/usr/bin/env bash
# ============================================================================
# build-pvm-host-kernel-pkg.sh
#
# Build the pvm-host kernel installation package:
#   - Clone https://cnb.cool/CubeSandbox/OpenCloudOS-Kernel.git (tag: 6.6.117-45.16.cubesandbox)
#   - Build an RPM or DEB package depending on the host distribution family:
#       RPM: `make dist-rpm CONFIG=generic-release`, using the kernel config
#            maintained in the source tree's dist/configs
#       DEB: apply the pvm-host kernel .config, then `make bindeb-pkg`
#
# The defaults of REPO_URL / BRANCH / CONFIG_URL / CONFIG_SHA256 / WORK_DIR /
# SRC_DIR / CONFIG_FILE / OUTPUT_DIR / JOBS / SKIP_DEPS can all be overridden
# via environment variables. CONFIG_URL / CONFIG_SHA256 / CONFIG_FILE /
# LOCAL_CONFIG_FILE only affect the DEB build.
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

REPO_URL="${REPO_URL:-https://cnb.cool/CubeSandbox/OpenCloudOS-Kernel.git}"
BRANCH="${BRANCH:-6.6.117-45.16.cubesandbox}"
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

# make dist-rpm emits this complete binary package set. Keep the local build
# contract aligned with the release workflow so a partial rpmbuild result is
# rejected before it can be published.
DIST_RPM_PACKAGES=(
    bpftool
    bpftool-debuginfo
    kernel
    kernel-core
    kernel-debuginfo
    kernel-debuginfo-common
    kernel-devel
    kernel-headers
    kernel-modules
    kernel-signing-keys
    kernel-source
    kernel-tools
    kernel-tools-debuginfo
    kernel-tools-libs
    kernel-tools-libs-devel
    perf
    perf-debuginfo
    python3-perf
    python3-perf-debuginfo
)

# ------------------------- Build -------------------------
# rpmbuild -bb refuses to start while any BuildRequires of the generated
# kernel.spec (perf, tools, docs, ...) is missing, so resolve them up front.
install_dist_rpm_builddeps() {
    local pm=""
    if command -v dnf >/dev/null 2>&1; then
        pm="dnf"
    elif command -v yum >/dev/null 2>&1; then
        pm="yum"
    else
        warn "Neither dnf nor yum was found; skipping dist-rpm BuildRequires installation"
        return 0
    fi

    log "Generating kernel.spec to resolve dist-rpm BuildRequires"
    (
        cd "${SRC_DIR}"
        make dist-specfile CONFIG="generic-release"
    )

    local spec="${SRC_DIR}/dist/rpm/SOURCES/kernel.spec"
    local builddeps=()
    mapfile -t builddeps < <(rpmspec -q --buildrequires "${spec}" | awk '{print $1}' | sort -u)
    if [[ ${#builddeps[@]} -eq 0 ]]; then
        warn "No BuildRequires parsed from ${spec}"
        return 0
    fi

    log "Installing dist-rpm BuildRequires: ${builddeps[*]}"
    dnf_install_with_retry "${pm}" "${builddeps[@]}" ||
        warn "Some BuildRequires failed to install; rpmbuild will report any that are still missing"
}

build_rpm() {
    if [[ "${SKIP_DEPS:-0}" != "1" ]]; then
        install_dist_rpm_builddeps
    fi

    log "Building RPM kernel packages (dist-rpm, CONFIG=generic-release) with ${JOBS} parallel jobs"
    : >"${BUILD_MARKER}"
    (
        cd "${SRC_DIR}"
        make -j"${JOBS}" dist-rpm CONFIG="generic-release"
    )

    log "Collecting RPM artifacts into ${OUTPUT_DIR}"
    # dist-rpm runs rpmbuild with _rpmdir=<src>/dist/rpm/RPMS, so packages
    # land in <src>/dist/rpm/RPMS/<arch>/.
    collect_artifacts "${SRC_DIR}/dist/rpm/RPMS" '*.rpm'

    if ! ls -1 "${OUTPUT_DIR}"/*.rpm >/dev/null 2>&1; then
        err "No RPM artifacts were found. Check the build log above."
        exit 1
    fi

    local package arch matches
    shopt -s nullglob
    for package in "${DIST_RPM_PACKAGES[@]}"; do
        arch="x86_64"
        if [[ "${package}" == "kernel-source" ]]; then
            arch="noarch"
        fi
        matches=("${OUTPUT_DIR}/${package}"-[0-9]*.oc9."${arch}".rpm)
        if [[ ${#matches[@]} -ne 1 ]]; then
            err "Expected exactly one ${package}-*.oc9.${arch}.rpm, found ${#matches[@]}"
            exit 1
        fi
    done
    shopt -u nullglob

    log "RPM build finished. Artifacts in: ${OUTPUT_DIR}"
    ls -lh "${OUTPUT_DIR}"
}

build_deb() {
    fetch_config

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
