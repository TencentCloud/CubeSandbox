#!/usr/bin/env bash
# Unit tests for cube-s3lvol-entrypoint helpers and cubelet [cow.s3] opt-in patch.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="${SCRIPT_DIR}/cube-s3lvol-entrypoint.sh"
COMPONENT="${SCRIPT_DIR}/component-entrypoint.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=./cube-s3lvol-entrypoint.sh
source "${ENTRYPOINT}"

RCOW_COMMON="$(cd "${SCRIPT_DIR}/../../../.." && pwd)/CubeS3lvol/scripts/rcow_common.sh"
[[ -f "${RCOW_COMMON}" ]] || fail "missing ${RCOW_COMMON}"

cross_hash() {
  local id="$1"
  local tmp
  tmp="$(mktemp -d)"
  RCOW_LVS_NAME=entrypoint-cross-hash \
    RCOW_ACTIVE_FILE="${tmp}/active_lvols" \
    RCOW_BSTORE_FILE="${tmp}/bstore.json" \
    bash -c '
      set -euo pipefail
      # shellcheck disable=SC1090
      source "$1"
      rcow_node_hash "$2"
    ' _ "${RCOW_COMMON}" "${id}"
  rm -rf "${tmp}"
}

unset RCOW_LVS_NAME RCOW_TGT_CPUMASK
NODE_NAME=worker-1
s3lvol_export_optional_knobs
[[ "${RCOW_LVS_NAME}" == "$(s3lvol_derived_lvs_name worker-1)" ]] \
  || fail "worker-1 must derive rcow-<hash of full node name>"
[[ "${RCOW_LVS_NAME}" == "rcow-$(cross_hash worker-1)" ]] \
  || fail "entrypoint hash of worker-1 must match rcow_node_hash"
[[ -z "${RCOW_TGT_CPUMASK:-}" ]] || fail "must not invent RCOW_TGT_CPUMASK when unset"

unset RCOW_LVS_NAME
NODE_NAME=192.0.2.48
s3lvol_export_optional_knobs
from48="${RCOW_LVS_NAME}"
[[ "${from48}" == "$(s3lvol_derived_lvs_name 192.0.2.48)" ]] \
  || fail "192.0.2.48 must derive from the full node name"
[[ "${from48}" == "rcow-$(cross_hash 192.0.2.48)" ]] \
  || fail "entrypoint hash of 192.0.2.48 must match rcow_node_hash"
[[ "${from48}" != "rcow-$(cross_hash 192)" ]] \
  || fail "192.0.2.48 must not hash like hostname -s"

unset RCOW_LVS_NAME
NODE_NAME=192.0.2.44
s3lvol_export_optional_knobs
from44="${RCOW_LVS_NAME}"
[[ "${from44}" != "${from48}" ]] \
  || fail "192.0.2.48 and 192.0.2.44 must get different LVS names"
[[ "${from44}" == "rcow-$(cross_hash 192.0.2.44)" ]] \
  || fail "entrypoint hash of 192.0.2.44 must match rcow_node_hash"

unset RCOW_LVS_NAME
NODE_NAME=n1.zone-a
s3lvol_export_optional_knobs
from_zone_a="${RCOW_LVS_NAME}"
unset RCOW_LVS_NAME
NODE_NAME=n1.zone-b
s3lvol_export_optional_knobs
from_zone_b="${RCOW_LVS_NAME}"
[[ "${from_zone_a}" != "${from_zone_b}" ]] \
  || fail "n1.zone-a and n1.zone-b must get different LVS names"

RCOW_LVS_NAME=rcow-explicit
RCOW_TGT_CPUMASK=0x30
NODE_NAME=192.0.2.48
s3lvol_export_optional_knobs
[[ "${RCOW_LVS_NAME}" == "rcow-explicit" ]] || fail "must keep explicit RCOW_LVS_NAME"
[[ "${RCOW_TGT_CPUMASK}" == "0x30" ]] || fail "must keep explicit RCOW_TGT_CPUMASK"

tmp="$(mktemp)"
fn="$(mktemp)"
trap 'rm -f "${tmp}" "${fn}"' EXIT

# component-entrypoint.sh always runs main; extract the patch helper only.
python3 - "${COMPONENT}" "${fn}" <<'PY'
from pathlib import Path
import sys
text = Path(sys.argv[1]).read_text()
start = text.index("patch_cow_s3_opt_in()")
end = text.index("\ndetect_primary_interface()")
Path(sys.argv[2]).write_text(text[start:end])
PY
grep -q 'patch_cow_s3_opt_in' "${fn}" || fail "failed to extract patch_cow_s3_opt_in"

run_patch() {
  local sock="$1"
  bash -c '
    set -euo pipefail
    fail() { printf "%s\n" "$*" >&2; exit 1; }
    log() { :; }
    source "$1"
    patch_cow_s3_opt_in "$2" "$3"
  ' _ "${fn}" "${tmp}" "${sock}"
}

count_key() {
  local key="$1"
  grep -c "^[[:space:]]*${key}[[:space:]]*=" "${tmp}"
}

# Config with the section present, enable = false, image-default socket.
cat > "${tmp}" <<'EOF'
    [plugins."io.cubelet.internal.v1.storage"]
    storage_backend = "cubecow"
    [plugins."io.cubelet.internal.v1.storage".cow.log]
    level = "info"
    [plugins."io.cubelet.internal.v1.storage".cow.s3]
    enable = false
    socket_path = "/var/run/s3lvol.sock"
    [plugins."io.cubelet.internal.v1.images"]
    foo = "bar"
EOF

run_patch "/var/run/s3lvol/s3lvol.sock"
grep -Fq '[plugins."io.cubelet.internal.v1.storage".cow.s3]' "${tmp}" \
  || fail "patch must keep cow.s3 section"
grep -Fq 'enable = true' "${tmp}" \
  || fail "patch must set enable = true"
grep -Fq 'enable = false' "${tmp}" \
  && fail "patch must replace enable = false"
grep -Fq 'socket_path = "/var/run/s3lvol/s3lvol.sock"' "${tmp}" \
  || fail "patch must override socket_path to the sidecar socket"
grep -Fq 'socket_path = "/var/run/s3lvol.sock"' "${tmp}" \
  && fail "patch must not keep the default socket_path"
[[ "$(count_key enable)" -eq 1 ]] || fail "must keep a single enable (got $(count_key enable))"
[[ "$(count_key socket_path)" -eq 1 ]] || fail "must keep a single socket_path (got $(count_key socket_path))"
[[ "$(grep -c '\[plugins\."io.cubelet.internal.v1.storage"\.cow\.s3\]' "${tmp}")" -eq 1 ]] \
  || fail "must not insert a second cow.s3 table"

run_patch "/tmp/other.sock"
[[ "$(count_key enable)" -eq 1 ]] || fail "re-patch must keep a single enable"
[[ "$(count_key socket_path)" -eq 1 ]] || fail "re-patch must keep a single socket_path"
grep -Fq 'enable = true' "${tmp}" || fail "re-patch must keep enable = true"
grep -Fq 'socket_path = "/tmp/other.sock"' "${tmp}" \
  || fail "re-patch must replace socket_path"

# No [cow.s3] section: append enable + socket.
cat > "${tmp}" <<'EOF'
    [plugins."io.cubelet.internal.v1.storage"]
    storage_backend = "cubecow"
    [plugins."io.cubelet.internal.v1.storage".cow.log]
    level = "info"
EOF

run_patch "/var/run/s3lvol/s3lvol.sock"
grep -Fq '[plugins."io.cubelet.internal.v1.storage".cow.s3]' "${tmp}" \
  || fail "patch must add cow.s3 section when missing"
grep -Fq 'enable = true' "${tmp}" \
  || fail "append path must write enable = true"
grep -Fq 'socket_path = "/var/run/s3lvol/s3lvol.sock"' "${tmp}" \
  || fail "append path must write socket_path"

RCOW_START="$(cd "${SCRIPT_DIR}/../../../.." && pwd)/CubeS3lvol/scripts/rcow_start.sh"
[[ -f "${RCOW_START}" ]] || fail "missing ${RCOW_START}"
# Literal -r "${RCOW_RPC_SOCK}" so a non-default emptyDir socket is honored.
grep -Fq -- '-r "${RCOW_RPC_SOCK}"' "${RCOW_START}" \
  || fail "rcow_start.sh must pass -r \"\${RCOW_RPC_SOCK}\" to s3lvol_tgt"

echo "cube-s3lvol entrypoint helper tests passed"
