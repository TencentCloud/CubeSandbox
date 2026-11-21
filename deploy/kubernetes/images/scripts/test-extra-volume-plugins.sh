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

CFG="${TMP}/config.toml"
cat >"${CFG}" <<'TOML'
[plugins."io.cubelet.internal.v1.storage"]
    volume_plugin_base_dir = "/data/cube-shared/volume"

    [[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
      name        = "s3"
      type        = "binary"
      binary_path = "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3"

    [plugins."io.cubelet.internal.v1.storage".cow.log]
      level = "info"
TOML
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
assert names == ["s3", "myfs"], names
PY
  ok "valid TOML"
fi

for bad in 'bad name|rpc|/s.sock' 'x|nfs|/s.sock' 'x|rpc|' 'x|rpc|/a"b' 's3|binary|/opt/s3'; do
  if (patch_extra_volume_plugins "${CFG}" "${bad}") 2>/dev/null; then
    fail "accepted ${bad}"
  fi
done
ok "invalid entries rejected"
