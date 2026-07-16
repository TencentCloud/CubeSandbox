#!/bin/sh
set -eu

log() { printf '[pvm-host-bootstrap] %s\n' "$*"; }
fail() { printf '[pvm-host-bootstrap] ERROR: %s\n' "$*" >&2; exit 1; }

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
if [ -f "${SCRIPT_DIR}/node-prep-lib.sh" ]; then
  # shellcheck disable=SC1091
  . "${SCRIPT_DIR}/node-prep-lib.sh"
fi

HOST_ROOT="${HOST_ROOT:-/host}"
STATE_DIR="${STATE_DIR:-/var/lib/cube-node-bootstrap}"
DESIRED_KERNEL_PATTERN="${DESIRED_KERNEL_PATTERN:-cubesandbox.pvm.host}"
RPM_PATH="${PVM_KERNEL_RPM_PATH:-/artifacts/kernel-pvm-host.rpm}"
DEB_PATH="${PVM_KERNEL_DEB_PATH:-/artifacts/linux-image-pvm-host.deb}"
RPM_URL="${PVM_KERNEL_RPM_URL:-}"
DEB_URL="${PVM_KERNEL_DEB_URL:-}"
PACKAGE_SOURCE="${PVM_KERNEL_PACKAGE_SOURCE:-image}"
KERNEL_BOOT_ARGS="${PVM_KERNEL_BOOT_ARGS:-}"
REBOOT_ENABLED="${REBOOT_ENABLED:-true}"
REBOOT_MAX_COUNT="${REBOOT_MAX_COUNT:-1}"
REBOOT_COORDINATED="${REBOOT_COORDINATED:-true}"
LEASE_NAME="${LEASE_NAME:-cube-node-pvm-bootstrap}"
LEASE_TTL_SECONDS="${LEASE_TTL_SECONDS:-900}"
NODE_NAME="${NODE_NAME:-$(hostname)}"
NAMESPACE="${POD_NAMESPACE:-default}"
# REV3.2: export for node-prep-lib fingerprint helpers
PVM_ENABLED="${PVM_ENABLED:-1}"
export HOST_ROOT STATE_DIR DESIRED_KERNEL_PATTERN KERNEL_BOOT_ARGS PVM_ENABLED

host_path() { printf '%s%s' "$HOST_ROOT" "$1"; }
lease_time() { date -u +%Y-%m-%dT%H:%M:%S.000000Z; }
lease_expired() {
  renew_time="$(kubectl -n "$NAMESPACE" get lease "$LEASE_NAME" -o jsonpath='{.spec.renewTime}' 2>/dev/null || true)"
  if [ -z "$renew_time" ]; then
    return 0
  fi
  renew_epoch="$(date -u -d "$renew_time" +%s 2>/dev/null || echo 0)"
  now_epoch="$(date -u +%s)"
  if [ "$renew_epoch" -eq 0 ]; then
    return 0
  fi
  [ $((now_epoch - renew_epoch)) -ge "$LEASE_TTL_SECONDS" ]
}
json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}
shell_quote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}
host_sh() {
  # SECURITY: host_sh forwards its arguments to `/bin/sh -c` inside the host
  # namespace, so callers must build the command string with shell_quote for
  # any value that is not a literal constant. This function is intentionally
  # kept for scripts that assemble shell pipelines. For invocations that only
  # need argv-style execution use host_run instead, which does not go through
  # `sh -c`.
  nsenter --target 1 --uts --ipc --net --pid -- chroot "$HOST_ROOT" /bin/sh -c "$*"
}
host_run() {
  # Exec argv on the host without a shell interpreter. Preferred entry point
  # for calls that don't need pipelines / redirection because each argument
  # is passed to execve() untouched, so no meta-characters ($, `, ;, &&, etc.)
  # trigger unintended shell expansion.
  nsenter --target 1 --uts --ipc --net --pid -- chroot "$HOST_ROOT" "$@"
}

release_lease() {
  if [ "$REBOOT_COORDINATED" != "true" ]; then
    return 0
  fi
  command -v kubectl >/dev/null 2>&1 || return 0
  holder="$(kubectl -n "$NAMESPACE" get lease "$LEASE_NAME" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)"
  if [ "$holder" = "$NODE_NAME" ]; then
    kubectl -n "$NAMESPACE" patch lease "$LEASE_NAME" --type merge \
      -p '{"spec":{"holderIdentity":""}}' >/dev/null 2>&1 || true
    log "lease released"
  fi
}

mkdir -p "$(host_path "$STATE_DIR")"

acquire_lease() {
  if [ "$REBOOT_COORDINATED" != "true" ]; then
    log "coordinated reboot disabled; skip lease"
    return 0
  fi
  command -v kubectl >/dev/null 2>&1 || fail "kubectl is required in pvm-host-bootstrap image when coordinated reboot is enabled"
  log "waiting for lease ${NAMESPACE}/${LEASE_NAME} as ${NODE_NAME}"
  while true; do
    if ! kubectl -n "$NAMESPACE" get lease "$LEASE_NAME" >/dev/null 2>&1; then
      cat <<EOF | kubectl -n "$NAMESPACE" apply -f - >/dev/null 2>&1 || true
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: ${LEASE_NAME}
  namespace: ${NAMESPACE}
spec:
  holderIdentity: ""
  leaseDurationSeconds: ${LEASE_TTL_SECONDS}
EOF
    fi
    holder="$(kubectl -n "$NAMESPACE" get lease "$LEASE_NAME" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)"
    if [ -z "$holder" ] || [ "$holder" = "$NODE_NAME" ] || lease_expired; then
      now="$(lease_time)"
      patch_file="$(mktemp)"
      cat > "$patch_file" <<EOF
[
  {"op":"test","path":"/spec/holderIdentity","value":"$(json_escape "$holder")"},
  {"op":"replace","path":"/spec/holderIdentity","value":"$(json_escape "$NODE_NAME")"},
  {"op":"replace","path":"/spec/leaseDurationSeconds","value":${LEASE_TTL_SECONDS}},
  {"op":"replace","path":"/spec/renewTime","value":"${now}"}
]
EOF
      if kubectl -n "$NAMESPACE" patch lease "$LEASE_NAME" --type json --patch-file "$patch_file" >/dev/null 2>&1; then
        rm -f "$patch_file"
        log "lease acquired"
        return 0
      fi
      rm -f "$patch_file"
      log "lease changed before acquire; retrying"
    fi
    log "lease held by ${holder}; sleep 15s"
    sleep 15
  done
}

# REV3.2: only acquire the cluster reboot lease on paths that may reboot.
# Do NOT acquire on the fast-success path (already on desired kernel).

will_mutate_host() {
  if command -v invalidate_node_prep_ready >/dev/null 2>&1; then
    invalidate_node_prep_ready
  else
    rm -f "$(host_path "$STATE_DIR/node-prep-ready")" "$(host_path "$STATE_DIR/node-prep-ready.tmp")"
  fi
}

install_kernel() {
  if [ "$PACKAGE_SOURCE" = "download" ]; then
    mkdir -p /artifacts
    if [ -n "$RPM_URL" ] && [ ! -f "$RPM_PATH" ]; then
      log "downloading PVM host kernel rpm from ${RPM_URL}"
      curl -fL --retry 3 -o "$RPM_PATH" "$RPM_URL"
    fi
    if [ -n "$DEB_URL" ] && [ ! -f "$DEB_PATH" ]; then
      log "downloading PVM host kernel deb from ${DEB_URL}"
      curl -fL --retry 3 -o "$DEB_PATH" "$DEB_URL"
    fi
  fi
  if [ -f "$RPM_PATH" ]; then
    log "installing PVM host kernel rpm from ${RPM_PATH}"
    cp "$RPM_PATH" "$(host_path /tmp/cube-pvm-host-kernel.rpm)"
    host_sh "rpm -ivh --oldpackage --replacepkgs /tmp/cube-pvm-host-kernel.rpm || rpm -Uvh --oldpackage --replacepkgs /tmp/cube-pvm-host-kernel.rpm"
  elif [ -f "$DEB_PATH" ]; then
    log "installing PVM host kernel deb from ${DEB_PATH}"
    cp "$DEB_PATH" "$(host_path /tmp/cube-pvm-host-kernel.deb)"
    host_sh "dpkg -i /tmp/cube-pvm-host-kernel.deb"
  else
    fail "no PVM host kernel package found: rpm=${RPM_PATH}, deb=${DEB_PATH}"
  fi
  echo "installed" > "$(host_path "$STATE_DIR/pvm-kernel-installed")"
}

configure_bootloader() {
  log "configuring bootloader for kernel pattern ${DESIRED_KERNEL_PATTERN}"
  boot_args="$(shell_quote "$KERNEL_BOOT_ARGS")"
  # Quote DESIRED_KERNEL_PATTERN so operator-supplied values (arriving via
  # bootstrap.pvmHostKernel.desiredKernelPattern in the chart) cannot inject
  # shell metacharacters into the host shell interpreter.
  pattern="$(shell_quote "$DESIRED_KERNEL_PATTERN")"
  if [ -x "$(host_path /usr/sbin/grubby)" ]; then
    host_sh "PVM_KERNEL_BOOT_ARGS=${boot_args}; PVM_DESIRED_PATTERN=${pattern}; pvm_kernel=\$(ls /boot/vmlinuz-*\"\${PVM_DESIRED_PATTERN}\"* 2>/dev/null | sort | tail -1); test -n \"\$pvm_kernel\"; grubby --set-default \"\$pvm_kernel\"; if [ -n \"\$PVM_KERNEL_BOOT_ARGS\" ]; then grubby --update-kernel \"\$pvm_kernel\" --args \"\$PVM_KERNEL_BOOT_ARGS\"; fi"
  elif [ -x "$(host_path /usr/sbin/update-grub)" ] || [ -x "$(host_path /usr/sbin/grub-mkconfig)" ]; then
    host_sh "PVM_KERNEL_BOOT_ARGS=${boot_args}; PVM_DESIRED_PATTERN=${pattern}; pvm_kernel=\$(ls /boot/vmlinuz-*\"\${PVM_DESIRED_PATTERN}\"* 2>/dev/null | sed 's|/boot/vmlinuz-||' | sort | tail -1); test -n \"\$pvm_kernel\"; if [ -f /etc/default/grub ]; then sed -i \"s|^GRUB_DEFAULT=.*|GRUB_DEFAULT=\\\"Advanced options for Ubuntu>Ubuntu, with Linux \${pvm_kernel}\\\"|\" /etc/default/grub; if [ -n \"\$PVM_KERNEL_BOOT_ARGS\" ]; then if grep -q '^GRUB_CMDLINE_LINUX=' /etc/default/grub; then sed -i \"s|^GRUB_CMDLINE_LINUX=\\\"\\(.*\\)\\\"|GRUB_CMDLINE_LINUX=\\\"\\1 \${PVM_KERNEL_BOOT_ARGS}\\\"|\" /etc/default/grub; else printf 'GRUB_CMDLINE_LINUX=\\\"%s\\\"\\n' \"\$PVM_KERNEL_BOOT_ARGS\" >> /etc/default/grub; fi; fi; fi; if command -v update-grub >/dev/null 2>&1; then update-grub; elif command -v grub-mkconfig >/dev/null 2>&1; then grub-mkconfig -o /boot/grub/grub.cfg; fi"
  else
    fail "no supported bootloader tool found in host root"
  fi
  mkdir -p "$(host_path /etc/modules-load.d)" "$(host_path /etc/udev/rules.d)"
  printf 'kvm_pvm\n' > "$(host_path /etc/modules-load.d/kvm-pvm.conf)"
  printf 'KERNEL=="kvm", MODE="0666"\n' > "$(host_path /etc/udev/rules.d/99-kvm.rules)"
}

missing_boot_args() {
  [ -n "$KERNEL_BOOT_ARGS" ] || return 0
  cmdline=" $(cat /proc/cmdline 2>/dev/null || true) "
  missing=""
  for arg in $KERNEL_BOOT_ARGS; do
    case "$cmdline" in
      *" ${arg} "*) ;;
      *) missing="${missing} ${arg}" ;;
    esac
  done
  [ -z "$missing" ] || printf '%s\n' "$missing"
}

request_reboot_or_fail() {
  count_name="${1:-reboot-count}"
  not_ready_message="${2:-PVM kernel installed but host is not running it yet}"
  count_file="$(host_path "$STATE_DIR/${count_name}")"
  count="0"
  [ -f "$count_file" ] && count="$(cat "$count_file" || echo 0)"
  if [ "$REBOOT_ENABLED" = "true" ] && [ "$count" -lt "$REBOOT_MAX_COUNT" ]; then
    count=$((count + 1))
    echo "$count" > "$count_file"
    lease_time > "$(host_path "$STATE_DIR/reboot-requested")"
    log "rebooting host ${NODE_NAME}; reboot-count=${count}/${REBOOT_MAX_COUNT}"
    host_sh "if command -v systemctl >/dev/null 2>&1; then systemctl reboot; else reboot; fi" || true
    # The reboot command either succeeds (host goes down within a minute or
    # two) or fails (in which case waiting any longer will not help). Sleep
    # briefly so the init container exits with a clear failure signal after
    # the reboot request has had time to take effect, letting the DaemonSet
    # restart with the next attempt. The prior 1 hour sleep pinned the pod
    # in Init state and hid the underlying reboot failure from operators.
    sleep "${REBOOT_WAIT_SECONDS:-120}"
    exit 1
  fi

  fail "${not_ready_message}; reboot_enabled=${REBOOT_ENABLED}, reboot_count=${count}/${REBOOT_MAX_COUNT}, missing_boot_args=$(missing_boot_args)"
}

current_kernel="$(uname -r || true)"
log "current kernel: ${current_kernel}"
if printf '%s' "$current_kernel" | grep -q "$DESIRED_KERNEL_PATTERN"; then
  missing_args="$(missing_boot_args)"
  if [ -z "$missing_args" ]; then
    log "PVM kernel check passed (fast path; no lease)"
    echo "$current_kernel" > "$(host_path "$STATE_DIR/pvm-ready")"
    exit 0
  fi
  log "PVM kernel is running but required boot args are missing:${missing_args}"
  will_mutate_host
  acquire_lease
  configure_bootloader
  request_reboot_or_fail boot-args-reboot-count "PVM kernel boot args configured but host is not running with required boot args yet"
fi

will_mutate_host
acquire_lease
install_kernel
configure_bootloader
request_reboot_or_fail reboot-count "PVM kernel installed but host is not running it yet"
