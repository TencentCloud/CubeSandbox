#!/usr/bin/env bash
# ============================================================================
# build_mvm_vmlinux.sh
#
# Purpose
#   1) Build an aarch64 cross-compile image from the Dockerfile in this dir;
#   2) Clone the OpenCloudOS-Kernel source (default tag: 6.6.69-1.1.cubesandbox)
#      onto the host, idempotently, using mvm.config from this dir as .config;
#   3) Cross-compile the ARM64 kernel Image inside that image and copy
#      arch/arm64/boot/Image into OUTPUT_DIR;
#   4) Print the manual follow-up steps (vmlinux placement + shim cmdline).
#
# CLI
#   build_mvm_vmlinux.sh [--clean] [--skip-build-image] [--no-cache]
#                       [--tag <kernel_tag>] [--jobs <N>]
#                       [--work-dir <dir>] [--output-dir <dir>]
#                       [--image <docker_image_tag>] [-h|--help]
#
# Environment overrides (CLI flags take precedence)
#   BUILD_IMAGE       Build image tag (default: ...:YYYYMMDD)
#   REPO_URL          Kernel git repo URL
#   KERNEL_TAG        Kernel git tag
#   WORK_DIR          Host working directory (mounted into the container)
#   OUTPUT_DIR        Artifact output directory
#   JOBS              Parallel build jobs (default: nproc)
#   SKIP_BUILD_IMAGE  Set to 1 to skip the docker build step
#   CLEAN_BUILD       Set to 1 to wipe SRC_DIR before cloning
#   DOCKER_NO_CACHE   Set to 1 to pass --no-cache to docker build
# ============================================================================

set -Eeuo pipefail

# --------------------------- Config & defaults ------------------------------
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd -P)"
SCRIPT_NAME="$(basename -- "${BASH_SOURCE[0]}")"

BUILD_IMAGE="${BUILD_IMAGE:-csighub.tencentyun.com/likexu/tencentos4-build:$(date +%Y%m%d)}"
REPO_URL="${REPO_URL:-https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git}"
KERNEL_TAG="${KERNEL_TAG:-6.6.69-1.1.cubesandbox}"

WORK_DIR="${WORK_DIR:-$(pwd)/mvm-build}"
OUTPUT_DIR="${OUTPUT_DIR:-}" # resolved after WORK_DIR is finalised
JOBS="${JOBS:-$(nproc 2>/dev/null || echo 4)}"

SKIP_BUILD_IMAGE="${SKIP_BUILD_IMAGE:-0}"
CLEAN_BUILD="${CLEAN_BUILD:-0}"
DOCKER_NO_CACHE="${DOCKER_NO_CACHE:-0}"

CONFIG_FILE="${SCRIPT_DIR}/mvm.config"
CMDLINE_FILE="${SCRIPT_DIR}/mvm.cmdline"

# --------------------------- Logging ---------------------------------------
log() { echo -e "\033[1;32m[INFO ]\033[0m $*"; }
warn() { echo -e "\033[1;33m[WARN ]\033[0m $*"; }
err() { echo -e "\033[1;31m[ERROR]\033[0m $*" >&2; }

on_error() {
    local exit_code=$?
    local line_no=${1:-?}
    err "Script failed at line ${line_no} (exit=${exit_code}). See ${BUILD_LOG:-<no log>} for details."
    exit "${exit_code}"
}
trap 'on_error ${LINENO}' ERR

# --------------------------- Helpers ---------------------------------------
usage() {
    cat <<EOF
Usage: ${SCRIPT_NAME} [options]

Options:
  --tag <tag>          Kernel git tag (default: ${KERNEL_TAG})
  --jobs <N>           Parallel build jobs  (default: ${JOBS})
  --work-dir <dir>     Host working directory  (default: ${WORK_DIR})
  --output-dir <dir>   Artifact output directory  (default: <work-dir>/output)
  --image <image:tag>  Build image tag  (default: ${BUILD_IMAGE})
  --skip-build-image   Reuse an existing build image, skip 'docker build'
  --no-cache           Pass --no-cache to 'docker build'
  --clean              Remove the source tree before cloning (full rebuild)
  -h, --help           Show this help and exit

Examples:
  # First run
  ${SCRIPT_NAME}

  # Re-run reusing image; force a clean source tree
  SKIP_BUILD_IMAGE=1 ${SCRIPT_NAME} --clean

  # Build a different tag
  ${SCRIPT_NAME} --tag 6.6.69-1.1.cubesandbox --jobs 16
EOF
}

require_cmd() {
    local c
    for c in "$@"; do
        if ! command -v "${c}" >/dev/null 2>&1; then
            err "Required command not found in PATH: ${c}"
            exit 127
        fi
    done
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
        --tag)
            KERNEL_TAG="${2:?}"
            shift 2
            ;;
        --jobs)
            JOBS="${2:?}"
            if ! [[ "${JOBS}" =~ ^[1-9][0-9]*$ ]]; then
                err "Invalid --jobs value '${JOBS}': must be a positive integer"
                exit 2
            fi
            shift 2
            ;;
        --work-dir)
            WORK_DIR="${2:?}"
            shift 2
            ;;
        --output-dir)
            OUTPUT_DIR="${2:?}"
            shift 2
            ;;
        --image)
            BUILD_IMAGE="${2:?}"
            shift 2
            ;;
        --skip-build-image)
            SKIP_BUILD_IMAGE=1
            shift
            ;;
        --no-cache)
            DOCKER_NO_CACHE=1
            shift
            ;;
        --clean)
            CLEAN_BUILD=1
            shift
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            err "Unknown argument: $1"
            usage
            exit 2
            ;;
        esac
    done

    # Now that WORK_DIR is final, derive paths that depend on it.
    SRC_DIR="${WORK_DIR}/linux"
    OUTPUT_DIR="${OUTPUT_DIR:-${WORK_DIR}/output}"
    BUILD_LOG="${OUTPUT_DIR}/build.log"
}

# --------------------------- 1. Build image --------------------------------
prepare_builder() {
    if [[ "${SKIP_BUILD_IMAGE}" == "1" ]]; then
        if ! docker image inspect "${BUILD_IMAGE}" >/dev/null 2>&1; then
            err "SKIP_BUILD_IMAGE=1 but image '${BUILD_IMAGE}' is not present locally."
            err "Pull/build it first, or drop --skip-build-image."
            exit 1
        fi
        warn "Reusing existing build image: ${BUILD_IMAGE}"
        return
    fi

    log "Building build image: ${BUILD_IMAGE}"
    local extra=()
    [[ "${DOCKER_NO_CACHE}" == "1" ]] && extra+=(--no-cache)

    # The Dockerfile uses 'RUN --mount=type=cache', which requires BuildKit.
    # Note: --network=host is intentionally NOT used. The build image only
    # downloads packages via dnf and does not require host networking; using
    # the default bridge network is a minor security hardening.
    DOCKER_BUILDKIT=1 docker build \
        "${extra[@]}" \
        -t "${BUILD_IMAGE}" \
        --file "${SCRIPT_DIR}/Dockerfile" \
        "${SCRIPT_DIR}"
}

# --------------------------- 2. Clone source -------------------------------
# Idempotent: works for fresh clone, re-runs, and tag switches alike.
clone_source() {
    mkdir -p "${WORK_DIR}"

    if [[ "${CLEAN_BUILD}" == "1" && -d "${SRC_DIR}" ]]; then
        warn "CLEAN_BUILD=1, removing existing source tree: ${SRC_DIR}"
        rm -rf -- "${SRC_DIR}"
    fi

    if [[ -d "${SRC_DIR}/.git" ]]; then
        log "Source tree exists at ${SRC_DIR}; updating to tag '${KERNEL_TAG}' ..."

        # Containers / CI often run as root while the tree was cloned by a
        # different uid; mark it safe so 'git' won't refuse to operate.
        # Note: this modifies the user's GLOBAL git config. We use idempotent
        # add so re-runs do not accumulate duplicate safe.directory entries.
        if ! git config --global --get-all safe.directory 2>/dev/null |
            grep -Fxq "${SRC_DIR}"; then
            log "Marking ${SRC_DIR} as a safe git directory (global git config)."
            git config --global --add safe.directory "${SRC_DIR}" >/dev/null 2>&1 || true
        fi

        # Realign origin to REPO_URL (handles user re-running with a different REPO_URL).
        local current_url=""
        current_url="$(git -C "${SRC_DIR}" remote get-url origin 2>/dev/null || true)"
        if [[ -z "${current_url}" ]]; then
            git -C "${SRC_DIR}" remote add origin "${REPO_URL}"
        elif [[ "${current_url}" != "${REPO_URL}" ]]; then
            warn "origin URL differs (${current_url} -> ${REPO_URL}); updating."
            git -C "${SRC_DIR}" remote set-url origin "${REPO_URL}"
        fi

        # Force-fetch the requested tag so a moved/updated tag overrides the
        # local copy. --tags + --force is essential here.
        if ! git -C "${SRC_DIR}" fetch --depth=1 --tags --force origin \
            "refs/tags/${KERNEL_TAG}:refs/tags/${KERNEL_TAG}"; then
            warn "Targeted tag fetch failed; retrying with a generic fetch."
            git -C "${SRC_DIR}" fetch --tags --force origin
        fi

        local target_sha
        target_sha="$(git -C "${SRC_DIR}" rev-parse --verify -q "refs/tags/${KERNEL_TAG}^{commit}")" || {
            err "Tag '${KERNEL_TAG}' not found at ${REPO_URL}"
            exit 1
        }

        log "Resolved tag '${KERNEL_TAG}' -> ${target_sha}; checking out (detached)."
        git -C "${SRC_DIR}" checkout --detach --quiet "${target_sha}"
        git -C "${SRC_DIR}" reset --hard "${target_sha}"
        # Drop ALL untracked / build artefacts so a previous run's leftovers
        # cannot pollute this build.
        git -C "${SRC_DIR}" clean -fdx
    else
        log "Cloning ${REPO_URL} (tag: ${KERNEL_TAG}) into ${SRC_DIR} ..."
        git clone --depth=1 --branch "${KERNEL_TAG}" "${REPO_URL}" "${SRC_DIR}"
    fi
}

# --------------------------- 3. Build inside container ---------------------
build_in_container() {
    if [[ ! -f "${CONFIG_FILE}" ]]; then
        err "Kernel config not found: ${CONFIG_FILE}"
        exit 1
    fi

    mkdir -p "${OUTPUT_DIR}"

    # Run as the invoking host user so that artefacts under WORK_DIR are not
    # owned by root and easy to delete/edit afterwards.
    local uid gid
    uid="$(id -u)"
    gid="$(id -g)"

    log "Cross-compiling ARM64 kernel (image=${BUILD_IMAGE}, jobs=${JOBS}, uid=${uid}:${gid})"
    log "Build log: ${BUILD_LOG}"

    # All steps that touch SRC_DIR happen inside the container so that
    # tooling versions (make/gcc/aarch64-gcc) come from the image, not the host.
    local in_container_script
    in_container_script=$(
        cat <<'EOSH'
set -Eeuo pipefail
echo "[container] kernel: $(make -s kernelversion 2>/dev/null || echo unknown)"
echo "[container] cross : $(${CROSS_COMPILE}gcc --version | head -1)"

cp -f "${HOST_CONFIG_FILE}" "${SRC_DIR}/.config"
cd "${SRC_DIR}"
make ARCH="${ARCH}" CROSS_COMPILE="${CROSS_COMPILE}" olddefconfig
make ARCH="${ARCH}" CROSS_COMPILE="${CROSS_COMPILE}" -j"${JOBS}" Image
EOSH
    )

    docker run --rm \
        --user "${uid}:${gid}" \
        -e HOME=/tmp \
        -e ARCH=arm64 \
        -e CROSS_COMPILE=aarch64-linux-gnu- \
        -e SRC_DIR="${SRC_DIR}" \
        -e HOST_CONFIG_FILE="${CONFIG_FILE}" \
        -e JOBS="${JOBS}" \
        -v "${WORK_DIR}:${WORK_DIR}" \
        -v "${SCRIPT_DIR}:${SCRIPT_DIR}:ro" \
        -w "${SRC_DIR}" \
        "${BUILD_IMAGE}" \
        bash -c "${in_container_script}" 2>&1 | tee "${BUILD_LOG}"

    local image_src="${SRC_DIR}/arch/arm64/boot/Image"
    if [[ ! -s "${image_src}" ]]; then
        err "Build artifact not found or empty: ${image_src}"
        exit 1
    fi

    # Keep both a stable name and a tag/sha-stamped copy for traceability.
    local sha
    sha="$(git -C "${SRC_DIR}" rev-parse --short=12 HEAD 2>/dev/null || echo nogit)"
    local stamped="${OUTPUT_DIR}/Image-${KERNEL_TAG}-${sha}"

    cp -f "${image_src}" "${OUTPUT_DIR}/Image"
    cp -f "${image_src}" "${stamped}"

    log "Build finished."
    log "  Stable artifact: ${OUTPUT_DIR}/Image"
    log "  Tagged copy:     ${stamped}"
    ls -lh "${OUTPUT_DIR}/Image" "${stamped}"
}

# --------------------------- 4. Manual follow-up hint ----------------------
post_hint() {
    local cmdline_value=""
    if [[ -f "${CMDLINE_FILE}" ]]; then
        # Strip CR & LF so the printed value is single-line and copy-paste safe.
        cmdline_value="$(tr -d '\r\n' <"${CMDLINE_FILE}")"
    else
        cmdline_value="(missing: ${CMDLINE_FILE})"
    fi

    cat <<EOF

============================================================================
[NEXT STEPS] Please complete the following two steps manually:

  1) Use the ARM64 kernel Image below as the mvm vmlinux and place it in
     the proper location:
       ${OUTPUT_DIR}/Image
     (source path: ${SRC_DIR}/arch/arm64/boot/Image)

  2) Use the recommended mvm.cmdline value in shim:
       file:    ${CMDLINE_FILE}
       content: ${cmdline_value}
============================================================================

EOF
}

# --------------------------- Main ------------------------------------------
main() {
    parse_args "$@"

    require_cmd docker git

    mkdir -p "${OUTPUT_DIR}"

    log "Working directory: ${WORK_DIR}"
    log "Source directory:  ${SRC_DIR}"
    log "Output directory:  ${OUTPUT_DIR}"
    log "Build image:       ${BUILD_IMAGE}"
    log "Kernel source:     ${REPO_URL} (tag: ${KERNEL_TAG})"
    log "Parallel jobs:     ${JOBS}"

    prepare_builder
    clone_source
    build_in_container
    post_hint

    log "All done."
}

main "$@"
