#!/bin/sh
set -eu

log() { printf '[cube-node-init] %s\n' "$*"; }
fail() { printf '[cube-node-init] ERROR: %s\n' "$*" >&2; exit 1; }

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/node-prep-lib.sh"

HOST_ROOT="${HOST_ROOT:-/host}"
STATE_DIR="${STATE_DIR:-/var/lib/cube-node-bootstrap}"
DATA_CUBELET="${DATA_CUBELET:-/data/cubelet}"
REQUIRE_KVM="${REQUIRE_KVM:-true}"
REQUIRE_XFS="${REQUIRE_XFS:-true}"
CHMOD_KVM="${CHMOD_KVM:-true}"
LOAD_KVM_MODULE="${LOAD_KVM_MODULE:-true}"
WRITE_UDEV_RULE="${WRITE_UDEV_RULE:-true}"
CREATE_HOST_DIRS="${CREATE_HOST_DIRS:-true}"
CHECK_MASTER_CONNECTIVITY="${CHECK_MASTER_CONNECTIVITY:-true}"
CUBE_MASTER_ENDPOINT="${CUBE_MASTER_ENDPOINT:-cube-master.cube-system.svc.cluster.local:8089}"
CHECK_MEMORY="${CHECK_MEMORY:-true}"
MIN_MEMORY_KB="${MIN_MEMORY_KB:-7500000}"
CHECK_CGROUP_CPU="${CHECK_CGROUP_CPU:-true}"
CHECK_GLIBC="${CHECK_GLIBC:-true}"
CHECK_CIDR="${CHECK_CIDR:-true}"
CHECK_CUBECOW_DEPS="${CHECK_CUBECOW_DEPS:-true}"
CUBE_PVM_ENABLE="${CUBE_PVM_ENABLE:-0}"
CUBE_SANDBOX_NETWORK_CIDR="${CUBE_SANDBOX_NETWORK_CIDR:-}"
CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK="${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK:-0}"
LOOPBACK_ENABLED="${LOOPBACK_ENABLED:-false}"
LOOPBACK_IMAGE_PATH="${LOOPBACK_IMAGE_PATH:-/data/cubelet-xfs.img}"
LOOPBACK_SIZE="${LOOPBACK_SIZE:-25G}"
# When bootstrap Pod restarts and fingerprint already matches, skip
# (avoids blocking artifact-unrelated paths on Master/preflight). Default true
# for bootstrap DS; set false to force a full re-run.
SKIP_IF_NODE_PREP_READY="${SKIP_IF_NODE_PREP_READY:-true}"
NODE_INIT_ENABLED="${NODE_INIT_ENABLED:-1}"
PVM_ENABLED="${PVM_ENABLED:-${CUBE_PVM_ENABLE}}"
export HOST_ROOT STATE_DIR NODE_INIT_ENABLED PVM_ENABLED

host_path() { printf '%s%s' "$HOST_ROOT" "$1"; }

# Per-node effective-pvm (from wait-pvm-host) overrides Helm CUBE_PVM_ENABLE / PVM_ENABLED.
apply_effective_pvm_env

if [ "$SKIP_IF_NODE_PREP_READY" = "true" ] && node_prep_fingerprint_matches_file; then
  log "node-prep-ready fingerprint matches; skipping node-init"
  exit 0
fi
# SECURITY MODEL (host_chroot_sh / host_mount_sh):
#   Both helpers enter PID 1's uts/ipc/net/pid namespaces via `nsenter --target 1`
#   and chroot into /host. This is a deliberate, architecturally required
#   container escape: node bootstrap needs kernel-module loading, block
#   device operations (XFS check/create), and host filesystem preparation
#   which cannot be done from inside the container mount namespace.
#
#   Consequences to be aware of:
#   - A compromise of the cube-node-init or pvm-host-bootstrap image grants
#     unrestricted host root access. Treat these images as security-critical.
#     Restrict the registry that can push them, sign them (cosign/notary),
#     and pin `imagePullPolicy: Always` + a digest in production.
#   - The pod's ServiceAccount is separate from this escape; SYS_PTRACE +
#     hostPID on the runtime cube-node container is what governs kubelet
#     credential exposure, not these init helpers.
#   - Callers MUST quote every operator-supplied value before splicing it
#     into the `sh -c` argument to prevent command injection.
host_chroot_sh() {
  # Keep the container mount namespace so the host root bind mount remains
  # visible at $HOST_ROOT, but enter host uts/ipc/net/pid namespaces for host
  # service operations.
  nsenter --target 1 --uts --ipc --net --pid -- chroot "$HOST_ROOT" /bin/sh -c "$*"
}
host_mount_sh() {
  # Enter the host mount namespace when creating or mounting host filesystems.
  nsenter --target 1 --mount --uts --ipc --net --pid -- /bin/sh -c "$*"
}

version_ge() {
  # Returns true when $1 >= $2 for MAJOR.MINOR glibc-style versions.
  awk -v a="$1" -v b="$2" 'BEGIN {
    split(a, av, "."); split(b, bv, ".");
    if ((av[1] + 0) > (bv[1] + 0)) exit 0;
    if ((av[1] + 0) < (bv[1] + 0)) exit 1;
    exit ((av[2] + 0) >= (bv[2] + 0)) ? 0 : 1;
  }'
}

check_memory() {
  [ "$CHECK_MEMORY" = "true" ] || return 0
  mem_total_kb="$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  case "$MIN_MEMORY_KB" in
    ''|*[!0-9]*) fail "MIN_MEMORY_KB must be a positive integer: ${MIN_MEMORY_KB}" ;;
  esac
  [ "$mem_total_kb" -ge "$MIN_MEMORY_KB" ] || fail "system memory must be at least ${MIN_MEMORY_KB}KB, found ${mem_total_kb}KB"
  log "memory check passed: ${mem_total_kb}KB"
}

check_glibc() {
  [ "$CHECK_GLIBC" = "true" ] || return 0
  glibc_ver="$(ldd --version 2>&1 | awk 'NR == 1 { print $NF; exit }' || true)"
  [ -n "$glibc_ver" ] || fail "unable to detect glibc version"
  version_ge "$glibc_ver" "2.31" || fail "glibc version ${glibc_ver} is too old; requires >= 2.31"
  log "glibc check passed: ${glibc_ver}"
}

check_cgroup_cpu() {
  [ "$CHECK_CGROUP_CPU" = "true" ] || return 0
  fstype="$(stat -fc %T /sys/fs/cgroup 2>/dev/null || echo unknown)"
  [ "$fstype" = "cgroup2fs" ] || return 0
  controllers="$(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null || true)"
  echo "$controllers" | grep -qw cpu || fail "cgroup v2 does not expose cpu controller: ${controllers:-<empty>}"
  subtree="$(cat /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true)"
  if echo "$subtree" | grep -qw cpu; then
    log "cgroup v2 cpu controller check passed"
    return 0
  fi
  log "cgroup v2 cpu controller not enabled; trying to enable +cpu"
  if printf '+cpu\n' > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null; then
    log "enabled cgroup v2 +cpu"
    return 0
  fi
  fail "failed to enable cgroup v2 cpu controller on /sys/fs/cgroup/cgroup.subtree_control"
}

ip_to_int() {
  awk -v ip="$1" 'BEGIN {
    n = split(ip, a, ".");
    if (n != 4) exit 1;
    for (i = 1; i <= 4; i++) {
      if (a[i] !~ /^[0-9]+$/ || a[i] < 0 || a[i] > 255) exit 1;
    }
    printf "%.0f\n", (a[1] * 16777216) + (a[2] * 65536) + (a[3] * 256) + a[4];
  }'
}

int_to_ip() {
  awk -v n="$1" 'BEGIN {
    a = int(n / 16777216); n -= a * 16777216;
    b = int(n / 65536); n -= b * 65536;
    c = int(n / 256); d = n - c * 256;
    printf "%d.%d.%d.%d\n", a, b, c, d;
  }'
}

cidr_range() {
  cidr="$1"
  ip="${cidr%/*}"
  mask="${cidr#*/}"
  [ "$ip" != "$cidr" ] || return 1
  case "$mask" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$mask" -ge 0 ] && [ "$mask" -le 32 ] || return 1
  ip_int="$(ip_to_int "$ip")" || return 1
  host_bits=$((32 - mask))
  if [ "$host_bits" -eq 32 ]; then
    block_size=4294967296
  else
    block_size=$((1 << host_bits))
  fi
  start=$((ip_int / block_size * block_size))
  end=$((start + block_size - 1))
  printf '%s %s\n' "$start" "$end"
}

cidr_overlaps() {
  a="$1"
  b="$2"
  a_range="$(cidr_range "$a")" || return 1
  b_range="$(cidr_range "$b")" || return 1
  # shellcheck disable=SC2086
  set -- $a_range $b_range
  a_start="$1"
  a_end="$2"
  b_start="$3"
  b_end="$4"
  [ "$a_start" -le "$b_end" ] && [ "$b_start" -le "$a_end" ]
}

normalize_existing_cidr() {
  token="$1"
  case "$token" in
    default|broadcast|throw|unreachable|blackhole|prohibit|local|nat|multicast|'')
      return 1
      ;;
  esac
  case "$token" in
    */*)
      ip="${token%/*}"
      mask="${token#*/}"
      ;;
    *)
      ip="$token"
      mask=32
      ;;
  esac
  case "$mask" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$mask" -ge 0 ] && [ "$mask" -le 32 ] || return 1
  ip_to_int "$ip" >/dev/null 2>&1 || return 1
  printf '%s/%s\n' "$ip" "$mask"
}

validate_cidr() {
  cidr="$1"
  ip="${cidr%/*}"
  mask="${cidr#*/}"
  [ "$ip" != "$cidr" ] || fail "CUBE_SANDBOX_NETWORK_CIDR must be IPv4 CIDR, got ${cidr}"
  case "$mask" in
    ''|*[!0-9]*) fail "invalid CIDR mask: ${cidr}" ;;
  esac
  [ "$mask" -ge 8 ] && [ "$mask" -le 30 ] || fail "CIDR mask must be between 8 and 30: ${cidr}"
  ip_int="$(ip_to_int "$ip")" || fail "invalid CIDR IP: ${cidr}"
  host_bits=$((32 - mask))
  block_size=$((1 << host_bits))
  aligned=$((ip_int / block_size * block_size))
  if [ "$ip_int" -ne "$aligned" ]; then
    suggested="$(int_to_ip "$aligned")/${mask}"
    fail "CIDR is not aligned to network address: ${cidr}; did you mean ${suggested}"
  fi
}

check_cidr_conflict() {
  [ "$CHECK_CIDR" = "true" ] || return 0
  [ -n "$CUBE_SANDBOX_NETWORK_CIDR" ] || return 0
  validate_cidr "$CUBE_SANDBOX_NETWORK_CIDR"
  if [ "$CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK" = "1" ]; then
    log "CIDR conflict check skipped: ${CUBE_SANDBOX_NETWORK_CIDR}"
    return 0
  fi

  conflicts_file="$(mktemp)"
  {
    ip -o -4 addr show 2>/dev/null | awk '{print $4 " addr " $2}'
    ip -o -4 route show 2>/dev/null | awk '{print $1 " route " $0}'
  } | while read -r candidate kind detail; do
    existing="$(normalize_existing_cidr "$candidate" || true)"
    [ -n "$existing" ] || continue
    if cidr_overlaps "$CUBE_SANDBOX_NETWORK_CIDR" "$existing"; then
      printf -- '- %s %s %s\n' "$kind" "$existing" "$detail" >> "$conflicts_file"
    fi
  done

  if [ -s "$conflicts_file" ]; then
    conflicts="$(cat "$conflicts_file")"
    rm -f "$conflicts_file"
    fail "CUBE_SANDBOX_NETWORK_CIDR conflicts with existing host network: ${CUBE_SANDBOX_NETWORK_CIDR}
${conflicts}
Set CUBE_SANDBOX_NETWORK_CIDR to a non-overlapping private CIDR or set CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1 to bypass the conflict check."
  fi
  rm -f "$conflicts_file"
  log "CIDR check passed: ${CUBE_SANDBOX_NETWORK_CIDR}"
}

check_cubecow_deps() {
  [ "$CHECK_CUBECOW_DEPS" = "true" ] || return 0
  missing=""
  for cmd in mkfs.ext4 mount umount losetup; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      missing="${missing} ${cmd}"
    fi
  done
  [ -z "$missing" ] || fail "missing cubecow startup dependencies:${missing}"
  log "cubecow startup dependency check passed"
}

normalize_pvm_enable() {
  case "$CUBE_PVM_ENABLE" in
    1|true|TRUE|yes|YES)
      CUBE_PVM_ENABLE=1
      ;;
    0|false|FALSE|no|NO)
      CUBE_PVM_ENABLE=0
      ;;
    *)
      fail "unsupported CUBE_PVM_ENABLE=${CUBE_PVM_ENABLE}; expected 0 or 1"
      ;;
  esac
}

check_pvm_consistency() {
  normalize_pvm_enable
  command -v lsmod >/dev/null 2>&1 || fail "lsmod is required for PVM consistency check"

  has_kvm_pvm=0
  if lsmod 2>/dev/null | grep -qE '^kvm_pvm[[:space:]]'; then
    has_kvm_pvm=1
  fi

  if [ "$has_kvm_pvm" -eq 1 ] && [ "$CUBE_PVM_ENABLE" != "1" ]; then
    fail "PVM host detected because kvm_pvm is loaded, but CUBE_PVM_ENABLE=0. Set cubeNode.pvmGuestKernel.enabled=true so cube-node selects cube-kernel-scf/vmlinux-pvm."
  fi

  if [ "$has_kvm_pvm" -eq 0 ] && [ "$CUBE_PVM_ENABLE" = "1" ]; then
    fail "CUBE_PVM_ENABLE=1 requires kvm_pvm to be loaded on the host. Enable bootstrap.pvmHostKernel.enabled=true for chart-managed PVM host bootstrap, or prepare/reboot the node into a PVM host kernel before starting cube-node."
  fi

  if [ "$CUBE_PVM_ENABLE" = "1" ]; then
    log "PVM consistency check passed: kvm_pvm loaded and CUBE_PVM_ENABLE=1"
  else
    log "PVM consistency check passed: ordinary guest kernel mode"
  fi
}

check_memory
check_glibc
check_cgroup_cpu
check_cidr_conflict
check_cubecow_deps

if [ "$LOAD_KVM_MODULE" = "true" ]; then
  log "loading kvm_pvm module"
  modprobe kvm_pvm 2>/dev/null || host_chroot_sh "modprobe kvm_pvm" 2>/dev/null || true
fi

if [ "$REQUIRE_KVM" = "true" ]; then
  [ -e /dev/kvm ] || fail "/dev/kvm does not exist"
  log "/dev/kvm check passed"
fi

check_pvm_consistency

if [ "$CHMOD_KVM" = "true" ] && [ -e /dev/kvm ]; then
  chmod 666 /dev/kvm || true
fi

if [ "$WRITE_UDEV_RULE" = "true" ]; then
  mkdir -p "$(host_path /etc/udev/rules.d)"
  printf 'KERNEL=="kvm", MODE="0666"\n' > "$(host_path /etc/udev/rules.d/99-kvm.rules)"
fi

if [ "$CREATE_HOST_DIRS" = "true" ]; then
  log "creating host directories"
  mkdir -p \
    "$(host_path /data/cubelet)" \
    "$(host_path /data/log)" \
    "$(host_path /data/cube-shim)" \
    "$(host_path /data/snapshot_pack)" \
    "$(host_path /tmp/cube)"
fi

if ! xfs_info "$DATA_CUBELET" >/dev/null 2>&1; then
  if [ "$LOOPBACK_ENABLED" = "true" ]; then
    log "initializing loopback XFS at ${LOOPBACK_IMAGE_PATH} size=${LOOPBACK_SIZE}"
    host_mount_sh "mkdir -p ${DATA_CUBELET}; if [ ! -f ${LOOPBACK_IMAGE_PATH} ]; then truncate -s ${LOOPBACK_SIZE} ${LOOPBACK_IMAGE_PATH}; mkfs.xfs -f -m reflink=1 ${LOOPBACK_IMAGE_PATH}; fi; mountpoint -q ${DATA_CUBELET} || mount -o loop,pquota ${LOOPBACK_IMAGE_PATH} ${DATA_CUBELET}; grep -q '${LOOPBACK_IMAGE_PATH} ${DATA_CUBELET}' /etc/fstab || echo '${LOOPBACK_IMAGE_PATH} ${DATA_CUBELET} xfs loop,pquota 0 0' >> /etc/fstab"
  fi
fi

if [ "$REQUIRE_XFS" = "true" ]; then
  xfs_info "$DATA_CUBELET" >/dev/null 2>&1 || fail "${DATA_CUBELET} is not an XFS mount"
  log "${DATA_CUBELET} XFS check passed"
fi

if [ "$CHECK_MASTER_CONNECTIVITY" = "true" ]; then
  command -v curl >/dev/null 2>&1 || fail "curl is required in cube-node-init image"
  log "checking CubeMaster connectivity: ${CUBE_MASTER_ENDPOINT}"
  curl -fsS "http://${CUBE_MASTER_ENDPOINT}/notify/health" >/dev/null || fail "CubeMaster is unreachable at ${CUBE_MASTER_ENDPOINT}"
  log "cube-master connectivity check passed"
fi

log "cube node init completed"
