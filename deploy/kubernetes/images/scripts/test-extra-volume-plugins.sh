#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit checks for patch_extra_volume_plugins in component-entrypoint.sh (the
# cubelet container's entrypoint) (no
# container required): blocks are appended, rewritten idempotently across
# restarts, removed when the list shrinks, and bad entries are rejected.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${SCRIPT_DIR}/component-entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Load only the function under test; the entrypoint itself needs a node.
# shellcheck disable=SC1090
source <(awk '/^patch_extra_volume_plugins\(\) \{/,/^}/' "${ENTRY}")

# The shipped Cubelet config, including the cow.* tables after volume_plugins.
CFG="${TMP}/config.toml"
cp "${SCRIPT_DIR}/../../../../Cubelet/config/config.toml" "${CFG}"
ORIGINAL="$(cat "${CFG}")"

ok() { printf 'ok: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
count() { grep -c -- "$1" "${CFG}" || true; }

MYFS='myfs|rpc|/data/cube-shared/run/myfs/node.sock'

patch_extra_volume_plugins "${CFG}" "${MYFS}"
[[ "$(count 'name = "myfs"')" == 1 ]] || fail "myfs block appended once"
grep -q 'socket_path = "/data/cube-shared/run/myfs/node.sock"' "${CFG}" || fail "rpc uses socket_path"
ok "rpc plugin appended"

patch_extra_volume_plugins "${CFG}" "${MYFS}"
patch_extra_volume_plugins "${CFG}" "${MYFS}"
[[ "$(count 'name = "myfs"')" == 1 ]] || fail "restart must not duplicate the block"
ok "idempotent across restarts"

patch_extra_volume_plugins "${CFG}" "${MYFS};tool|binary|/opt/plugins/tool"
[[ "$(count 'name = "tool"')" == 1 ]] || fail "binary plugin appended"
grep -q 'binary_path = "/opt/plugins/tool"' "${CFG}" || fail "binary uses binary_path"
ok "second plugin appended"

patch_extra_volume_plugins "${CFG}" ""
[[ "$(cat "${CFG}")" == "${ORIGINAL}" ]] || fail "empty list restores the packaged file exactly"
ok "removed plugins disappear"

if command -v python3 >/dev/null && python3 -c 'import tomllib' 2>/dev/null; then
  patch_extra_volume_plugins "${CFG}" "${MYFS}"
  python3 - "${CFG}" <<'PY' || fail "patched file is valid TOML with both plugins"
import sys, tomllib
cfg = tomllib.load(open(sys.argv[1], "rb"))
names = [p["name"] for p in cfg["plugins"]["io.cubelet.internal.v1.storage"]["volume_plugins"]]
assert names == ["s3", "cos", "myfs"], names
PY
  ok "valid TOML"
fi

patch_extra_volume_plugins "${CFG}" ""
bad_inputs=(
  'bad name|rpc|/s.sock'
  'x|nfs|/s.sock'
  'x|rpc|'
  'x|rpc|/a"b'
  'x|rpc|/a b'
  'x|rpc|/data/a\form'
  'x|rpc|/data/a\tb'
  'x|rpc|/data/a\qb'
  'x|rpc|/data/a\'
  $'x|rpc|/data/a\tb'
  $'x|rpc|/data/a\ny|rpc|/data/b'
  $'x|rpc|/data/a\r'
  's3|binary|/opt/s3'
  'cos|rpc|/data/cos.sock'
  'x|rpc|/data/a;x|rpc|/data/b'
  'x|rpc|/data/a;y|rpc|/data/a\b'
)
for bad in "${bad_inputs[@]}"; do
  if (patch_extra_volume_plugins "${CFG}" "${bad}") 2>/dev/null; then
    fail "accepted $(printf '%q' "${bad}")"
  fi
  [[ "$(cat "${CFG}")" == "${ORIGINAL}" ]] || fail "rejected $(printf '%q' "${bad}") must leave config.toml unchanged"
done
ok "invalid entries rejected, config.toml untouched"

out="$( (patch_extra_volume_plugins "${CFG}" 'x|rpc|/data/a;x|rpc|/data/b') 2>&1 || true)"
if [[ "${out}" == *'listed twice'* ]]; then
  ok "duplicate reported as a duplicate"
else
  fail "duplicate must be reported as listed twice"
fi
out="$( (patch_extra_volume_plugins "${CFG}" 'cos|rpc|/data/a') 2>&1 || true)"
if [[ "${out}" == *'packaged plugin'* ]]; then
  ok "packaged name reported as packaged"
else
  fail "cos must be reported as a packaged plugin"
fi

# A name key outside volume_plugins must not count as a packaged plugin.
printf '\n[plugins."io.cubelet.internal.v1.other"]\n  name = "myfs"\n' >>"${CFG}"
patch_extra_volume_plugins "${CFG}" "${MYFS}" 2>/dev/null || fail "unrelated name key must not collide"
ok "collision check scoped to volume_plugins"
