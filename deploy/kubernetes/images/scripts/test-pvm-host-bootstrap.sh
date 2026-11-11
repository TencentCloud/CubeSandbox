#!/bin/sh
set -eu

REPO_ROOT="$(CDPATH= cd -- "$(dirname "$0")/../../../.." && pwd)"
SCRIPT="$REPO_ROOT/deploy/kubernetes/images/scripts/pvm-host-bootstrap.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

mkdir -p "$TMP_DIR/bin" "$TMP_DIR/host/boot" "$TMP_DIR/host/usr/sbin" \
  "$TMP_DIR/host/tmp" "$TMP_DIR/artifacts" "$TMP_DIR/state"
printf 'rpm\n' > "$TMP_DIR/artifacts/kernel-pvm-host.rpm"
printf 'meta\n' > "$TMP_DIR/artifacts/kernel-pvm-host-meta.rpm"
printf 'modules\n' > "$TMP_DIR/artifacts/kernel-pvm-host-modules.rpm"
printf 'devel\n' > "$TMP_DIR/artifacts/kernel-pvm-host-devel.rpm"
printf 'headers\n' > "$TMP_DIR/artifacts/kernel-pvm-host-headers.rpm"
: > "$TMP_DIR/host/boot/vmlinuz-6.6.117-45.16.cubesandbox.oc9.x86_64"
: > "$TMP_DIR/host/boot/vmlinuz-6.6.117-opencloudos9.cubesandbox.pvm.host-gold"
: > "$TMP_DIR/host/usr/sbin/grubby"
chmod +x "$TMP_DIR/host/usr/sbin/grubby"

cat > "$TMP_DIR/bin/uname" <<'EOF'
#!/bin/sh
printf '%s\n' '6.6.117-opencloudos9.cubesandbox.pvm.host-gold'
EOF
cat > "$TMP_DIR/bin/kubectl" <<'EOF'
#!/bin/sh
case "$*" in
  *"get lease"*jsonpath*) printf '%s' "$NODE_NAME" ;;
  *"get lease"*) exit 0 ;;
  *"patch lease"*) exit 0 ;;
  *"get node"*) printf 'NoSchedule=true ' ;;
esac
EOF
cat > "$TMP_DIR/bin/nsenter" <<'EOF'
#!/bin/sh
set -eu
while [ "$1" != "chroot" ]; do shift; done
shift
host_root="$1"
shift
if [ "${1:-}" = /bin/sh ] && [ "${2:-}" = -c ]; then
  cmd="$3"
  case "$cmd" in
    "command -v rpm"*) exit 0 ;;
    *"rpm -Uvh"*) printf '%s\n' "$cmd" >> "$TEST_LOG" ;;
    *"grubby --set-default"*)
      printf '%s\n' "$cmd" >> "$TEST_LOG"
      case "$cmd" in
        *"PVM_TARGET_RELEASE='6.6.117-45.16.cubesandbox.oc9.x86_64'"*) ;;
        *) exit 1 ;;
      esac
      ;;
    *) printf 'unexpected host shell command: %s\n' "$cmd" >&2; exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = rpm ] && [ "${2:-}" = -qp ]; then
  printf '%s\n' '6.6.117-45.16.cubesandbox.oc9.x86_64'
  exit 0
fi
printf 'unexpected host argv: %s\n' "$*" >&2
exit 1
EOF
chmod +x "$TMP_DIR/bin/"*

export PATH="$TMP_DIR/bin:$PATH"
export TEST_LOG="$TMP_DIR/bootstrap.log"
export HOST_ROOT="$TMP_DIR/host"
export STATE_DIR=/state
export PVM_KERNEL_RPM_PATH="$TMP_DIR/artifacts/kernel-pvm-host.rpm"
export PVM_KERNEL_META_RPM_PATH="$TMP_DIR/artifacts/kernel-pvm-host-meta.rpm"
export PVM_KERNEL_MODULES_RPM_PATH="$TMP_DIR/artifacts/kernel-pvm-host-modules.rpm"
export PVM_KERNEL_DEVEL_RPM_PATH="$TMP_DIR/artifacts/kernel-pvm-host-devel.rpm"
export PVM_KERNEL_HEADERS_RPM_PATH="$TMP_DIR/artifacts/kernel-pvm-host-headers.rpm"
export PVM_KERNEL_DEB_PATH="$TMP_DIR/artifacts/missing.deb"
export DESIRED_KERNEL_PATTERN='cubesandbox\.\(oc9\|pvm\.host\)'
export KERNEL_BOOT_ARGS=
export REBOOT_ENABLED=false
export REBOOT_COORDINATED=true
export NODE_NAME=test-node
export POD_NAMESPACE=test-ns
export STARTUP_GATE_ENABLED=true
export STARTUP_GATE_TAINT_KEY=cube.tencent.com/pvm-not-ready
export STARTUP_GATE_TAINT_EFFECT=NoSchedule
export CUBE_RELEASE=test-release
export REBOOT_MAX_COUNT=0

if sh "$SCRIPT" >"$TMP_DIR/output.log" 2>&1; then
  echo "bootstrap unexpectedly completed without the required reboot" >&2
  exit 1
fi

if [ ! -f "$TEST_LOG" ]; then
  cat "$TMP_DIR/output.log" >&2
  exit 1
fi
grep -q 'expected kernel from packaged RPM: 6.6.117-45.16.cubesandbox.oc9.x86_64' "$TMP_DIR/output.log"
grep -q 'rpm -Uvh --oldpackage --replacepkgs' "$TEST_LOG"
grep -q "PVM_TARGET_RELEASE='6.6.117-45.16.cubesandbox.oc9.x86_64'" "$TEST_LOG"
if grep -q 'PVM_TARGET_RELEASE=.*pvm.host' "$TEST_LOG"; then
  echo "bootstrap selected the legacy pvm.host kernel" >&2
  exit 1
fi

echo "PVM host bootstrap tests passed"
