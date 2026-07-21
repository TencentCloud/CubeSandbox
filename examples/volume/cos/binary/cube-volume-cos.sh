#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# cube-volume-cos — CubeSandbox VolumePlugin for Tencent Cloud COS
#
# What this script does (one sentence per hook):
#   create  — make an empty folder for this volume in COS (control plane)
#   destroy — delete that folder from COS (control plane)
#   attach  — mount the COS folder on the node with cosfs (data plane)
#   detach  — unmount cosfs when no sandbox uses the volume anymore
#
# CubeMaster calls create/destroy when users create/delete volumes via API.
# Cubelet calls attach/detach when sandboxes start/stop using a volume.
#
# Calling convention: one subprocess per operation.
#   cube-volume-cos --op <op> [--<key> <value> ...]
#
# Output: single JSON line to stdout; exit 0 on success, non-zero on error.
#
# Plugin config file: <plugin-dir>/volume-cos.conf (same directory as this script)
#                     (or $CUBE_COS_CONFIG)
#   SECRET_ID=AKIDxx…
#   SECRET_KEY=***
#   BUCKET=mybucket-1250000000
#   REGION=ap-guangzhou
#
# The FUSE mount path is not configured here: Cubelet passes it on attach via
# --volume-base-dir (default /data/volume) and the plugin mounts at
# <volume-base-dir>/cos-<volume_id>.
#
# chmod 600 <plugin-dir>/volume-cos.conf
#
# Dependencies (must be installed on every Cubelet node):
#   cosfs  — FUSE mount driver for COS
#     TencentOS 4.x:  rpm -ivh --nodeps cosfs-*.centos8.x86_64.rpm
#                     yum install -y compat-openssl11  # provides libcrypto.so.1.1
#     CentOS/TLinux:  rpm -ivh cosfs-*.centos8.x86_64.rpm
#                     yum install -y libxml2 fuse
#     Latest RPM:     https://github.com/tencentyun/cosfs/releases
#   coscmd — COS CLI used for volume dir init/delete (create/destroy ops)
#     python3 -m venv /opt/coscmd-venv
#     /opt/coscmd-venv/bin/pip install coscmd
#     ln -sf /opt/coscmd-venv/bin/coscmd /usr/local/bin/coscmd
#
# Mount layout (one cosfs process per volume):
#   <volume-base-dir>/cos-<volume_id>/  →  BUCKET:/volumes/<volume_id>/
#   where <volume-base-dir> is passed by Cubelet via --volume-base-dir
#   (default /data/volume). host_path MUST live inside it.
#
# Locking: per-volume flock on /run/cube-volume-cos/<volume_id>.lock
# ensures concurrent attach/detach for the same volume is serialised.

set -euo pipefail

# ---------------------------------------------------------------------------
# Config — read COS credentials from volume-cos.conf next to this script
# ---------------------------------------------------------------------------

# Where this script lives; config file sits in the same directory unless overridden.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${CUBE_COS_CONFIG:-${SCRIPT_DIR}/volume-cos.conf}"
LOCK_DIR="/run/cube-volume-cos"
PASSWD_FILE="/etc/cube/.passwd-cosfs"

# Parent directory Cubelet requires cosfs mounts to live under.
# Cubelet passes --volume-base-dir on attach (default /data/volume).
# Each volume gets its own subdir: <volume-base-dir>/cos-<volume_id>.
VOLUME_BASE_DIR="/data/volume"

# Read SECRET_ID, SECRET_KEY, BUCKET, REGION from the config file.
load_config() {
    [[ -f "$CONFIG_FILE" ]] || die "config file not found: $CONFIG_FILE"
    # shellcheck source=/dev/null
    source "$CONFIG_FILE"
    [[ -n "${SECRET_ID:-}"  ]] || die "config: SECRET_ID is empty"
    [[ -n "${SECRET_KEY:-}" ]] || die "config: SECRET_KEY is empty"
    [[ -n "${BUCKET:-}"     ]] || die "config: BUCKET is empty"
    [[ -n "${REGION:-}"     ]] || die "config: REGION is empty"
}

# Write cosfs credential file: BucketName-APPID:SecretId:SecretKey (mode 600).
# cosfs reads this file when mounting; we refresh it only when credentials change.
ensure_passwd_file() {
    mkdir -p "$(dirname "$PASSWD_FILE")"
    local content="${BUCKET}:${SECRET_ID}:${SECRET_KEY}"
    if [[ "$(cat "$PASSWD_FILE" 2>/dev/null)" != "$content" ]]; then
        printf '%s\n' "$content" > "$PASSWD_FILE"
        chmod 600 "$PASSWD_FILE"
    fi
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log()      { echo "[cube-volume-cos] $*" >&2; }
die()      { log "ERROR: $*"; exit 1; }
ok_json()  { printf '{"error":""}\n'; }
err_json() { local msg; msg="$(printf '%s' "$1" | jq -Rn 'input')"; printf '{"error":%s}\n' "$msg"; }

# Local path where this volume is mounted on the node (must stay under VOLUME_BASE_DIR).
volume_mountpoint() { echo "${VOLUME_BASE_DIR%/}/cos-$1"; }

# Object key prefix inside the bucket for this volume's data.
cos_subdir() { echo "volumes/$1"; }

# ---------------------------------------------------------------------------
# Per-volume flock: serialise concurrent attach/detach for the same volumeID.
# Usage:
#   exec {LOCK_FD}>/run/cube-volume-cos/<id>.lock
#   flock -x "$LOCK_FD"
#   ... critical section ...
#   flock -u "$LOCK_FD"
# ---------------------------------------------------------------------------

# Per-volume file lock: if two attach/detach calls run at once for the same
# volume, only one proceeds at a time (avoids double-mount or race on unmount).
volume_lock_acquire() {
    local volume_id="$1"
    mkdir -p "$LOCK_DIR"
    local lock_file="${LOCK_DIR}/${volume_id}.lock"
    # Open on fd 200 (arbitrary high fd; safe for sub-processes)
    exec 200>"$lock_file"
    flock -x 200
    log "lock acquired for volume ${volume_id}"
}

volume_lock_release() {
    flock -u 200
}

# ---------------------------------------------------------------------------
# COS backend ops — use coscmd CLI to create/delete volume folders in the bucket
# ---------------------------------------------------------------------------

_COSCMD_CFG=""

# One-time coscmd login using credentials from volume-cos.conf.
_coscmd_cfg_init() {
    coscmd config -a "$SECRET_ID" -s "$SECRET_KEY" -b "$BUCKET" -r "$REGION" \
        2>/dev/null
    _COSCMD_CFG="done"
}

coscmd_run() {
    [[ -z "$_COSCMD_CFG" ]] && _coscmd_cfg_init
    coscmd "$@"
}

# Upload a tiny .keep file so the volume folder exists in COS before first mount.
cos_create_dir() {
    local volume_id="$1"
    log "coscmd: create $(cos_subdir "$volume_id")/.keep"
    local tmpfile
    tmpfile="$(mktemp)"
    coscmd_run upload "$tmpfile" "$(cos_subdir "$volume_id")/.keep"
    rm -f "$tmpfile"
}

# Recursively delete the volume folder from COS (destroy hook).
# Only ignore explicit NotFound; other failures must propagate so Master does
# not delete the DB row while objects remain in COS.
cos_remove_dir() {
    local volume_id="$1"
    local out=""
    local rc=0
    log "coscmd: delete $(cos_subdir "$volume_id")/"
    set +e
    out="$(coscmd_run delete -r -f "$(cos_subdir "$volume_id")/" 2>&1)"
    rc=$?
    set -e
    if [[ "$rc" -eq 0 ]]; then
        return 0
    fi
    if printf '%s' "$out" | grep -qiE 'not found|NoSuchKey|does not exist|404|No such file'; then
        log "coscmd: delete ignored not-found for ${volume_id}"
        return 0
    fi
    log "ERROR: coscmd delete failed for ${volume_id}: ${out}"
    return "$rc"
}

# ---------------------------------------------------------------------------
# FUSE ops — cosfs mounts one COS subfolder per volume on the node
# ---------------------------------------------------------------------------

# Mount BUCKET:/volumes/<volume_id> at <volume-base-dir>/cos-<volume_id>.
# Safe to call twice: skips if already mounted.
cosfs_mount_volume() {
    local volume_id="$1"
    local mnt
    mnt="$(volume_mountpoint "$volume_id")"
    local endpoint="https://cos.${REGION}.myqcloud.com"

    if mountpoint -q "$mnt" 2>/dev/null; then
        log "cosfs: volume ${volume_id} already mounted at ${mnt}"
        return 0
    fi

    mkdir -p "$mnt"
    log "cosfs: mounting ${BUCKET}:/$(cos_subdir "$volume_id") -> ${mnt}"
    cosfs "${BUCKET}:/$(cos_subdir "$volume_id")" "$mnt" \
        "-ourl=${endpoint}"            \
        "-opasswd_file=${PASSWD_FILE}" \
        "-oallow_other"                \
        "-ononempty"                   \
        "-odbglevel=info"              \
        "-onoxattr"
    log "cosfs: mounted ok"
}

# Unmount the per-volume FUSE mount and remove the mountpoint dir (created at attach).
cosfs_unmount_volume() {
    local mnt="$1"

    if mountpoint -q "$mnt" 2>/dev/null; then
        log "cosfs: unmounting ${mnt}"
        fusermount -u "$mnt" 2>/dev/null || umount -l "$mnt" 2>/dev/null || true
        log "cosfs: unmounted ${mnt}"
    else
        log "cosfs: ${mnt} not mounted, skipping unmount"
    fi

    if [[ -d "$mnt" ]]; then
        rmdir "$mnt" && log "cosfs: removed mount dir ${mnt}" \
            || log "cosfs: could not remove ${mnt} (not empty?)"
    fi
}

# ---------------------------------------------------------------------------
# CubeMaster hooks (control plane — run when user creates/deletes a volume)
# ---------------------------------------------------------------------------

# create: provision backend storage for a new volume.
# Steps: load config -> create COS folder -> return empty token JSON.
#
# Input:  --volume-id <id>  --name <name>
# Output: stdout JSON {"token":"","error":""}
do_create() {
    local volume_id="$1" name="$2"
    log "create volumeID=${volume_id} name=${name}"

    load_config
    # Step 1: create volumes/<volume_id>/ in the COS bucket
    cos_create_dir "$volume_id" || { err_json "coscmd create dir failed for ${volume_id}"; exit 1; }

    # Step 2: return success (COS plugin has no extra token to hand back)
    jq -cn '{ token: "", error: "" }'
}

# destroy: remove backend storage when user deletes a volume.
# Steps: load config -> delete COS folder -> return success JSON.
#
# Input:  --volume-id <id>
# Output: stdout JSON {"error":""}
do_destroy() {
    local volume_id="$1"
    log "destroy volumeID=${volume_id}"

    load_config
    # Step 1: delete volumes/<volume_id>/ from COS (irreversible)
    cos_remove_dir "$volume_id" || {
        err_json "coscmd delete failed for ${volume_id}"
        exit 1
    }
    ok_json
}

# ---------------------------------------------------------------------------
# Cubelet hooks (data plane — run when a sandbox mounts/unmounts a volume)
# ---------------------------------------------------------------------------

# attach: make volume data visible on this node and tell Cubelet where it is.
# Steps: load config -> write cosfs passwd -> lock -> cosfs mount -> return host_path.
#
# Cubelet bind-mounts host_path into the sandbox at the user's chosen path.
#
# Input:  --sandbox-id <id>  --namespace <ns>  --volume-id <vid>
#         --ref-count <n>  --volume-base-dir <dir>
# Output: {"host_path":"<volume-base-dir>/cos-<vid>","metadata":{...},"error":""}
do_attach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"

    log "attach sandbox=${sandbox_id} volumeID=${volume_id} refcount_before=${ref_count}"

    load_config
    ensure_passwd_file

    # Step 1: serialize concurrent attach for the same volume
    volume_lock_acquire "$volume_id"
    trap 'volume_lock_release' EXIT

    # Step 2: mount COS folder with cosfs (skip if already mounted)
    cosfs_mount_volume "$volume_id" || {
        err_json "cosfs mount failed for volume ${volume_id}"
        exit 1
    }

    local mnt
    mnt="$(volume_mountpoint "$volume_id")"

    log "attach ready: host_path=${mnt}"

    # Step 3: return host_path so Cubelet can bind-mount into the sandbox
    jq -cn \
        --arg path "$mnt" \
        --arg vid  "$volume_id" \
        '{
            host_path: $path,
            metadata:  { mount_dir: $path, volume_id: $vid },
            error:     ""
        }'
}

# detach: stop exposing volume data on this node when nobody uses it.
# Steps: if ref_count>0 skip -> else lock -> unmount cosfs -> return success.
#
# ref_count is how many sandboxes on this node still use the volume after this detach.
# Only unmount when ref_count reaches 0 (last sandbox gone).
#
# Input:  --sandbox-id <id>  --namespace <ns>  --volume-id <vid>
#         --ref-count <n>  --metadata <json>
# Output: {"error":""}
do_detach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"
    local metadata_json="$4"

    log "detach sandbox=${sandbox_id} volumeID=${volume_id} refcount_after=${ref_count}"

    # Step 1: other sandboxes still mounted — leave cosfs running
    if [[ "$ref_count" -gt 0 ]]; then
        log "skipping unmount: volume still in use (refcount_after=${ref_count})"
        ok_json
        return
    fi

    load_config

    volume_lock_acquire "$volume_id"
    trap 'volume_lock_release' EXIT

    # Step 2: find mount path (prefer path saved at attach time)
    local mnt
    mnt="$(printf '%s' "$metadata_json" | jq -r '.mount_dir // empty' 2>/dev/null)"
    [[ -n "$mnt" ]] || mnt="$(volume_mountpoint "$volume_id")"

    # Step 3: last user gone — unmount FUSE (COS data stays until destroy)
    cosfs_unmount_volume "$mnt"

    log "detach done volumeID=${volume_id} (COS data preserved; delete volume to remove backend data)"
    ok_json
}

# ---------------------------------------------------------------------------
# Entry point — parse CLI flags and dispatch to the right hook
# ---------------------------------------------------------------------------

OP=""
VOLUME_ID="" NAME=""
SANDBOX_ID="" NAMESPACE="" REF_COUNT="0"
METADATA="{}"

# CubeMaster/Cubelet pass arguments like --op attach --volume-id xxx ...
while [[ $# -gt 0 ]]; do
    case "$1" in
        --op)           OP="$2";           shift 2 ;;
        --volume-id)    VOLUME_ID="$2";    shift 2 ;;
        --name)         NAME="$2";         shift 2 ;;
        --sandbox-id)   SANDBOX_ID="$2";   shift 2 ;;
        --namespace)    NAMESPACE="$2";    shift 2 ;;
        --ref-count)    REF_COUNT="$2";    shift 2 ;;
        --volume-base-dir)
            [[ -n "${2:-}" ]] && VOLUME_BASE_DIR="$2"; shift 2 ;;
        --metadata)     METADATA="$2";     shift 2 ;;
        *) die "unknown argument: $1" ;;
    esac
done

[[ -n "$OP" ]] || die "--op is required"

case "$OP" in
    # CubeMaster (control plane)
    create)  do_create  "$VOLUME_ID" "$NAME" ;;
    destroy) do_destroy "$VOLUME_ID" ;;
    # Cubelet (data plane)
    attach)  do_attach  "$SANDBOX_ID" "$VOLUME_ID" "$REF_COUNT" ;;
    detach)  do_detach  "$SANDBOX_ID" "$VOLUME_ID" "$REF_COUNT" "$METADATA" ;;
    *)       err_json "unknown op: ${OP}"; exit 1 ;;
esac
