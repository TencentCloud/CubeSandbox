#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
# Check the shared volume and show all active JuiceFS client sessions.

set -euo pipefail
PREFIX="${CUBE_INSTALL_PREFIX:-/usr/local/services/cubetoolbox}"
ENV_FILE="${PREFIX}/.one-click.env"

log() { printf '[cube-jfs-check] %s\n' "$*"; }
die() { printf '[cube-jfs-check] ERROR: %s\n' "$*" >&2; exit 1; }
[[ "$(id -u)" -eq 0 ]] || die "run as root"
[[ -f "${ENV_FILE}" ]] || die "missing ${ENV_FILE}"
set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a
[[ "${ONE_CLICK_DEPLOY_ROLE:-control}" != "compute" ]] || die "run this script on the control node"
[[ "${ONE_CLICK_ENABLE_JFS:-0}" == "1" ]] || die "set ONE_CLICK_ENABLE_JFS=1 in ${ENV_FILE}"
if [[ -z "${CUBE_JFS_META_URL:-}" && -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
  CUBE_JFS_META_URL="redis://${CUBE_SANDBOX_NODE_IP}:6380/0"
  log "using metadata URL derived from CUBE_SANDBOX_NODE_IP"
fi
: "${CUBE_JFS_META_URL:?set CUBE_JFS_META_URL in ${ENV_FILE}}"
if [[ -n "${CUBE_JFS_META_PASSWORD:-}" \
    && "${CUBE_JFS_META_URL}" =~ ^[A-Za-z][A-Za-z0-9+.-]*://[^/@]*:[^/@]+@ ]]; then
  die "do not set both CUBE_JFS_META_PASSWORD and a password in CUBE_JFS_META_URL"
fi

image="${CUBE_JFS_IMAGE:-juicedata/mount:ce-v1.4.1}"
mount_path="${CUBE_JFS_MOUNT_PATH:-/data/cubelet/jfs}"
systemctl is-active --quiet cube-sandbox-jfs.service \
  || die "local cube-sandbox-jfs.service is not active"
findmnt -rn -T "${mount_path}" -o FSTYPE | grep -q '^fuse\.juicefs$' \
  || die "${mount_path} is not a JuiceFS mount"
probe="${mount_path}/.cube-jfs-check-$$"
status_json="$(mktemp)"
trap 'rm -f "${probe:-}" "${status_json:-}"' EXIT
printf 'ok\n' > "${probe}"
grep -qx ok "${probe}" || die "shared mount read/write check failed"
log "local mount read/write check passed (${mount_path})"
meta_env_args=()
if [[ -n "${CUBE_JFS_META_PASSWORD:-}" ]]; then
  export META_PASSWORD="${CUBE_JFS_META_PASSWORD}"
  meta_env_args+=(--env META_PASSWORD)
fi
docker run --rm "${meta_env_args[@]}" --entrypoint juicefs "${image}" \
  status "${CUBE_JFS_META_URL}" >"${status_json}"

command -v python3 >/dev/null 2>&1 || die "python3 is required to display JuiceFS status"
python3 - "${status_json}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as handle:
    raw = handle.read()

# JuiceFS normally writes logs to stderr and JSON to stdout.  Be tolerant of
# versions that prefix stdout with informational messages.
start = raw.find("{")
if start < 0:
    raise SystemExit("[cube-jfs-check] ERROR: JuiceFS status returned no JSON")
try:
    status, _ = json.JSONDecoder().raw_decode(raw[start:])
except json.JSONDecodeError as exc:
    raise SystemExit(f"[cube-jfs-check] ERROR: invalid JuiceFS status JSON: {exc}")

setting = status.get("Setting") or {}
sessions = status.get("Sessions") or []
stat = status.get("Stat") or {}

print("\nJuiceFS volume")
print(f"  Name:          {setting.get('Name', '-')}")
print(f"  Storage:       {setting.get('Storage', '-')}")
print(f"  Bucket:        {setting.get('Bucket', '-')}")
print(f"  Active clients: {len(sessions)}")
print(f"  Used space:    {stat.get('UsedSpace', '-')} bytes")

print("\nActive client sessions")
if not sessions:
    print("  (none)")
else:
    headers = ("SID", "CLIENT HOST", "REPORTED ADDRESSES", "MOUNT PATH", "MOUNT TIME")
    rows = []
    for session in sessions:
        rows.append((
            str(session.get("Sid", "-")),
            str(session.get("HostName", "-")),
            ",".join(session.get("IPAddrs") or []) or "-",
            str(session.get("MountPoint", "-")),
            str(session.get("MountTime", "-")),
        ))
    widths = [max(len(headers[i]), *(len(row[i]) for row in rows)) for i in range(len(headers))]
    print("  " + "  ".join(headers[i].ljust(widths[i]) for i in range(len(headers))))
    print("  " + "  ".join("-" * width for width in widths))
    for row in rows:
        print("  " + "  ".join(row[i].ljust(widths[i]) for i in range(len(headers))))

print("\nNote: CLIENT HOST and REPORTED ADDRESSES are reported by the JuiceFS mount")
print("container. With Docker bridge networking, the address is a container address,")
print("not necessarily the node management IP.")
PY

log "verify that the active client count matches the expected mounted nodes"
