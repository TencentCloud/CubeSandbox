#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
ASSET_DIR="${CUBEVZ_BENCH_ASSET_DIR:-${REPO_ROOT}/_output/cube-vz/benchmark-guest}"
RESULT_DIR="${CUBEVZ_BENCH_RESULT_DIR:-${REPO_ROOT}/_output/cube-vz/benchmark-results}"
VM_DIR="${CUBEVZ_BENCH_VM_DIR:-${REPO_ROOT}/.workdir/cube-vz/benchmark-vm}"
VCPUS="${CUBEVZ_BENCH_VCPUS:-2}"
MEMORY_MIB="${CUBEVZ_BENCH_MEMORY_MIB:-2048}"
CUBEVZ="${REPO_ROOT}/_output/bin/cube-vz"

now_ns() {
  python3 -c 'import time; print(time.time_ns())'
}

elapsed_ms() {
  python3 - "$1" "$2" <<'PY'
import sys
print(f"{(int(sys.argv[2]) - int(sys.argv[1])) / 1_000_000:.3f}")
PY
}

if [[ "${CUBEVZ_BENCH_SKIP_BUILD:-0}" != 1 ]]; then
  make -C "${REPO_ROOT}" cube-vz >/dev/null
fi

if [[ "${CUBEVZ_BENCH_REBUILD_GUEST:-0}" == 1 || ! -s "${ASSET_DIR}/rootfs.raw" ]]; then
  "${SCRIPT_DIR}/build-guest.sh"
fi

rm -rf "${VM_DIR}"
mkdir -p "${RESULT_DIR}" "$(dirname "${VM_DIR}")"

timestamp="$(date -u +'%Y%m%dT%H%M%SZ')"
console_log="${RESULT_DIR}/${timestamp}-console.log"
report="${RESULT_DIR}/${timestamp}-report.md"

create_start="$(now_ns)"
"${CUBEVZ}" create \
  --vm-dir "${VM_DIR}" \
  --kernel "${ASSET_DIR}/kernel" \
  --initrd "${ASSET_DIR}/initrd" \
  --disk "${ASSET_DIR}/rootfs.raw" \
  --cpus "${VCPUS}" \
  --memory-mib "${MEMORY_MIB}" \
  --cmdline "console=hvc0 root=/dev/vda rw rootfstype=ext4 modules=ext4,virtio_blk,virtio_pci init=/usr/local/sbin/cube-vz-bench-init"
create_end="$(now_ns)"
create_ms="$(elapsed_ms "${create_start}" "${create_end}")"

run_start="$(now_ns)"
set +e
"${CUBEVZ}" run --vm-dir "${VM_DIR}" 2>&1 | tr -d '\r' | tee "${console_log}"
run_status=${PIPESTATUS[0]}
set -e
run_end="$(now_ns)"
run_ms="$(elapsed_ms "${run_start}" "${run_end}")"

if [[ ${run_status} -ne 0 ]]; then
  echo "ERROR: cube-vz exited with status ${run_status}; console: ${console_log}" >&2
  exit "${run_status}"
fi
if ! grep -q '^CUBEVZ_BENCH_END$' "${console_log}"; then
  echo "ERROR: guest benchmark did not emit its completion marker: ${console_log}" >&2
  exit 1
fi

host_model="$(sysctl -n hw.model)"
host_cpu="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || true)"
host_memory_bytes="$(sysctl -n hw.memsize)"

{
  echo "# CubeVZ M4 benchmark"
  echo
  echo "- Timestamp (UTC): ${timestamp}"
  echo "- Host model: ${host_model}"
  echo "- Host CPU: ${host_cpu:-Apple Silicon}"
  echo "- Host memory bytes: ${host_memory_bytes}"
  echo "- macOS: $(sw_vers -productVersion) ($(sw_vers -buildVersion))"
  echo "- Guest vCPUs: ${VCPUS}"
  echo "- Guest memory MiB: ${MEMORY_MIB}"
  echo "- VM directory create: ${create_ms} ms"
  echo "- VM run including CPU, memory, and file-I/O workloads: ${run_ms} ms"
  echo
  echo '```text'
  sed -n '/^CUBEVZ_BENCH_BEGIN/,/^CUBEVZ_BENCH_END/p' "${console_log}"
  echo '```'
} >"${report}"

echo "CubeVZ benchmark completed"
echo "  create_ms=${create_ms}"
echo "  run_ms=${run_ms}"
echo "  report=${report}"
echo "  console=${console_log}"
