#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# cube-volume-s3 — CubeSandbox VolumePlugin for any S3-compatible object storage
# (AWS S3, Tencent Cloud COS, MinIO, Cloudflare R2, Ceph RGW, ...)
#
# What this script does (one sentence per hook):
#   create  — make an empty folder for this volume in the bucket (control plane)
#   destroy — delete that folder from the bucket (control plane)
#   attach  — mount the bucket folder on the node with s3fs (data plane)
#   detach  — unmount s3fs when no sandbox uses the volume anymore
#
# CubeMaster calls create/destroy when users create/delete volumes via API.
# Cubelet calls attach/detach when sandboxes start/stop using a volume.
#
# Calling convention: one subprocess per operation.
#   cube-volume-s3 --op <op> [--<key> <value> ...]
#
# Output: single JSON line to stdout; exit 0 on success, non-zero on error.
#
# Plugin config file: <plugin-dir>/volume-s3.conf (same directory as this script)
#                     (or $CUBE_S3_CONFIG)
#   ACCESS_KEY_ID=...
#   SECRET_ACCESS_KEY=...
#   BUCKET=my-bucket
#   ENDPOINT=https://<your-s3-endpoint>   # required; see volume-s3.conf.example
#   REGION=us-east-1                      # optional, default us-east-1
#   S3FS_EXTRA_OPTS="-ouse_path_request_style"   # optional, appended to s3fs
#
# Path overrides (defaults are correct for a normal root-run deployment; these
# exist so the plugin can be exercised without root, e.g. in CI):
#   CUBE_S3_CONFIG       config file path
#   CUBE_S3_PASSWD_FILE  s3fs credential file  (default /etc/cube/.passwd-s3fs-volume-<bucket>)
#   CUBE_S3_LOCK_DIR     attach/detach locks   (default /run/cube-volume-s3)
#
# The FUSE mount path is not configured here: Cubelet passes it on attach via
# --volume-base-dir (default /data/cube-shared/volume) and the plugin mounts at
# <volume-base-dir>/s3-<volume_id>.
#
# The config file holds a plaintext secret: keep it root-owned and chmod 600.
#
# Dependencies:
#   s3fs  — FUSE mount driver, required on every Cubelet node (attach/detach)
#     Debian/Ubuntu:  apt-get install -y s3fs
#     RHEL/CentOS:    yum install -y s3fs-fuse   # needs EPEL
#     Both amd64 and arm64 are packaged by the distros — no manual download.
#   aws   — AWS CLI v2, required on CubeMaster (create/destroy)
#     Works against any S3-compatible endpoint via --endpoint-url.
#   jq    — JSON output, required on both CubeMaster and Cubelet
#
# Mount layout (one s3fs process per volume):
#   <volume-base-dir>/s3-<volume_id>/  →  BUCKET:/volumes/<volume_id>/
#   where <volume-base-dir> is passed by Cubelet via --volume-base-dir
#   (default /data/cube-shared/volume). host_path MUST live inside it.
#
# Locking: per-volume flock on /run/cube-volume-s3/<volume_id>.lock
# ensures concurrent attach/detach for the same volume is serialised.

set -euo pipefail

# ---------------------------------------------------------------------------
# Config — read S3 credentials from volume-s3.conf next to this script
# ---------------------------------------------------------------------------

# Where this script lives; config file sits in the same directory unless overridden.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${CUBE_S3_CONFIG:-${SCRIPT_DIR}/volume-s3.conf}"
# Per-volume attach/detach lock directory.
LOCK_DIR="${CUBE_S3_LOCK_DIR:-/run/cube-volume-s3}"

DEFAULT_REGION="us-east-1"

# Parent directory Cubelet requires s3fs mounts to live under.
# Cubelet passes --volume-base-dir on attach (default /data/cube-shared/volume).
# Each volume gets its own subdir: <volume-base-dir>/s3-<volume_id>.
VOLUME_BASE_DIR="/data/cube-shared/volume"

# Read ACCESS_KEY_ID, SECRET_ACCESS_KEY, BUCKET, ENDPOINT (+ optional REGION).
load_config() {
    [[ -f "$CONFIG_FILE" ]] || die "config file not found: $CONFIG_FILE"
    # shellcheck source=/dev/null
    source "$CONFIG_FILE"
    [[ -n "${ACCESS_KEY_ID:-}"     ]] || die "config: ACCESS_KEY_ID is empty"
    [[ -n "${SECRET_ACCESS_KEY:-}" ]] || die "config: SECRET_ACCESS_KEY is empty"
    [[ -n "${BUCKET:-}"            ]] || die "config: BUCKET is empty"
    [[ -n "${ENDPOINT:-}"          ]] || die "config: ENDPOINT is empty (set your S3-compatible endpoint URL)"
    REGION="${REGION:-$DEFAULT_REGION}"
    # MinIO and other path-style endpoints need AWS CLI path-style addressing
    # (virtual-hosted-style breaks against http://minio:9000). Also disable
    # default CRC32 checksums that AWS CLI v2.23+ sends and older MinIO rejects.
    if [[ "${ADDRESSING_STYLE:-}" == "path" || "${S3FS_EXTRA_OPTS:-}" == *path_request_style* ]]; then
        mkdir -p /etc/cube
        AWS_CONFIG_FILE="${CUBE_S3_AWS_CONFIG:-/etc/cube/aws-config-volume-s3-${BUCKET}}"
        printf '[default]\ns3 =\n  addressing_style = path\n' > "${AWS_CONFIG_FILE}"
        chmod 600 "${AWS_CONFIG_FILE}"
        export AWS_CONFIG_FILE
    fi
    export AWS_REQUEST_CHECKSUM_CALCULATION="${AWS_REQUEST_CHECKSUM_CALCULATION:-WHEN_REQUIRED}"
    export AWS_RESPONSE_CHECKSUM_VALIDATION="${AWS_RESPONSE_CHECKSUM_VALIDATION:-WHEN_REQUIRED}"
}

# Write the s3fs credential file: BUCKET:AccessKeyId:SecretAccessKey (mode 600).
# s3fs reads this file when mounting; we refresh it only when credentials change.
# The path is per-bucket so several plugin instances (different driver names,
# different buckets) on one node never race on a shared credential file.
# Call after load_config — the filename derives from BUCKET.
ensure_passwd_file() {
    PASSWD_FILE="${CUBE_S3_PASSWD_FILE:-/etc/cube/.passwd-s3fs-volume-${BUCKET}}"
    mkdir -p "$(dirname "$PASSWD_FILE")"
    local content="${BUCKET}:${ACCESS_KEY_ID}:${SECRET_ACCESS_KEY}"
    if [[ "$(cat "$PASSWD_FILE" 2>/dev/null)" != "$content" ]]; then
        printf '%s\n' "$content" > "$PASSWD_FILE"
        chmod 600 "$PASSWD_FILE"
    fi
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log()      { echo "[cube-volume-s3] $*" >&2; }
die()      { log "ERROR: $*"; exit 1; }
ok_json()  { printf '{"error":""}\n'; }
err_json() { local msg; msg="$(printf '%s' "$1" | jq -Rn 'input')"; printf '{"error":%s}\n' "$msg"; }

# Local path where this volume is mounted on the node (must stay under VOLUME_BASE_DIR).
volume_mountpoint() { echo "${VOLUME_BASE_DIR%/}/s3-$1"; }

# Object key prefix inside the bucket for this volume's data.
s3_subdir() { echo "volumes/$1"; }

# ---------------------------------------------------------------------------
# Per-volume flock: serialise concurrent attach/detach for the same volumeID.
# Usage:
#   exec {LOCK_FD}>/run/cube-volume-s3/<id>.lock
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
# S3 backend ops — use the AWS CLI to create/delete volume folders
# ---------------------------------------------------------------------------

# Run the AWS CLI against the configured endpoint. Credentials come from the
# plugin config, not from the host's ~/.aws, so CubeMaster needs no AWS
# profile set up. AWS_EC2_METADATA_DISABLED stops the SDK from probing the
# instance metadata service when a call fails, which otherwise adds a
# multi-second hang per op.
aws_run() {
    AWS_ACCESS_KEY_ID="$ACCESS_KEY_ID" \
    AWS_SECRET_ACCESS_KEY="$SECRET_ACCESS_KEY" \
    AWS_DEFAULT_REGION="$REGION" \
    AWS_REGION="$REGION" \
    AWS_EC2_METADATA_DISABLED=true \
    aws --endpoint-url "$ENDPOINT" "$@"
}

# Create the bucket on first use when the S3-compatible store does not have it
# yet (typical for bundled MinIO). Existing buckets are left untouched.
s3_ensure_bucket() {
    local out rc=0
    local region="${REGION:-$DEFAULT_REGION}"
    set +e
    out="$(aws_run s3api head-bucket --bucket "$BUCKET" 2>&1)"
    rc=$?
    set -e
    if [[ "$rc" -eq 0 ]]; then
        return 0
    fi
    log "aws: creating bucket ${BUCKET}"
    local create_args=(s3api create-bucket --bucket "$BUCKET")
    # AWS us-east-1 rejects LocationConstraint; R2 uses REGION=auto.
    if [[ -n "$region" && "$region" != "us-east-1" && "$region" != "auto" ]]; then
        create_args+=(--create-bucket-configuration "LocationConstraint=${region}")
    fi
    set +e
    out="$(aws_run "${create_args[@]}" 2>&1)"
    rc=$?
    set -e
    if [[ "$rc" -ne 0 ]]; then
        # Concurrent first creates: the loser sees already-exists, not a real failure.
        if printf '%s' "$out" | grep -qiE 'BucketAlreadyOwnedByYou|BucketAlreadyExists'; then
            log "aws: bucket ${BUCKET} already exists"
            return 0
        fi
        log "ERROR: aws create-bucket failed for ${BUCKET}: ${out}"
        return 1
    fi
    return 0
}

# PUT the trailing-slash directory object s3fs uses for mkdir (key "dir/").
# Object storage has no real directories; s3fs 1.91+ stats this key on mount
# of bucket:/volumes/<id>. A sibling ".keep" file is not that key.
s3_create_dir() {
    local volume_id="$1"
    local key empty out rc=0
    key="$(s3_subdir "$volume_id")/"
    log "aws: create ${key}"
    # AWS CLI 2.36+ requires --body to be a regular file (/dev/null is a
    # character device and is rejected). A 0-byte temp file still sends an
    # empty HTTP body, which some S3-compatible gateways require instead of
    # a body-less PUT.
    empty="$(mktemp)"
    : > "$empty"
    set +e
    out="$(aws_run s3api put-object \
        --bucket "$BUCKET" \
        --key "$key" \
        --body "$empty" 2>&1)"
    rc=$?
    set -e
    rm -f "$empty"
    if [[ "$rc" -ne 0 ]]; then
        log "ERROR: aws put-object failed for ${volume_id}: ${out}"
        return 1
    fi
    return 0
}

# Recursively delete the volume folder (destroy hook).
# Only ignore explicit NotFound; other failures must propagate so Master does
# not delete the DB row while objects remain in the bucket.
s3_remove_dir() {
    local volume_id="$1"
    local out=""
    local rc=0
    log "aws: delete $(s3_subdir "$volume_id")/"
    set +e
    out="$(aws_run s3 rm "s3://${BUCKET}/$(s3_subdir "$volume_id")/" --recursive 2>&1)"
    rc=$?
    set -e
    if [[ "$rc" -eq 0 ]]; then
        return 0
    fi
    if printf '%s' "$out" | grep -qiE 'NoSuchKey|NoSuchBucket|does not exist|Not Found|404'; then
        log "aws: delete ignored not-found for ${volume_id}"
        return 0
    fi
    log "ERROR: aws delete failed for ${volume_id}: ${out}"
    return "$rc"
}

# ---------------------------------------------------------------------------
# FUSE ops — s3fs mounts one bucket prefix per volume on the node
# ---------------------------------------------------------------------------

# Mount BUCKET:/volumes/<volume_id> at <volume-base-dir>/s3-<volume_id>.
# Safe to call twice: skips if already mounted.
s3fs_mount_volume() {
    local volume_id="$1"
    local mnt
    mnt="$(volume_mountpoint "$volume_id")"

    if mountpoint -q "$mnt" 2>/dev/null; then
        log "s3fs: volume ${volume_id} already mounted at ${mnt}"
        return 0
    fi

    mkdir -p "$mnt"
    log "s3fs: mounting ${BUCKET}:/$(s3_subdir "$volume_id") -> ${mnt}"

    # Optional extra s3fs options from volume-s3.conf, whitespace-separated,
    # e.g. S3FS_EXTRA_OPTS="-ouse_path_request_style" for MinIO or for bucket
    # names containing dots.
    local extra_opts=()
    if [[ -n "${S3FS_EXTRA_OPTS:-}" ]]; then
        read -r -a extra_opts <<< "$S3FS_EXTRA_OPTS"
    fi

    local out rc=0
    set +e
    # -o endpoint      : region used for SigV4 signing
    # -o url           : the S3-compatible endpoint from volume-s3.conf
    # -o allow_other   : Cubelet (a different user) must traverse the mount to
    #                    bind it into the microVM via virtiofs
    # -o nonempty      : mountpoint may already hold a stale dir entry
    # Create already PUT volumes/<id>/ (the same object s3fs mkdir would write).
    out="$(s3fs "${BUCKET}:/$(s3_subdir "$volume_id")" "$mnt" \
        "-ourl=${ENDPOINT}"             \
        "-oendpoint=${REGION}"          \
        "-opasswd_file=${PASSWD_FILE}"  \
        "-oallow_other"                 \
        "-ononempty"                    \
        ${extra_opts[@]+"${extra_opts[@]}"} 2>&1)"
    rc=$?
    set -e
    # s3fs can exit 0 even when auth fails; require a real mountpoint.
    if [[ "$rc" -ne 0 ]] || ! mountpoint -q "$mnt" 2>/dev/null; then
        log "ERROR: s3fs mount failed for ${volume_id}: ${out}"
        rmdir "$mnt" 2>/dev/null || true
        return 1
    fi
    log "s3fs: mounted ok"
}

# Unmount the per-volume FUSE mount and remove the mountpoint dir (created at attach).
s3fs_unmount_volume() {
    local mnt="$1"

    if mountpoint -q "$mnt" 2>/dev/null; then
        log "s3fs: unmounting ${mnt}"
        fusermount -u "$mnt" 2>/dev/null || umount -l "$mnt" 2>/dev/null || true
        log "s3fs: unmounted ${mnt}"
    else
        log "s3fs: ${mnt} not mounted, skipping unmount"
    fi

    if [[ -d "$mnt" ]]; then
        rmdir "$mnt" && log "s3fs: removed mount dir ${mnt}" \
            || log "s3fs: could not remove ${mnt} (not empty?)"
    fi
}

# ---------------------------------------------------------------------------
# CubeMaster hooks (control plane — run when user creates/deletes a volume)
# ---------------------------------------------------------------------------

# create: provision backend storage for a new volume.
# Steps: load config -> create bucket folder -> return token/private_data JSON.
#
# Input:  --volume-id <id>  --name <name>
# Output: stdout JSON {"token":"","private_data":"volumes/<id>/","error":""}
#
# private_data is opaque Create→Attach state (max 1024 bytes). This plugin
# stores the object-key prefix so Attach can log/reuse it without hardcoding.
do_create() {
    local volume_id="$1" name="$2"
    log "create volumeID=${volume_id} name=${name}"

    load_config
    # Step 1: ensure the bucket exists, then create volumes/<volume_id>/
    s3_ensure_bucket || { err_json "aws create bucket failed for ${BUCKET}"; exit 1; }
    s3_create_dir "$volume_id" || { err_json "aws create dir failed for ${volume_id}"; exit 1; }

    # Step 2: return success; private_data carries the key prefix for Attach
    jq -cn --arg pd "volumes/${volume_id}/" \
        '{ token: "", private_data: $pd, error: "" }'
}

# destroy: remove backend storage when user deletes a volume.
# Steps: load config -> delete bucket folder -> return success JSON.
#
# Input:  --volume-id <id>
# Output: stdout JSON {"error":""}
do_destroy() {
    local volume_id="$1"
    log "destroy volumeID=${volume_id}"

    load_config
    # Step 1: delete volumes/<volume_id>/ from the bucket (irreversible)
    s3_remove_dir "$volume_id" || {
        err_json "aws delete failed for ${volume_id}"
        exit 1
    }
    ok_json
}

# ---------------------------------------------------------------------------
# Cubelet hooks (data plane — run when a sandbox mounts/unmounts a volume)
# ---------------------------------------------------------------------------

# attach: make volume data visible on this node and tell Cubelet where it is.
# Steps: load config -> write s3fs passwd -> lock -> s3fs mount -> return host_path.
#
# Cubelet bind-mounts host_path into the sandbox at the user's chosen path.
#
# Input:  --sandbox-id <id>  --namespace <ns>  --volume-id <vid>
#         --ref-count <n>  --volume-base-dir <dir>  [--private-data <str>]
# Output: {"host_path":"<volume-base-dir>/s3-<vid>","metadata":{...},"error":""}
do_attach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"
    local private_data="${4:-}"

    log "attach sandbox=${sandbox_id} volumeID=${volume_id} refcount_before=${ref_count} private_data=${private_data}"

    load_config
    ensure_passwd_file

    # Step 1: serialize concurrent attach for the same volume
    volume_lock_acquire "$volume_id"
    trap 'volume_lock_release' EXIT

    # Step 2: mount the bucket prefix with s3fs (skip if already mounted)
    s3fs_mount_volume "$volume_id" || {
        err_json "s3fs mount failed for volume ${volume_id}"
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
# Steps: if ref_count>0 skip -> else lock -> unmount s3fs -> return success.
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

    # Step 1: other sandboxes still mounted — leave s3fs running
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

    # Step 3: last user gone — unmount FUSE (bucket data stays until destroy)
    s3fs_unmount_volume "$mnt"

    log "detach done volumeID=${volume_id} (bucket data preserved; delete volume to remove backend data)"
    ok_json
}

# ---------------------------------------------------------------------------
# Entry point — parse CLI flags and dispatch to the right hook
# ---------------------------------------------------------------------------

OP=""
VOLUME_ID="" NAME=""
SANDBOX_ID="" NAMESPACE="" REF_COUNT="0"
METADATA="{}"
PRIVATE_DATA=""

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
        --private-data) PRIVATE_DATA="$2"; shift 2 ;;
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
    attach)  do_attach  "$SANDBOX_ID" "$VOLUME_ID" "$REF_COUNT" "$PRIVATE_DATA" ;;
    detach)  do_detach  "$SANDBOX_ID" "$VOLUME_ID" "$REF_COUNT" "$METADATA" ;;
    *)       err_json "unknown op: ${OP}"; exit 1 ;;
esac
