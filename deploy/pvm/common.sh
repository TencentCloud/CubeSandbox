# Shared helpers for the PVM kernel build scripts.
#
# This file is sourced by build-pvm-host-kernel-pkg.sh and
# build-pvm-guest-vmlinux.sh after they have re-execed under bash and enabled
# strict mode. Keep entry-point-specific defaults in the caller.

# Make apt-get / dpkg fully non-interactive globally. This is a no-op on
# RPM/zypper distros and avoids surprises when SUDO expands to an empty string.
export DEBIAN_FRONTEND=noninteractive

# Make sure common system paths are present. Some invocation contexts (cron,
# systemd units, `sh -c` from other tools, stripped container shells) strip
# PATH down to just /usr/local/bin:/usr/bin, which breaks things like sudo,
# git, dnf, yum and apt-get.
for _p in /usr/local/sbin /usr/sbin /sbin /usr/local/bin /usr/bin /bin; do
    case ":${PATH:-}:" in
    *":${_p}:"*) : ;;
    *) PATH="${PATH:+${PATH}:}${_p}" ;;
    esac
done
export PATH
unset _p

log() { echo -e "\033[1;32m[INFO ]\033[0m $*"; }
warn() { echo -e "\033[1;33m[WARN ]\033[0m $*"; }
err() { echo -e "\033[1;31m[ERROR]\033[0m $*" 1>&2; }

resolve_sudo() {
    if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
        SUDO=""
    elif command -v sudo >/dev/null 2>&1; then
        SUDO="sudo -E"
    else
        err "This script needs root privileges but neither 'sudo' is installed nor the current user is root"
        exit 1
    fi
    export SUDO
}

ensure_core_tools() {
    local missing=()
    command -v git >/dev/null 2>&1 || missing+=(git)
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        missing+=(curl)
    fi
    [[ ${#missing[@]} -eq 0 ]] && return 0

    log "Bootstrapping missing core tools: ${missing[*]}"
    if command -v dnf >/dev/null 2>&1; then
        ${SUDO} dnf install -y "${missing[@]}" || warn "dnf bootstrap failed"
    elif command -v yum >/dev/null 2>&1; then
        ${SUDO} yum install -y "${missing[@]}" || warn "yum bootstrap failed"
    elif command -v apt-get >/dev/null 2>&1; then
        ${SUDO} apt-get update -y || true
        ${SUDO} apt-get install -y "${missing[@]}" || warn "apt-get bootstrap failed"
    elif command -v zypper >/dev/null 2>&1; then
        ${SUDO} zypper --non-interactive install "${missing[@]}" || warn "zypper bootstrap failed"
    else
        err "No supported package manager (dnf/yum/apt-get/zypper) found to install: ${missing[*]}"
        exit 1
    fi

    local t
    for t in "${missing[@]}"; do
        if ! command -v "${t}" >/dev/null 2>&1; then
            err "Required tool still missing after bootstrap: ${t}"
            exit 1
        fi
    done
}

dnf_install_with_retry() {
    local pm="$1"
    shift
    if ${SUDO} "${pm}" install -y "$@"; then
        return 0
    fi
    warn "'${pm} install' failed once; cleaning the metadata cache and retrying..."
    ${SUDO} "${pm}" clean all || true
    ${SUDO} "${pm}" makecache || true
    if ${SUDO} "${pm}" install -y "$@"; then
        log "Retry succeeded after cleaning ${pm} cache."
        return 0
    fi
    warn "'${pm} install' still failed after cache cleanup; continuing so that ensure_build_tools can take another shot."
    return 1
}

# Some kernel configs (e.g. CONFIG_KERNEL_LZ4=y, CONFIG_RD_LZ4=y,
# CONFIG_HAVE_KERNEL_LZ4=y in our pvm_host / pvm_guest) make the build invoke
# `lz4c` at link/packaging time. Modern distros often only ship the `lz4`
# command (in the `lz4` package) and no longer create the `lz4c` alias, which
# causes the kernel build to fail with: "lz4c: command not found".
#
# This helper makes sure:
#   1. The `lz4` package is installed (so that an `lz4` / `lz4c` binary exists).
#   2. If only `lz4` is available, a thin `lz4c` shim is created in
#      /usr/local/bin that forwards to `lz4`, keeping the kernel Makefiles
#      happy without patching the source tree.
ensure_lz4c() {
    if command -v lz4c >/dev/null 2>&1; then
        return 0
    fi

    if ! command -v lz4 >/dev/null 2>&1; then
        log "Installing 'lz4' package to provide lz4c/lz4 (required by CONFIG_KERNEL_LZ4 / CONFIG_RD_LZ4)"
        if command -v dnf >/dev/null 2>&1; then
            ${SUDO} dnf install -y lz4 || warn "dnf failed to install lz4"
        elif command -v yum >/dev/null 2>&1; then
            ${SUDO} yum install -y lz4 || warn "yum failed to install lz4"
        elif command -v apt-get >/dev/null 2>&1; then
            ${SUDO} apt-get update -y || true
            ${SUDO} apt-get install -y lz4 || warn "apt-get failed to install lz4"
        elif command -v zypper >/dev/null 2>&1; then
            ${SUDO} zypper --non-interactive install lz4 || warn "zypper failed to install lz4"
        else
            warn "No supported package manager found to install 'lz4'; build may fail with 'lz4c: command not found'"
        fi
    fi

    if command -v lz4c >/dev/null 2>&1; then
        return 0
    fi

    if command -v lz4 >/dev/null 2>&1; then
        local lz4_bin shim="/usr/local/bin/lz4c"
        lz4_bin="$(command -v lz4)"
        log "Creating lz4c -> ${lz4_bin} compatibility shim at ${shim}"
        ${SUDO} mkdir -p /usr/local/bin
        if ${SUDO} bash -c "cat > '${shim}' <<'EOS'
#!/bin/sh
# Auto-generated by deploy/pvm/common.sh: ensure_lz4c()
# Forward all arguments to the modern 'lz4' binary, which understands the
# same CLI flags that the kernel build uses (-l / -c0 / -c1 / -c9 ...).
exec '${lz4_bin}' \"\$@\"
EOS"; then
            ${SUDO} chmod +x "${shim}"
        else
            warn "Failed to create ${shim}; kernel build may still fail with 'lz4c: command not found'"
        fi
    fi

    if ! command -v lz4c >/dev/null 2>&1; then
        warn "lz4c is still not available; kernels with CONFIG_KERNEL_LZ4=y may fail to build"
    fi
}

ensure_build_tools() {
    local need_pkgs=()
    command -v make >/dev/null 2>&1 || need_pkgs+=(make)
    if ! command -v gcc >/dev/null 2>&1 && ! command -v cc >/dev/null 2>&1; then
        need_pkgs+=(gcc)
    fi
    command -v bc >/dev/null 2>&1 || need_pkgs+=(bc)
    command -v bison >/dev/null 2>&1 || need_pkgs+=(bison)
    command -v flex >/dev/null 2>&1 || need_pkgs+=(flex)
    if ! command -v lz4c >/dev/null 2>&1 && ! command -v lz4 >/dev/null 2>&1; then
        need_pkgs+=(lz4)
    fi
    # The kernel's libbpf / tools/bpf build invokes scripts/bpf_doc.py
    # (shebang: /usr/bin/env python3) to generate bpf_helper_defs.h. Without
    # python3 this fails with a very misleading:
    #   install -m 644 libbpf_legacy.h ...
    #   make[8]: *** [Makefile:160: .../libbpf/bpf_helper_defs.h] Error 127
    # (Error 127 = command not found, referring to the python3 interpreter
    # that runs bpf_doc.py, not to `install`.)
    if ! command -v python3 >/dev/null 2>&1; then
        need_pkgs+=(python3)
    fi

    if [[ ${#need_pkgs[@]} -eq 0 ]]; then
        ensure_lz4c
        return 0
    fi

    log "Bootstrapping missing build tools (commands): ${need_pkgs[*]}"

    if command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
        local pm="dnf"
        command -v dnf >/dev/null 2>&1 || pm="yum"
        local rpm_pkgs=(
            make gcc gcc-c++ bc bison flex
            elfutils-libelf-devel openssl-devel
            perl-core ncurses-devel
            dwarves cpio tar xz which findutils hostname lz4
            python3
        )
        if [[ "${PVM_BUILD_PROFILE:-}" == "host" ]]; then
            rpm_pkgs+=(rpm-build rsync)
        else
            rpm_pkgs+=(rsync)
        fi
        dnf_install_with_retry "${pm}" "${rpm_pkgs[@]}" || true
    elif command -v apt-get >/dev/null 2>&1; then
        local deb_pkgs=(
            build-essential make bc bison flex
            libelf-dev libssl-dev libncurses-dev
            dwarves cpio kmod lz4
            python3
        )
        if [[ "${PVM_BUILD_PROFILE:-}" == "host" ]]; then
            deb_pkgs+=(fakeroot rsync dpkg-dev debhelper)
        fi
        ${SUDO} apt-get update -y || true
        ${SUDO} apt-get install -y "${deb_pkgs[@]}" || warn "apt-get build-tool bootstrap failed"
    elif command -v zypper >/dev/null 2>&1; then
        local zypper_pkgs=(
            make gcc gcc-c++ bc bison flex
            libelf-devel libopenssl-devel ncurses-devel
            dwarves cpio lz4
            python3
        )
        if [[ "${PVM_BUILD_PROFILE:-}" == "host" ]]; then
            zypper_pkgs+=(rpm-build rsync)
        fi
        ${SUDO} zypper --non-interactive install "${zypper_pkgs[@]}" || warn "zypper build-tool bootstrap failed"
    else
        err "No supported package manager found; cannot bootstrap: ${need_pkgs[*]}"
        exit 1
    fi

    local t
    for t in make gcc; do
        if ! command -v "${t}" >/dev/null 2>&1 && [[ "${t}" != "gcc" || ! $(command -v cc) ]]; then
            err "Required build tool still missing after bootstrap: ${t}"
            exit 1
        fi
    done

    # Kernels with CONFIG_KERNEL_LZ4 / CONFIG_RD_LZ4 enabled need the `lz4c`
    # command at build time; make sure it's available (installing `lz4` and
    # falling back to a shim if the distro only ships `lz4`).
    ensure_lz4c
}

detect_family() {
    if [[ -r /etc/os-release ]]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        local id_like="${ID_LIKE:-} ${ID:-}"
        case "${id_like}" in
        *rhel* | *centos* | *fedora* | *tencentos* | *opencloudos* | *rocky* | *almalinux* | *anolis* | *openeuler* | *suse*)
            echo "rpm"
            return 0
            ;;
        *debian* | *ubuntu*)
            echo "deb"
            return 0
            ;;
        esac
    fi
    if command -v rpm >/dev/null 2>&1; then
        echo "rpm"
    elif command -v dpkg >/dev/null 2>&1; then
        echo "deb"
    else
        echo "unknown"
    fi
}

detect_pkg_type() {
    local family
    family="$(detect_family)"
    if [[ "${family}" == "unknown" ]]; then
        err "Cannot detect the package manager on this platform (neither rpm nor dpkg)"
        exit 1
    fi
    echo "${family}"
}

install_deps_rpm() {
    local label="${PVM_DEPS_LABEL:-}"
    [[ -n "${label}" ]] && label=" (${label})"
    log "Installing RPM-family build dependencies${label}..."
    local pm=""
    if command -v dnf >/dev/null 2>&1; then
        pm="dnf"
    elif command -v yum >/dev/null 2>&1; then
        pm="yum"
    else
        err "Neither dnf nor yum was found"
        exit 1
    fi

    local pkgs=(
        git make gcc gcc-c++ bc bison flex
        elfutils-libelf-devel openssl-devel
        perl-core ncurses-devel
        dwarves cpio tar xz which findutils
        hostname wget rsync lz4
        python3
    )
    if [[ "${PVM_BUILD_PROFILE:-}" == "host" ]]; then
        pkgs+=(rpm-build)
    fi
    dnf_install_with_retry "${pm}" "${pkgs[@]}" || true
}

install_deps_deb() {
    local label="${PVM_DEPS_LABEL:-}"
    [[ -n "${label}" ]] && label=" (${label})"
    log "Installing Debian-family build dependencies${label}..."
    local pkgs=(
        git build-essential bc bison flex
        libelf-dev libssl-dev libncurses-dev
        dwarves cpio kmod
        wget ca-certificates lz4
        python3
    )
    if [[ "${PVM_BUILD_PROFILE:-}" == "host" ]]; then
        pkgs+=(fakeroot rsync dpkg-dev debhelper)
    fi
    ${SUDO} apt-get update -y || true
    ${SUDO} apt-get install -y "${pkgs[@]}" || {
        warn "Some dependencies failed to install; continuing with the build"
    }
}

install_deps() {
    local family
    family="$(detect_family)"
    case "${family}" in
    rpm) install_deps_rpm ;;
    deb) install_deps_deb ;;
    *)
        warn "Unknown distribution family; skipping automatic dependency installation. Please make sure gcc/make/bc/bison/flex/libelf/openssl-dev are installed."
        ;;
    esac
}

clone_source() {
    mkdir -p "${WORK_DIR}"

    # BRANCH can be any of: a branch name, a tag name, or a commit SHA.
    # The previous implementation assumed a branch name and referenced
    # `origin/${BRANCH}` after fetch, which breaks on a second run when
    # BRANCH is actually a tag: `git fetch --depth=1 origin <tag>` only
    # updates FETCH_HEAD and does NOT create a remote-tracking ref
    # `refs/remotes/origin/<tag>`, so `git reset --hard origin/<tag>`
    # fails with
    #     fatal: ambiguous argument 'origin/<tag>': unknown revision ...
    #
    # Instead, fetch and then pin to the concrete SHA resolved via
    # FETCH_HEAD. That works uniformly for branches, tags and commit
    # SHAs, and is also correct on shallow clones.
    if [[ -d "${SRC_DIR}/.git" ]]; then
        log "Source tree already exists at ${SRC_DIR}; updating to ${BRANCH} ..."

        # Make sure we're pointing at the configured remote; if someone
        # hand-edited .git/config or re-ran with a different REPO_URL,
        # update it so we don't silently fetch from the wrong place.
        local current_url=""
        current_url="$(git -C "${SRC_DIR}" remote get-url origin 2>/dev/null || true)"
        if [[ -z "${current_url}" ]]; then
            git -C "${SRC_DIR}" remote add origin "${REPO_URL}"
        elif [[ "${current_url}" != "${REPO_URL}" ]]; then
            warn "origin URL differs (${current_url} -> ${REPO_URL}); updating."
            git -C "${SRC_DIR}" remote set-url origin "${REPO_URL}"
        fi

        # Fetch the requested ref together with its tags. --depth=1 keeps
        # things cheap; if a previous run left a deeper history behind,
        # this will simply shallow-fetch on top of it (harmless).
        if ! git -C "${SRC_DIR}" fetch --depth=1 --tags --force origin "${BRANCH}"; then
            # Some remotes won't accept a direct SHA in refspec position
            # on a shallow clone; fall back to fetching everything the
            # remote advertises and resolving locally.
            warn "Targeted fetch of '${BRANCH}' failed; retrying with a generic fetch."
            git -C "${SRC_DIR}" fetch --depth=1 --tags --force origin
        fi

        # Resolve BRANCH to a concrete commit SHA without relying on
        # refs/remotes/origin/<name> existing.
        local target_sha=""
        if target_sha="$(git -C "${SRC_DIR}" rev-parse --verify -q FETCH_HEAD)"; then
            :
        elif target_sha="$(git -C "${SRC_DIR}" rev-parse --verify -q "refs/tags/${BRANCH}")"; then
            :
        elif target_sha="$(git -C "${SRC_DIR}" rev-parse --verify -q "refs/remotes/origin/${BRANCH}")"; then
            :
        elif target_sha="$(git -C "${SRC_DIR}" rev-parse --verify -q "${BRANCH}^{commit}")"; then
            :
        else
            err "Could not resolve '${BRANCH}' to a commit in ${SRC_DIR}."
            err "Tried: FETCH_HEAD, refs/tags/${BRANCH}, refs/remotes/origin/${BRANCH}, ${BRANCH}."
            exit 1
        fi

        log "Resolved '${BRANCH}' to ${target_sha}; checking out."
        # Detach to the SHA: idempotent for branches/tags/SHAs alike, and
        # never fails because of a pre-existing local branch with the
        # same name pointing elsewhere.
        git -C "${SRC_DIR}" checkout --detach --quiet "${target_sha}"
        git -C "${SRC_DIR}" reset --hard "${target_sha}"
        # Drop any untracked leftovers from a previous, possibly failed
        # build so the tree really is at `${target_sha}` and nothing else.
        git -C "${SRC_DIR}" clean -fdx
    else
        log "Cloning ${REPO_URL} (ref ${BRANCH}) into ${SRC_DIR} ..."
        # `git clone --branch` accepts either a branch or a tag name, so
        # this single line handles both; it does not accept bare commit
        # SHAs, but REPO_URL+BRANCH combos consumed by this script always
        # resolve to a named ref.
        git clone --depth=1 --branch "${BRANCH}" "${REPO_URL}" "${SRC_DIR}"
    fi
}

verify_downloaded_config() {
    if [[ -z "${CONFIG_SHA256:-}" ]]; then
        warn "CONFIG_SHA256 is not set; downloaded kernel config integrity cannot be verified."
        return 0
    fi
    if ! command -v sha256sum >/dev/null 2>&1; then
        err "sha256sum is unavailable; cannot verify the downloaded kernel config."
        exit 1
    fi

    local actual
    actual="$(sha256sum "${CONFIG_FILE}" | awk '{print $1}')"
    if [[ "${actual}" != "${CONFIG_SHA256}" ]]; then
        err "Downloaded kernel config checksum mismatch."
        err "  expected: ${CONFIG_SHA256}"
        err "  actual:   ${actual}"
        exit 1
    fi
    log "Verified downloaded kernel config sha256: ${CONFIG_SHA256}"
}

fetch_config() {
    mkdir -p "$(dirname -- "${CONFIG_FILE}")"

    if [[ -n "${LOCAL_CONFIG_FILE:-}" && -f "${LOCAL_CONFIG_FILE}" && -r "${LOCAL_CONFIG_FILE}" ]]; then
        log "Using local kernel config: ${LOCAL_CONFIG_FILE}"
        cp -f -- "${LOCAL_CONFIG_FILE}" "${CONFIG_FILE}"
    else
        if [[ -n "${LOCAL_CONFIG_FILE:-}" ]]; then
            warn "Local kernel config not found at '${LOCAL_CONFIG_FILE}'; falling back to CONFIG_URL"
        fi
        log "Downloading kernel config: ${CONFIG_URL}"
        if command -v curl >/dev/null 2>&1; then
            curl -fsSL --retry 3 -o "${CONFIG_FILE}" "${CONFIG_URL}"
        elif command -v wget >/dev/null 2>&1; then
            wget -q -O "${CONFIG_FILE}" "${CONFIG_URL}"
        else
            err "Neither curl nor wget is available to download the config file"
            exit 1
        fi
        verify_downloaded_config

        warn "========================================================================"
        warn "WARNING: built ${PVM_BUILD_OUTPUT_LABEL:-PVM artifact} using the upstream CONFIG_URL."
        warn "         The resulting artifact MAY FAIL TO BOOT on your ${PVM_TARGET_DESC:-target system}."
        warn "         For a boot-tested configuration, use the bundled config:"
        warn "             ${SCRIPT_DIR}/configs/${PVM_CONFIG_NAME:-pvm_config}"
        warn "         (either place it at that path, or pass it explicitly via"
        warn "          LOCAL_CONFIG_FILE=/path/to/${PVM_CONFIG_NAME:-pvm_config})."
        warn "========================================================================"
    fi

    cp -f "${CONFIG_FILE}" "${SRC_DIR}/.config"
    log "Applied .config and running 'make olddefconfig'"
    (cd "${SRC_DIR}" && make olddefconfig)
}

canonicalise_path() {
    local p="$1"
    if command -v realpath >/dev/null 2>&1; then
        realpath -m -- "${p}" 2>/dev/null && return 0
    fi
    if command -v readlink >/dev/null 2>&1; then
        local r
        r="$(readlink -f -- "${p}" 2>/dev/null || true)"
        if [[ -n "${r}" ]]; then
            echo "${r}"
            return 0
        fi
    fi
    local parent base
    parent="$(dirname -- "${p}")"
    base="$(basename -- "${p}")"
    if [[ -d "${parent}" ]]; then
        (cd "${parent}" && printf '%s/%s\n' "$(pwd -P)" "${base}")
    else
        echo "${p}"
    fi
}

clean_output_dir() {
    if [[ -z "${OUTPUT_DIR:-}" ]]; then
        warn "OUTPUT_DIR is empty; skipping cleanup"
        return 0
    fi

    local out_canon work_canon
    out_canon="$(canonicalise_path "${OUTPUT_DIR}")"
    work_canon="$(canonicalise_path "${WORK_DIR}")"

    if [[ -z "${out_canon}" || "${out_canon}" == "/" ]]; then
        err "Refusing to clean unsafe OUTPUT_DIR: '${OUTPUT_DIR}' (canonical: '${out_canon}')"
        exit 1
    fi
    if [[ "${out_canon}" != "${work_canon}/"* && "${out_canon}" != "${work_canon}" ]]; then
        warn "OUTPUT_DIR '${out_canon}' is outside WORK_DIR '${work_canon}'; cleaning it anyway because the user explicitly pointed here"
    fi

    if [[ -d "${out_canon}" ]]; then
        log "Cleaning previous artifacts in ${out_canon}"
        find "${out_canon}" -mindepth 1 -delete 2>/dev/null || rm -rf "${out_canon:?}"/* "${out_canon:?}"/.[!.]* 2>/dev/null || true
    fi
    mkdir -p "${out_canon}"

    COLLECTED_BASENAMES=()
}

declare -gA COLLECTED_BASENAMES=()

collect_artifacts() {
    local search_root_raw="$1"
    local pattern="$2"

    mkdir -p "${OUTPUT_DIR}"
    local output_canon
    output_canon="$(canonicalise_path "${OUTPUT_DIR}")"

    local search_root
    search_root="$(canonicalise_path "${search_root_raw}")"
    [[ -d "${search_root}" ]] || return 0

    local newer_opt=()
    if [[ -f "${BUILD_MARKER:-}" ]]; then
        newer_opt=(-newer "${BUILD_MARKER}")
    fi

    while IFS= read -r f; do
        [[ -n "${f}" ]] || continue

        local f_canon
        f_canon="$(canonicalise_path "${f}")"

        if [[ "${f_canon}" == "${output_canon}/"* ]]; then
            continue
        fi

        local base
        base="$(basename -- "${f_canon}")"

        if [[ -n "${COLLECTED_BASENAMES[${base}]:-}" ]]; then
            continue
        fi
        COLLECTED_BASENAMES["${base}"]=1

        cp -v -- "${f_canon}" "${OUTPUT_DIR}/"
    done < <(find "${search_root}" -maxdepth 5 \
        -path "${output_canon}" -prune -o \
        -type f -name "${pattern}" "${newer_opt[@]}" -print 2>/dev/null)
}
