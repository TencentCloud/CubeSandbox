#!/usr/bin/env bash
# Build cube_gauge.ko against KDIR (the guest kernel *tree*, not a vmlinux ELF)
# and pack it into a 2MiB no-journal ext4.
#
# Required:
#   KDIR     kernel tree that produced the guest vmlinux
#   OUTPUT   destination .ext4 path
# Optional:
#   PAIR_VMLINUX  refuse to pack if md5(KDIR/vmlinux) != md5(PAIR_VMLINUX)
#   METADATA      write a json sidecar with md5 + vermagic
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="${MODULE_DIR:-${SCRIPT_DIR}/cube_gauge}"
IMAGE_BYTES="${IMAGE_BYTES:-$((2 * 1024 * 1024))}"

log() { printf '[cube-gauge] %s\n' "$*"; }
die() { printf '[cube-gauge] ERROR: %s\n' "$*" >&2; exit 1; }

KDIR="${KDIR:-}"
OUTPUT="${OUTPUT:-}"
PAIR_VMLINUX="${PAIR_VMLINUX:-}"
METADATA="${METADATA:-}"

[[ -n "${KDIR}" ]] || die "KDIR is required"
[[ -n "${OUTPUT}" ]] || die "OUTPUT is required"
KDIR="$(cd "${KDIR}" && pwd)"
[[ -d "${KDIR}" ]] || die "KDIR is not a directory: ${KDIR}"
OUTPUT="$(mkdir -p "$(dirname "${OUTPUT}")" && cd "$(dirname "${OUTPUT}")" && pwd)/$(basename "${OUTPUT}")"
[[ -f "${MODULE_DIR}/Makefile" ]] || die "missing module source ${MODULE_DIR}"
command -v mkfs.ext4 >/dev/null 2>&1 || die "mkfs.ext4 not found"
command -v make >/dev/null 2>&1 || die "make not found"

if [[ -n "${PAIR_VMLINUX}" ]]; then
  [[ -e "${PAIR_VMLINUX}" ]] || die "PAIR_VMLINUX not found: ${PAIR_VMLINUX}"
  PAIR_VMLINUX="$(cd "$(dirname "${PAIR_VMLINUX}")" && pwd)/$(basename "${PAIR_VMLINUX}")"
  if [[ -f "${KDIR}/vmlinux" ]]; then
    ksum="$(md5sum "${KDIR}/vmlinux" | awk '{print $1}')"
    psum="$(md5sum "${PAIR_VMLINUX}" | awk '{print $1}')"
    if [[ "${ksum}" != "${psum}" ]]; then
      die "KDIR vmlinux md5 ${ksum} != PAIR_VMLINUX ${psum}; refusing to mismatch .ko"
    fi
  fi
fi

if [[ -f "${KDIR}/Makefile" ]]; then
  make -C "${KDIR}" modules_prepare >/dev/null 2>&1 || true
fi

log "KDIR=${KDIR}"
log "OUTPUT=${OUTPUT}"
make -C "${MODULE_DIR}" KDIR="${KDIR}" clean >/dev/null 2>&1 || true
make -C "${MODULE_DIR}" KDIR="${KDIR}"
KO="${MODULE_DIR}/cube_gauge.ko"
[[ -s "${KO}" ]] || die "build produced no ${KO}"

vermagic=""
if command -v modinfo >/dev/null 2>&1; then
  vermagic="$(modinfo -F vermagic "${KO}" 2>/dev/null || true)"
fi
if [[ -z "${vermagic}" ]]; then
  vermagic="$(strings "${KO}" 2>/dev/null | awk '/^[[:alnum:].-]+ SMP / { print; exit }' || true)"
fi
[[ -n "${vermagic}" ]] && log "vermagic=${vermagic}"

work="$(mktemp -d)"
cleanup() {
  rm -rf "${work}"
  make -C "${MODULE_DIR}" KDIR="${KDIR}" clean >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${work}/root"
install -m 0644 "${KO}" "${work}/root/cube_gauge.ko"
truncate -s "${IMAGE_BYTES}" "${work}/cube_gauge.ext4"
mkfs.ext4 -F -b 4096 -O ^has_journal -d "${work}/root" "${work}/cube_gauge.ext4" >/dev/null
mkdir -p "$(dirname "${OUTPUT}")"
cp -f "${work}/cube_gauge.ext4" "${OUTPUT}"

vmlinux_md5=""
if [[ -n "${PAIR_VMLINUX}" ]]; then
  vmlinux_md5="$(md5sum "${PAIR_VMLINUX}" | awk '{print $1}')"
elif [[ -f "${KDIR}/vmlinux" ]]; then
  vmlinux_md5="$(md5sum "${KDIR}/vmlinux" | awk '{print $1}')"
fi
ext4_md5="$(md5sum "${OUTPUT}" | awk '{print $1}')"
log "installed ${OUTPUT} md5=${ext4_md5}"

if [[ -n "${METADATA}" ]]; then
  mkdir -p "$(dirname "${METADATA}")"
  python3 - "${METADATA}" "${vmlinux_md5}" "${ext4_md5}" "${vermagic}" "${KDIR}" <<'PY'
import json, os, sys
path, vmlinux_md5, ext4_md5, vermagic, kdir = sys.argv[1:6]
meta = {
    "schema_version": 1,
    "vmlinux_md5": vmlinux_md5,
    "cube_gauge_ext4_md5": ext4_md5,
    "vermagic": vermagic,
    "kdir": kdir,
}
with open(path, "w", encoding="utf-8") as f:
    json.dump(meta, f, indent=2, sort_keys=True)
    f.write("\n")
print(f"wrote {path}")
PY
fi
