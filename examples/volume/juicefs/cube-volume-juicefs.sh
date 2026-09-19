#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# cube-volume-juicefs — CubeSandbox VolumePlugin for JuiceFS
#
# What this script does (one sentence per hook):
#   create  — make an empty directory volumes/<volume_id> in the JuiceFS file system (control plane)
#   destroy — delete that directory (control plane)
#   attach  — mount volumes/<volume_id> on the node with `juicefs mount --subdir` (data plane)
#   detach  — unmount it when no sandbox on this node uses the volume anymore
#
# Why JuiceFS: it is a POSIX file system whose data lives in object storage
# (S3, COS, OSS, GCS, …) and whose metadata lives in Redis / PostgreSQL / MySQL /
# TiKV. Listing directories, many small files, git and file locks work at
# near-local speed, and several sandboxes (on one or many nodes) see each
# other's writes immediately.
#
# CubeMaster calls create/destroy when users create/delete volumes via API.
# Cubelet calls attach/detach when sandboxes start/stop using a volume.
#
# Calling convention: one subprocess per operation.
#   cube-volume-juicefs --op <op> [--<key> <value> ...]
#
# Output: single JSON line to stdout; exit 0 on success, non-zero on error.
#
# Plugin config file: <plugin-dir>/volume-juicefs.conf (same directory as this script)
#                     (or $CUBE_JUICEFS_CONFIG)
#   META_URL='postgres://juicefs@10.0.0.5:5432/juicefs?sslmode=disable'
#   META_PASSWORD='***'            # optional; keeps the password out of `ps`
#   CACHE_DIR='/data/juicefs-cache' # optional; local read cache shared by all volumes
#   CACHE_SIZE='102400'             # optional; MiB
#   MOUNT_OPTS=''                   # optional; extra `juicefs mount` flags
#
# chmod 600 <plugin-dir>/volume-juicefs.conf
#
# Object storage credentials are NOT configured here: they are stored in the
# file system metadata by `juicefs format`. Format without --access-key /
# --secret-key to use the node's cloud identity (e.g. an EC2 instance role).
#
# One-time setup (any machine that can reach the metadata engine):
#   juicefs format "$META_URL" <fs-name> --storage s3 \
#       --bucket https://<bucket>.s3.<region>.amazonaws.com
#
# Dependencies (every CubeMaster and Cubelet node):
#   juicefs — https://github.com/juicedata/juicefs/releases
#   fuse, jq, flock (util-linux)
#
# Mount layout (one juicefs process per volume on a node):
#   <volume-base-dir>/juicefs-<volume_id>/  →  <fs>:/volumes/<volume_id>/
#   where <volume-base-dir> is passed by Cubelet via --volume-base-dir
#   (default /data/cube-shared/volume). host_path MUST live inside it.
#   The sandbox sees only its volume's directory, never the file system root.
#
# Control plane mount: create/destroy need the file system root, so they keep
# one long-lived mount at $ADMIN_MOUNT on the CubeMaster node. It also runs
# JuiceFS background jobs (trash cleanup, compaction); per-volume mounts pass
# --no-bgjob.
#
# Locking: per-volume flock on /run/cube-volume-juicefs/<volume_id>.lock
# ensures concurrent attach/detach for the same volume is serialised.

set -euo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${CUBE_JUICEFS_CONFIG:-${SCRIPT_DIR}/volume-juicefs.conf}"
LOCK_DIR="/run/cube-volume-juicefs"
LOG_DIR="/var/log/cube-volume-juicefs"
ADMIN_MOUNT="${CUBE_JUICEFS_ADMIN_MOUNT:-/run/cube-volume-juicefs/admin}"

# Cubelet passes --volume-base-dir on attach (default /data/cube-shared/volume).
VOLUME_BASE_DIR="/data/cube-shared/volume"

load_config() {
    [[ -f "$CONFIG_FILE" ]] || die "config file not found: $CONFIG_FILE"
    # shellcheck source=/dev/null
    source "$CONFIG_FILE"
    [[ -n "${META_URL:-}" ]] || die "config: META_URL is empty"
    # juicefs reads the metadata password from this variable when it is not in META_URL.
    [[ -n "${META_PASSWORD:-}" ]] && export META_PASSWORD
    CACHE_DIR="${CACHE_DIR:-/var/jfsCache}"
    CACHE_SIZE="${CACHE_SIZE:-102400}"
    MOUNT_OPTS="${MOUNT_OPTS:-}"
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log()      { echo "[cube-volume-juicefs] $*" >&2; }
die()      { log "ERROR: $*"; err_json "$*"; exit 1; }
ok_json()  { printf '{"error":""}\n'; }
err_json() { local msg; msg="$(printf '%s' "$1" | jq -Rn 'input')"; printf '{"error":%s}\n' "$msg"; }

# volume_id becomes a path component on the node and inside the file system;
# reject anything that could escape its directory.
# Matches what CubeAPI and CubeMaster accept, so a volume they created can
# always be attached here. The two traversal names are still refused, and the
# id is only ever used as one path component, never as an argument.
validate_volume_id() {
    [[ "$1" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || die "invalid volume id: $1"
    [[ "$1" != "." && "$1" != ".." ]] || die "invalid volume id: $1"
}

volume_mountpoint() { echo "${VOLUME_BASE_DIR%/}/juicefs-$1"; }
volume_subdir()     { echo "volumes/$1"; }

# Bounded on purpose: a lock still held by a killed invocation (the
# daemonized juicefs inherits the fd) would otherwise make every later
# attach and detach for this volume block forever, taking the sandbox
# create or destroy down with it. A timeout turns that into a plain error.
LOCK_WAIT_SECONDS="${LOCK_WAIT_SECONDS:-60}"

volume_lock_acquire() {
    mkdir -p "$LOCK_DIR"
    exec 200>"${LOCK_DIR}/$1.lock"
    flock -x -w "$LOCK_WAIT_SECONDS" 200 || die "timed out waiting for the lock on volume $1"
}

volume_lock_release() { flock -u 200; }

# Mount the file system root for create/destroy. Safe to call repeatedly.
ensure_admin_mount() {
    mkdir -p "$LOCK_DIR" "$LOG_DIR"
    exec 201>"${LOCK_DIR}/admin.lock"
    flock -x -w "$LOCK_WAIT_SECONDS" 201 || die "timed out waiting for the admin mount lock"
    if ! mountpoint -q "$ADMIN_MOUNT" 2>/dev/null; then
        mkdir -p "$ADMIN_MOUNT"
        log "juicefs: mounting file system root at ${ADMIN_MOUNT}"
        # shellcheck disable=SC2086
        # 200>&- 201>&-: the daemon must not inherit the lock descriptors. A
        # flock is held by the open file description, so an inherited one
        # outlives this process and would wedge the volume if we are killed
        # before the EXIT trap runs.
        juicefs mount -d "$META_URL" "$ADMIN_MOUNT" \
            --cache-dir "$CACHE_DIR" --cache-size "$CACHE_SIZE" \
            --no-usage-report --log "${LOG_DIR}/admin.log" $MOUNT_OPTS >&2 200>&- 201>&- \
            || { flock -u 201; return 1; }
        mountpoint -q "$ADMIN_MOUNT" || { flock -u 201; return 1; }
    fi
    flock -u 201
}

# ---------------------------------------------------------------------------
# CubeMaster hooks (control plane)
# ---------------------------------------------------------------------------

# Input:  --volume-id <id>  --name <name>
# Output: {"token":"","private_data":"volumes/<id>/","error":""}
do_create() {
    local volume_id="$1" name="$2"
    log "create volumeID=${volume_id} name=${name}"
    validate_volume_id "$volume_id"
    load_config
    ensure_admin_mount || die "juicefs admin mount failed"

    mkdir -p "${ADMIN_MOUNT}/$(volume_subdir "$volume_id")" \
        || die "create directory failed for ${volume_id}"

    jq -cn --arg pd "$(volume_subdir "$volume_id")/" '{ token: "", private_data: $pd, error: "" }'
}

# Input:  --volume-id <id>
# Output: {"error":""}
do_destroy() {
    local volume_id="$1"
    log "destroy volumeID=${volume_id}"
    validate_volume_id "$volume_id"
    load_config
    ensure_admin_mount || die "juicefs admin mount failed"

    local dir
    dir="${ADMIN_MOUNT}/$(volume_subdir "$volume_id")"
    if [[ -e "$dir" ]]; then
        # rmr deletes through metadata in one call instead of walking every file.
        juicefs rmr "$dir" >&2 || die "delete failed for ${volume_id}"
    else
        # "Not there" is only believable when the parent is readable. A dead or
        # hung mount makes every stat fail, and reporting success then would have
        # CubeMaster delete the row while the objects stay in the bucket.
        [[ -d "${ADMIN_MOUNT}/volumes" ]] || die "cannot read ${ADMIN_MOUNT}/volumes; refusing to report ${volume_id} as deleted"
        log "destroy: ${dir} does not exist, nothing to delete"
    fi
    ok_json
}

# ---------------------------------------------------------------------------
# Cubelet hooks (data plane)
# ---------------------------------------------------------------------------

# Input:  --sandbox-id <id>  --namespace <ns>  --volume-id <vid>
#         --ref-count <n>  --volume-base-dir <dir>  [--private-data <str>]
# Output: {"host_path":"<volume-base-dir>/juicefs-<vid>","metadata":{...},"error":""}
do_attach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"
    log "attach sandbox=${sandbox_id} volumeID=${volume_id} refcount_before=${ref_count}"
    validate_volume_id "$volume_id"
    load_config

    volume_lock_acquire "$volume_id"
    trap 'volume_lock_release' EXIT

    local mnt
    mnt="$(volume_mountpoint "$volume_id")"
    if mountpoint -q "$mnt" 2>/dev/null; then
        log "juicefs: volume ${volume_id} already mounted at ${mnt}"
    else
        mkdir -p "$mnt" "$LOG_DIR"
        log "juicefs: mounting $(volume_subdir "$volume_id") -> ${mnt}"
        # shellcheck disable=SC2086
        if ! juicefs mount -d "$META_URL" "$mnt" \
                --subdir "$(volume_subdir "$volume_id")" \
                --cache-dir "$CACHE_DIR" --cache-size "$CACHE_SIZE" \
                --no-bgjob --no-usage-report \
                --log "${LOG_DIR}/${volume_id}.log" $MOUNT_OPTS >&2 200>&- 201>&- \
            || ! mountpoint -q "$mnt" 2>/dev/null; then
            rmdir "$mnt" 2>/dev/null || true
            die "juicefs mount failed for volume ${volume_id}"
        fi
    fi

    jq -cn --arg path "$mnt" --arg vid "$volume_id" \
        '{ host_path: $path, metadata: { mount_dir: $path, volume_id: $vid }, error: "" }'
}

# Input:  --sandbox-id <id>  --namespace <ns>  --volume-id <vid>
#         --ref-count <n>  --metadata <json>
# Output: {"error":""}
do_detach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3" metadata_json="$4"
    log "detach sandbox=${sandbox_id} volumeID=${volume_id} refcount_after=${ref_count}"
    validate_volume_id "$volume_id"

    if [[ "$ref_count" -gt 0 ]]; then
        log "skipping unmount: volume still in use (refcount_after=${ref_count})"
        ok_json
        return
    fi

    # Detach does not need META_URL, but it does need to know whether the mount
    # was made with --writeback and where its cache is.
    load_config

    volume_lock_acquire "$volume_id"
    trap 'volume_lock_release' EXIT

    local mnt
    mnt="$(printf '%s' "$metadata_json" | jq -r '.mount_dir // empty' 2>/dev/null)"
    [[ -n "$mnt" ]] || mnt="$(volume_mountpoint "$volume_id")"

    if mountpoint -q "$mnt" 2>/dev/null; then
        # --flush (juicefs >= 1.4.0) waits for buffered writes to reach object
        # storage before unmounting. If it fails, say so: with --writeback the
        # data is still only in CACHE_DIR, and a lazy unmount does not wait.
        if ! juicefs umount --flush "$mnt" >&2; then
            case "$MOUNT_OPTS" in
                *--writeback*) die "flush failed for ${volume_id}; data may still be in ${CACHE_DIR}" ;;
            esac
            log "detach: flush failed for ${volume_id}, falling back to a lazy unmount"
            umount -l "$mnt" 2>/dev/null || log "detach: lazy unmount also failed for ${mnt}"
        fi
    fi
    [[ -d "$mnt" ]] && { rmdir "$mnt" 2>/dev/null || log "could not remove ${mnt} (not empty?)"; }

    log "detach done volumeID=${volume_id} (data preserved until destroy)"
    ok_json
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

OP=""
VOLUME_ID="" NAME=""
SANDBOX_ID="" REF_COUNT="0"
METADATA="{}"

# --namespace and --private-data are accepted but not needed: the volume's
# directory is derived from volume_id alone.
while [[ $# -gt 0 ]]; do
    case "$1" in
        --op)           OP="$2";           shift 2 ;;
        --volume-id)    VOLUME_ID="$2";    shift 2 ;;
        --name)         NAME="$2";         shift 2 ;;
        --sandbox-id)   SANDBOX_ID="$2";   shift 2 ;;
        --namespace)                       shift 2 ;;
        --ref-count)    REF_COUNT="$2";    shift 2 ;;
        --volume-base-dir)
            [[ -n "${2:-}" ]] && VOLUME_BASE_DIR="$2"; shift 2 ;;
        --private-data)                    shift 2 ;;
        --metadata)     METADATA="$2";     shift 2 ;;
        *) die "unknown argument: $1" ;;
    esac
done

[[ -n "$OP" ]] || die "--op is required"

case "$OP" in
    create)  do_create  "$VOLUME_ID" "$NAME" ;;
    destroy) do_destroy "$VOLUME_ID" ;;
    attach)  do_attach  "$SANDBOX_ID" "$VOLUME_ID" "$REF_COUNT" ;;
    detach)  do_detach  "$SANDBOX_ID" "$VOLUME_ID" "$REF_COUNT" "$METADATA" ;;
    *)       die "unknown op: ${OP}" ;;
esac
