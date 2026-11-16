#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit checks for set_cubelet_log_level in component-entrypoint.sh (no
# container required): replace an existing common.log_level, insert one when
# the shipped conf.yaml has none, reject unknown names (Cubelet's
# StringToLevel would silently turn them into DEBUG) and leave the file alone
# when no level is requested.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${SCRIPT_DIR}/component-entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Load entrypoint helpers without executing main.
# shellcheck disable=SC1090
source <(sed '/^main "$@"/d' "${ENTRY}")

assert_level() { # <file> <want> <msg>
  local got
  got="$(awk '/^common:/{c=1; next} /^[^ ]/{c=0} c && /^  log_level:/{print $2}' "$1")"
  if [[ "${got}" != "$2" ]]; then
    printf 'FAIL: %s (got=%q want=%q)\n' "$3" "${got}" "$2" >&2
    exit 1
  fi
  if [[ "$(awk '/^common:/{c=1; next} /^[^ ]/{c=0} c && /^  log_level:/' "$1" | wc -l | tr -d ' ')" != 1 ]]; then
    printf 'FAIL: %s (common.log_level appears more than once)\n' "$3" >&2
    exit 1
  fi
  printf 'ok: %s\n' "$3"
}

cat > "${TMP}/with.yaml" <<'EOF'
common:
  log_level: info
  enable_pf_mode: false
network:
  log_level: keep-me
EOF
set_cubelet_log_level "${TMP}/with.yaml" warn >/dev/null 2>&1
assert_level "${TMP}/with.yaml" warn "existing common.log_level is replaced"
grep -q '^  log_level: keep-me$' "${TMP}/with.yaml" && echo "ok: other sections untouched"

cat > "${TMP}/without.yaml" <<'EOF'
common:
  enable_pf_mode: false
EOF
set_cubelet_log_level "${TMP}/without.yaml" debug >/dev/null 2>&1
assert_level "${TMP}/without.yaml" debug "missing common.log_level is inserted"

cat > "${TMP}/last.yaml" <<'EOF2'
network:
  log_level: keep-me
common:
  enable_pf_mode: false
EOF2
set_cubelet_log_level "${TMP}/last.yaml" error >/dev/null 2>&1
assert_level "${TMP}/last.yaml" error "common as the last section gets log_level"
grep -q '^  log_level: keep-me$' "${TMP}/last.yaml" && echo "ok: earlier sections untouched"

cp "${TMP}/with.yaml" "${TMP}/unchanged.yaml"
set_cubelet_log_level "${TMP}/unchanged.yaml" "" >/dev/null 2>&1
cmp -s "${TMP}/with.yaml" "${TMP}/unchanged.yaml" && echo "ok: empty level leaves conf.yaml unchanged"

rc=0
out="$( (set_cubelet_log_level "${TMP}/with.yaml" verbose) 2>&1 )" || rc=$?
if (( rc == 0 )) || [[ "${out}" != *"must be debug, info, warn or error"* ]]; then
  printf 'FAIL: unknown level accepted (rc=%s out=%q)\n' "${rc}" "${out}" >&2
  exit 1
fi
assert_level "${TMP}/with.yaml" warn "unknown level is rejected and the file is not modified"
