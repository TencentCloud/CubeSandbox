#!/usr/bin/env bash
# ============================================================================
# pvm_setup.sh
#
# One-shot bootstrap to prepare a machine for running CubeSandbox on PVM:
#
#   1. Run build-pvm-host-kernel-pkg.sh and build-pvm-guest-vmlinux.sh in
#      parallel (both scripts live next to this one in deploy/pvm/).
#   2. Ask the user for explicit confirmation, then install the built
#      pvm-host kernel package (RPM or DEB) and wire it into GRUB so that
#      the next reboot lands in the pvm-host kernel.
#   3. Place the freshly built guest vmlinux into the locations where
#      CubeSandbox expects to find it:
#        - <repo>/deploy/one-click/assets/kernel-artifacts/vmlinux
#          (used by deploy/one-click release packaging)
#        - /usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux
#          (the runtime path consumed by CubeShim / Cubelet, only copied
#           when that directory already exists on this host)
#
# Environment variables (all optional):
#   PVM_SETUP_WORK_DIR       Base dir for build scripts (default: $(pwd))
#   PVM_SETUP_ASSUME_YES=1   Skip the interactive confirmation prompt
#   PVM_SETUP_SKIP_BUILD=1   Skip the build step (reuse existing artifacts)
#   PVM_SETUP_SKIP_INSTALL=1 Skip the pvm-host package installation step
#   PVM_SETUP_SKIP_PLACE=1   Skip the guest vmlinux placement step
#   PVM_SETUP_SKIP_GRUB=1    Do not touch GRUB default / do not regenerate
#                            the GRUB configuration after installation
#   PVM_SETUP_TOOLBOX_ROOT   Override the cubetoolbox install prefix
#                            (default: /usr/local/services/cubetoolbox)
#   PVM_SETUP_ASSETS_DIR     Override the in-repo kernel-artifacts dir
#   PVM_SETUP_HOST_BUILD_DIR Override the host build dir (default:
#                            ${PVM_SETUP_WORK_DIR}/pvm-host-build)
#   PVM_SETUP_GUEST_BUILD_DIR Override the guest build dir (default:
#                            ${PVM_SETUP_WORK_DIR}/pvm-guest-build)
#   SKIP_DEPS=1              Forwarded to the two build scripts
#   JOBS, REPO_URL, BRANCH,
#   CONFIG_URL               Forwarded to the two build scripts
# ============================================================================

# Re-exec under bash if the script was invoked through /bin/sh (which on
# Debian/Ubuntu and many minimal container images is dash). dash does not
# understand `set -o pipefail`, `[[ ]]`, arrays or `${var,,}`, all of which
# this script relies on. Doing this *before* `set -euo pipefail` so the
# pipefail line itself cannot trip dash.
if [ -z "${BASH_VERSION:-}" ]; then
    if command -v bash >/dev/null 2>&1; then
        exec bash "$0" "$@"
    else
        echo "ERROR: this script requires bash, but bash was not found in PATH" >&2
        exit 1
    fi
fi

set -euo pipefail

# Make apt-get / dpkg fully non-interactive globally. Exporting it here
# rather than writing it as a per-command prefix (`DEBIAN_FRONTEND=... cmd`)
# is more robust and is a no-op on RPM distributions.
export DEBIAN_FRONTEND=noninteractive

# Make sure common system paths are present. Some invocation contexts (cron,
# systemd units, `sh -c` from other tools, stripped container shells) strip
# PATH down to just /usr/local/bin:/usr/bin, which breaks things like sudo,
# git, dnf, yum, apt-get that typically live in /usr/sbin or /sbin.
for _p in /usr/local/sbin /usr/sbin /sbin /usr/local/bin /usr/bin /bin; do
    case ":${PATH:-}:" in
    *":${_p}:"*) : ;;
    *) PATH="${PATH:+${PATH}:}${_p}" ;;
    esac
done
export PATH
unset _p

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

HOST_BUILD_SCRIPT="${SCRIPT_DIR}/build-pvm-host-kernel-pkg.sh"
GUEST_BUILD_SCRIPT="${SCRIPT_DIR}/build-pvm-guest-vmlinux.sh"

WORK_DIR="${PVM_SETUP_WORK_DIR:-$(pwd)}"
HOST_BUILD_DIR="${PVM_SETUP_HOST_BUILD_DIR:-${WORK_DIR}/pvm-host-build}"
GUEST_BUILD_DIR="${PVM_SETUP_GUEST_BUILD_DIR:-${WORK_DIR}/pvm-guest-build}"

TOOLBOX_ROOT="${PVM_SETUP_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
TOOLBOX_VMLINUX_DIR="${TOOLBOX_ROOT}/cube-kernel-scf"
ASSETS_DIR="${PVM_SETUP_ASSETS_DIR:-${REPO_ROOT}/deploy/one-click/assets/kernel-artifacts}"

ASSUME_YES="${PVM_SETUP_ASSUME_YES:-0}"

# ------------------------- Logging helpers -------------------------
log()  { echo -e "\033[1;32m[INFO ]\033[0m $*"; }
warn() { echo -e "\033[1;33m[WARN ]\033[0m $*"; }
err()  { echo -e "\033[1;31m[ERROR]\033[0m $*" 1>&2; }
step() { echo -e "\n\033[1;36m==> $*\033[0m"; }

# ------------------------- Privilege helper -------------------------
# Resolve a usable "run as root" prefix. In rootful containers / CI jobs
# there often is no sudo installed, but the shell already runs as uid 0. In
# that case SUDO is an empty string and commands run directly.
if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
    SUDO=""
elif command -v sudo >/dev/null 2>&1; then
    # `-E` preserves the exported DEBIAN_FRONTEND etc. through sudo.
    SUDO="sudo -E"
else
    echo -e "\033[1;31m[ERROR]\033[0m This script needs root privileges but neither 'sudo' is installed nor the current user is root" 1>&2
    exit 1
fi
export SUDO

# ------------------------- Argument parsing -------------------------
for arg in "$@"; do
    case "${arg}" in
        -y|--yes) ASSUME_YES=1 ;;
        -h|--help)
            sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            warn "Unknown argument: ${arg} (ignored)"
            ;;
    esac
done

# ------------------------- Platform detection -------------------------
detect_family() {
    if [[ -r /etc/os-release ]]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        local id_like="${ID_LIKE:-} ${ID:-}"
        case "${id_like}" in
            *rhel*|*centos*|*fedora*|*tencentos*|*opencloudos*|*rocky*|*almalinux*|*anolis*|*openeuler*|*suse*)
                echo "rpm"; return 0 ;;
            *debian*|*ubuntu*)
                echo "deb"; return 0 ;;
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

FAMILY="$(detect_family)"
log "Detected distribution family: ${FAMILY}"

# ------------------------- User confirmation -------------------------
confirm() {
    local prompt="$1"
    if [[ "${ASSUME_YES}" == "1" ]]; then
        log "Auto-confirm: ${prompt} [y]"
        return 0
    fi
    if [[ ! -t 0 ]]; then
        err "stdin is not a TTY; re-run with --yes (or PVM_SETUP_ASSUME_YES=1) to proceed non-interactively."
        exit 1
    fi
    local reply=""
    read -r -p "${prompt} [y/N] " reply || reply=""
    case "${reply}" in
        y|Y|yes|YES|Yes) return 0 ;;
        *) return 1 ;;
    esac
}

# ------------------------- Step 1: build -------------------------
# Pre-install the *union* of build-time dependencies for both kernels, serially
# and before launching the builds. This guarantees two things at once:
#   - The parallel builds never race for the rpm/dpkg lock, because neither of
#     them will touch the package manager afterwards (we pass SKIP_DEPS=1 to
#     both).
#   - A transient metadata/cache failure (e.g. TencentOS's classic
#     "[Errno 2] ...packages/<x>.rpm" error) gets one retry with a clean cache
#     in a controlled place, instead of being hit twice by parallel jobs.
install_common_build_deps() {
    step "Step 1a/3: install build dependencies for both kernel builds"

    _pm_retry() {
        # $1 = package manager (dnf/yum); rest = packages
        local pm="$1"; shift
        if ${SUDO} "${pm}" install -y "$@"; then return 0; fi
        warn "'${pm} install' failed once; cleaning the metadata cache and retrying..."
        ${SUDO} "${pm}" clean all || true
        ${SUDO} "${pm}" makecache || true
        ${SUDO} "${pm}" install -y "$@"
    }

    case "${FAMILY}" in
        rpm)
            local pm=""
            if command -v dnf >/dev/null 2>&1; then
                pm="dnf"
            elif command -v yum >/dev/null 2>&1; then
                pm="yum"
            else
                err "Neither dnf nor yum was found on this RPM-based system."
                exit 1
            fi
            log "Installing RPM build dependencies via ${pm} (union of host + guest needs)..."
            _pm_retry "${pm}" \
                git make gcc gcc-c++ bc bison flex \
                elfutils-libelf-devel openssl-devel \
                perl-core ncurses-devel \
                rpm-build rsync \
                dwarves cpio tar xz which findutils \
                hostname wget curl ca-certificates || {
                warn "Common dep install still failed; the sub-scripts' ensure_build_tools will try once more."
            }
            ;;
        deb)
            log "Installing DEB build dependencies via apt-get (union of host + guest needs)..."
            ${SUDO} apt-get update -y || true
            ${SUDO} apt-get install -y \
                git build-essential bc bison flex \
                libelf-dev libssl-dev libncurses-dev \
                dwarves cpio kmod \
                fakeroot rsync dpkg-dev debhelper \
                wget curl ca-certificates || {
                warn "Common dep install still failed; the sub-scripts' ensure_build_tools will try once more."
            }
            ;;
        *)
            warn "Unknown distribution family; skipping common dep install. Sub-scripts will attempt their own bootstrap."
            ;;
    esac
    unset -f _pm_retry
}

run_builds_in_parallel() {
    step "Step 1b/3: build pvm-host package and pvm-guest vmlinux in parallel"

    if [[ ! -f "${HOST_BUILD_SCRIPT}" ]]; then
        err "Build script not found: ${HOST_BUILD_SCRIPT}"
        exit 1
    fi
    if [[ ! -f "${GUEST_BUILD_SCRIPT}" ]]; then
        err "Build script not found: ${GUEST_BUILD_SCRIPT}"
        exit 1
    fi

    mkdir -p "${HOST_BUILD_DIR}" "${GUEST_BUILD_DIR}"
    local host_log="${HOST_BUILD_DIR}/pvm-setup-host-build.log"
    local guest_log="${GUEST_BUILD_DIR}/pvm-setup-guest-build.log"

    log "Host build log:  ${host_log}"
    log "Guest build log: ${guest_log}"
    log "Launching both builds; this can take a while..."

    # SKIP_DEPS=1 for *both* child scripts: the common deps were already
    # installed above, so neither needs to hit the package manager again.
    # ensure_build_tools inside each script will still verify that
    # make / gcc / bc / bison / flex are actually present.
    #
    # Invoke the child scripts explicitly through `bash` rather than relying
    # on the executable bit / shebang. This keeps things working even when
    # the repo was checked out without preserving +x (e.g. unpacked from a
    # zip on Windows) or when pvm_setup.sh itself was launched via `sh`.
    SKIP_DEPS=1 WORK_DIR="${HOST_BUILD_DIR}"  bash "${HOST_BUILD_SCRIPT}"  >"${host_log}"  2>&1 &
    local host_pid=$!

    SKIP_DEPS=1 WORK_DIR="${GUEST_BUILD_DIR}" bash "${GUEST_BUILD_SCRIPT}" >"${guest_log}" 2>&1 &
    local guest_pid=$!

    local host_rc=0
    local guest_rc=0
    wait "${host_pid}"  || host_rc=$?
    wait "${guest_pid}" || guest_rc=$?

    if [[ "${host_rc}" -ne 0 ]]; then
        err "pvm-host build failed (rc=${host_rc}). Last 40 lines of ${host_log}:"
        tail -n 40 "${host_log}" 1>&2 || true
    fi
    if [[ "${guest_rc}" -ne 0 ]]; then
        err "pvm-guest vmlinux build failed (rc=${guest_rc}). Last 40 lines of ${guest_log}:"
        tail -n 40 "${guest_log}" 1>&2 || true
    fi
    if [[ "${host_rc}" -ne 0 || "${guest_rc}" -ne 0 ]]; then
        exit 1
    fi

    log "Both builds completed successfully."
}

# ------------------------- Step 2: install pvm-host package -------------------------
list_host_packages() {
    local out_dir="${HOST_BUILD_DIR}/output"
    local pattern="$1"
    if [[ ! -d "${out_dir}" ]]; then
        return 0
    fi
    find "${out_dir}" -maxdepth 1 -type f -name "${pattern}" | sort
}

install_pvm_host_rpm() {
    local packages=()
    mapfile -t packages < <(list_host_packages '*.rpm')
    if [[ "${#packages[@]}" -eq 0 ]]; then
        err "No RPM packages found in ${HOST_BUILD_DIR}/output"
        exit 1
    fi

    echo
    log "The following pvm-host RPM packages are ready to be installed:"
    printf '    %s\n' "${packages[@]}"
    echo
    warn "After a successful install you will need to REBOOT the machine."
    warn "The default boot entry will be switched to the new pvm-host kernel."
    echo
    if ! confirm "Proceed with installing the pvm-host kernel packages above?"; then
        warn "User declined. Skipping pvm-host package installation."
        # Non-zero return tells install_pvm_host_package that no install
        # actually happened, so it must NOT touch GRUB defaults or write
        # /etc/modules-load.d/kvm_pvm.conf afterwards.
        return 1
    fi

    local installer=""
    if command -v dnf >/dev/null 2>&1; then
        installer="dnf"
    elif command -v yum >/dev/null 2>&1; then
        installer="yum"
    else
        err "Neither dnf nor yum is available on this RPM-based system."
        exit 1
    fi

    log "Installing pvm-host packages via ${SUDO:+${SUDO} }${installer} install -y ..."
    ${SUDO} "${installer}" install -y "${packages[@]}"
    log "pvm-host packages installed."
}

install_pvm_host_deb() {
    local packages=()
    mapfile -t packages < <(list_host_packages '*.deb')
    if [[ "${#packages[@]}" -eq 0 ]]; then
        err "No DEB packages found in ${HOST_BUILD_DIR}/output"
        exit 1
    fi

    echo
    log "The following pvm-host DEB packages are ready to be installed:"
    printf '    %s\n' "${packages[@]}"
    echo
    warn "After a successful install you will need to REBOOT the machine."
    warn "The default boot entry will be switched to the new pvm-host kernel."
    echo
    if ! confirm "Proceed with installing the pvm-host kernel packages above?"; then
        warn "User declined. Skipping pvm-host package installation."
        # Non-zero return tells install_pvm_host_package that no install
        # actually happened, so it must NOT touch GRUB defaults or write
        # /etc/modules-load.d/kvm_pvm.conf afterwards.
        return 1
    fi

    log "Installing pvm-host packages via ${SUDO:+${SUDO} }dpkg -i + apt-get install -f ..."
    ${SUDO} dpkg -i "${packages[@]}" || {
        warn "dpkg -i reported errors, trying 'apt-get install -f' to resolve dependencies"
        ${SUDO} apt-get install -f -y
    }
    log "pvm-host packages installed."
}

# Detect the kernel release (e.g. "6.12.33+") of the freshly built pvm-host
# kernel by extracting KERNELRELEASE from the built source tree. Prints the
# release string on stdout, or nothing if detection fails.
detect_host_kernel_release() {
    local src="${HOST_BUILD_DIR}/linux"
    if [[ ! -f "${src}/include/config/kernel.release" ]]; then
        return 0
    fi
    tr -d '[:space:]' < "${src}/include/config/kernel.release"
}

escape_shell_double_quoted_value() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//\$/\\\$}"
    value="${value//\`/\\\`}"
    printf '%s' "${value}"
}

set_grub_default_pvm_host() {
    if [[ "${PVM_SETUP_SKIP_GRUB:-0}" == "1" ]]; then
        warn "PVM_SETUP_SKIP_GRUB=1, skipping GRUB default switch."
        return 0
    fi

    local krel
    krel="$(detect_host_kernel_release || true)"
    if [[ -z "${krel}" ]]; then
        warn "Could not determine the installed pvm-host kernel release; please pick it manually in the GRUB menu after reboot."
        return 0
    fi
    log "Target pvm-host kernel release: ${krel}"

    # Try distro-specific helpers first.
    if command -v grubby >/dev/null 2>&1; then
        local kernel_path="/boot/vmlinuz-${krel}"
        if [[ -f "${kernel_path}" ]]; then
            log "Using grubby to set default kernel to ${kernel_path}"
            ${SUDO} grubby --set-default "${kernel_path}" || warn "grubby --set-default failed; please verify manually."
            return 0
        else
            warn "Expected kernel image not found at ${kernel_path}; will fall back to grub-mkconfig."
        fi
    fi

    # RPM family: grub2-mkconfig + grub2-set-default.
    if command -v grub2-mkconfig >/dev/null 2>&1; then
        local grub_cfg="/boot/grub2/grub.cfg"
        [[ -d /sys/firmware/efi ]] && grub_cfg="/boot/efi/EFI/$(awk -F= '/^ID=/{print $2}' /etc/os-release | tr -d '\"')/grub.cfg"
        log "Regenerating GRUB config at ${grub_cfg}"
        ${SUDO} grub2-mkconfig -o "${grub_cfg}" || warn "grub2-mkconfig failed; please regenerate manually."
        if command -v grub2-set-default >/dev/null 2>&1; then
            local entry_id
            entry_id="$(awk -F\' '/^menuentry /{print $2}' "${grub_cfg}" 2>/dev/null | grep -Fm1 "${krel}" || true)"
            if [[ -n "${entry_id}" ]]; then
                log "Setting default GRUB entry to: ${entry_id}"
                ${SUDO} grub2-set-default "${entry_id}" || warn "grub2-set-default failed; please set the default manually."
            else
                warn "Could not locate a GRUB menuentry matching ${krel}; please pick it manually at boot."
            fi
        fi
        return 0
    fi

    # Debian family: update-grub then fish the menuentry id.
    if command -v update-grub >/dev/null 2>&1; then
        log "Regenerating GRUB config via update-grub"
        ${SUDO} update-grub || warn "update-grub failed; please regenerate manually."
        local grub_cfg="/boot/grub/grub.cfg"
        if [[ -f "${grub_cfg}" ]]; then
            local entry_id
            entry_id="$(awk -F\' '/^menuentry /{print $2}' "${grub_cfg}" | grep -Fm1 "${krel}" || true)"
            if [[ -n "${entry_id}" ]]; then
                local grub_default_cfg="/etc/default/grub.d/pvm-default.cfg"
                local escaped_entry_id
                escaped_entry_id="$(escape_shell_double_quoted_value "${entry_id}")"
                log "Setting default GRUB entry via ${grub_default_cfg}: ${entry_id}"
                if ! ${SUDO} mkdir -p /etc/default/grub.d; then
                    warn "Failed to create /etc/default/grub.d; please set GRUB_DEFAULT manually."
                    return 0
                fi
                if ! printf 'GRUB_DEFAULT="%s"\n' "${escaped_entry_id}" | ${SUDO} tee "${grub_default_cfg}" >/dev/null; then
                    warn "Failed to write ${grub_default_cfg}; please set GRUB_DEFAULT manually."
                    return 0
                fi
                ${SUDO} update-grub || warn "update-grub failed after editing GRUB_DEFAULT."
            else
                warn "Could not locate a GRUB menuentry matching ${krel}; please pick it manually at boot."
            fi
        fi
        return 0
    fi

    warn "No known GRUB tooling found (grubby/grub2-mkconfig/update-grub). Please configure your bootloader manually."
}

# Persist `kvm_pvm` so the kernel module is auto-loaded by
# systemd-modules-load.service on every boot (after the user reboots into
# the freshly installed pvm-host kernel).
#
# We deliberately do NOT try to `modprobe kvm_pvm` here: at this point the
# running kernel is still the *old* one (the new pvm-host kernel only takes
# effect after reboot), so the module almost certainly isn't available yet
# and a live modprobe would only produce a confusing error.
#
# Covered distros: anything using systemd (>= ~RHEL 7 / Debian 8 / Ubuntu
# 15.04), which is effectively every supported pvm-host target.
# /etc/modules-load.d/*.conf is the portable, distro-agnostic location; on
# Debian/Ubuntu it supersedes the older /etc/modules file and is still the
# recommended mechanism.
enable_kvm_pvm_autoload() {
    local conf="/etc/modules-load.d/kvm_pvm.conf"
    log "Enabling auto-load of kernel module 'kvm_pvm' on boot via ${conf}"

    # Make sure the directory exists (it does on any systemd-based distro,
    # but be defensive for minimal images).
    ${SUDO} mkdir -p /etc/modules-load.d

    # Atomic write via a tempfile + install, so an interrupted run can't
    # leave behind a half-written conf file.
    local tmp
    tmp="$(mktemp)"
    cat >"${tmp}" <<'EOF'
# Auto-generated by deploy/pvm/pvm_setup.sh
# Load the PVM KVM module at boot so that CubeSandbox can start PVM guests
# immediately after the host comes up in the pvm-host kernel.
kvm_pvm
EOF
    if ${SUDO} install -m 0644 -- "${tmp}" "${conf}"; then
        log "Wrote ${conf} (will take effect on next boot under the pvm-host kernel)."
    else
        warn "Failed to write ${conf}; please create it manually with a single line 'kvm_pvm'."
    fi
    rm -f -- "${tmp}"

    # Debian/Ubuntu also honour /etc/modules for legacy reasons. Append
    # kvm_pvm there too, but only if it's not already present, so we stay
    # idempotent across repeated runs.
    if [[ "${FAMILY}" == "deb" && -f /etc/modules ]]; then
        if ! ${SUDO} grep -qE '^[[:space:]]*kvm_pvm([[:space:]]|$)' /etc/modules; then
            log "Appending 'kvm_pvm' to /etc/modules (Debian/Ubuntu legacy path)."
            # `tee -a` with sudo is the standard idiom for appending as root
            # without needing a root shell.
            echo 'kvm_pvm' | ${SUDO} tee -a /etc/modules >/dev/null || \
                warn "Failed to append 'kvm_pvm' to /etc/modules; please add it manually."
        fi
    fi
}

install_pvm_host_package() {
    step "Step 2/3: install pvm-host kernel package and configure GRUB"

    if [[ "${PVM_SETUP_SKIP_INSTALL:-0}" == "1" ]]; then
        warn "PVM_SETUP_SKIP_INSTALL=1, skipping pvm-host package installation."
        return 0
    fi

    # Track whether the install actually ran. The per-family helpers return
    # non-zero when the user declines the interactive prompt; in that case
    # we must NOT reconfigure GRUB nor enable kvm_pvm auto-loading, because
    # the machine will keep booting the existing (non-pvm) kernel.
    local installed=0
    case "${FAMILY}" in
        rpm) install_pvm_host_rpm && installed=1 || installed=0 ;;
        deb) install_pvm_host_deb && installed=1 || installed=0 ;;
        *)
            err "Unknown distribution family; cannot install pvm-host package automatically."
            err "Please install the artifacts in ${HOST_BUILD_DIR}/output manually."
            exit 1
            ;;
    esac

    if [[ "${installed}" -ne 1 ]]; then
        warn "pvm-host package was not installed; skipping GRUB default switch and kvm_pvm auto-load setup."
        return 0
    fi

    set_grub_default_pvm_host
    enable_kvm_pvm_autoload
}

# ------------------------- Step 3: place guest vmlinux -------------------------
place_guest_vmlinux() {
    step "Step 3/3: place pvm-guest vmlinux into the expected locations"

    if [[ "${PVM_SETUP_SKIP_PLACE:-0}" == "1" ]]; then
        warn "PVM_SETUP_SKIP_PLACE=1, skipping guest vmlinux placement."
        return 0
    fi

    local src="${GUEST_BUILD_DIR}/output/vmlinux"
    if [[ ! -f "${src}" ]]; then
        err "Guest vmlinux artifact not found at ${src}"
        exit 1
    fi
    if [[ ! -s "${src}" ]]; then
        err "Guest vmlinux artifact is empty: ${src}"
        exit 1
    fi

    # 3a) In-repo assets dir used by deploy/one-click release packaging.
    mkdir -p "${ASSETS_DIR}"
    cp -fv "${src}" "${ASSETS_DIR}/vmlinux"
    log "Copied guest vmlinux to ${ASSETS_DIR}/vmlinux"

    # 3b) Runtime path consumed by CubeShim / Cubelet, only if cubetoolbox
    #     is already installed on this host.
    if [[ -d "${TOOLBOX_ROOT}" ]]; then
        ${SUDO} mkdir -p "${TOOLBOX_VMLINUX_DIR}"
        ${SUDO} cp -fv "${src}" "${TOOLBOX_VMLINUX_DIR}/vmlinux"
        log "Copied guest vmlinux to ${TOOLBOX_VMLINUX_DIR}/vmlinux"
    else
        warn "${TOOLBOX_ROOT} does not exist yet; skipping the runtime copy."
        warn "After cubetoolbox is deployed, copy ${src} to ${TOOLBOX_VMLINUX_DIR}/vmlinux manually."
    fi
}

# ------------------------- Main -------------------------
main() {
    log "Script directory: ${SCRIPT_DIR}"
    log "Host build dir:   ${HOST_BUILD_DIR}"
    log "Guest build dir:  ${GUEST_BUILD_DIR}"
    log "In-repo assets:   ${ASSETS_DIR}"
    log "Toolbox root:     ${TOOLBOX_ROOT}"

    if [[ "${PVM_SETUP_SKIP_BUILD:-0}" == "1" ]]; then
        warn "PVM_SETUP_SKIP_BUILD=1, skipping build step (reusing existing artifacts)."
    else
        install_common_build_deps
        run_builds_in_parallel
    fi

    install_pvm_host_package
    place_guest_vmlinux

    echo
    log "All done."
    echo

    # Prominent, impossible-to-miss reminder: the pvm-host kernel needs
    # additional cmdline parameters (e.g. kvm.nx_huge_pages=never,
    # clearcpuid=..., mitigations=on, ...) that are NOT set by the package
    # install above. Those live in deploy/pvm/grub/host_grub_config.sh and
    # MUST be applied BEFORE the user reboots -- otherwise the new pvm-host
    # kernel boots with the old cmdline and CubeSandbox's PVM guests will
    # misbehave.
    local grub_cfg_script="${SCRIPT_DIR}/grub/host_grub_config.sh"
    echo -e "\033[1;33m================================================================\033[0m"
    echo -e "\033[1;33m IMPORTANT: Apply pvm-host GRUB cmdline BEFORE rebooting!\033[0m"
    echo -e "\033[1;33m================================================================\033[0m"
    echo "Run the following as root (idempotent; safe to re-run):"
    echo "    sudo bash ${grub_cfg_script}"
    echo "This appends the kernel cmdline parameters required by the pvm-host"
    echo "kernel to /etc/default/grub and regenerates the GRUB config."
    echo

    cat <<EOF
Next steps:
  1. Apply the pvm-host GRUB cmdline parameters (see the notice above):
       sudo bash ${grub_cfg_script}

  2. REBOOT this machine. The next boot should land in the new pvm-host
     kernel. If GRUB still shows the previous default, pick the pvm-host
     entry manually in the GRUB menu.

  3. After reboot, verify you are running the pvm-host kernel:
       uname -r

     And verify that the kvm_pvm module was auto-loaded on boot (configured
     via /etc/modules-load.d/kvm_pvm.conf):
       lsmod | grep -E '^kvm_pvm\b'

  4. Continue with the regular CubeSandbox bring-up (e.g.
     deploy/one-click/install.sh).
EOF
}

main
