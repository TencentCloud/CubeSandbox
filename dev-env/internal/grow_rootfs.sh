#!/usr/bin/env bash

set -euo pipefail

LOG_TAG="grow_rootfs"

if [[ -t 1 && -t 2 ]]; then
  LOG_COLOR_RESET=$'\033[0m'
  LOG_COLOR_INFO=$'\033[0;36m'
  LOG_COLOR_SUCCESS=$'\033[0;32m'
  LOG_COLOR_WARN=$'\033[0;33m'
  LOG_COLOR_ERROR=$'\033[0;31m'
else
  LOG_COLOR_RESET=""
  LOG_COLOR_INFO=""
  LOG_COLOR_SUCCESS=""
  LOG_COLOR_WARN=""
  LOG_COLOR_ERROR=""
fi

_log() {
  local color="$1"
  local level="$2"
  shift 2
  printf '%s[%s][%s]%s %s\n' \
    "${color}" "${LOG_TAG}" "${level}" "${LOG_COLOR_RESET}" "$*"
}

log_info()    { _log "${LOG_COLOR_INFO}"    "INFO"  "$@"; }
log_success() { _log "${LOG_COLOR_SUCCESS}" "OK"    "$@"; }
log_warn()    { _log "${LOG_COLOR_WARN}"    "WARN"  "$@" >&2; }
log_error()   { _log "${LOG_COLOR_ERROR}"   "ERROR" "$@" >&2; }

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "Missing required command: $1"
    exit 1
  fi
}

get_sysfs_block_name() {
  basename "$(readlink -f "/sys/class/block/$(basename "$1")")"
}

get_partition_number() {
  local block_name=""
  local partition_file=""

  block_name="$(get_sysfs_block_name "$1")"
  partition_file="/sys/class/block/${block_name}/partition"

  if [[ -r "${partition_file}" ]]; then
    tr -d '[:space:]' <"${partition_file}"
    return 0
  fi

  lsblk -no PARTNUM "$1" 2>/dev/null | tr -d '[:space:]'
}

get_parent_block_name() {
  local block_name=""
  local sysfs_path=""

  block_name="$(get_sysfs_block_name "$1")"
  sysfs_path="$(readlink -f "/sys/class/block/${block_name}")"

  if [[ -r "/sys/class/block/${block_name}/partition" ]]; then
    basename "$(dirname "${sysfs_path}")"
    return 0
  fi

  lsblk -no PKNAME "$1" 2>/dev/null | tr -d '[:space:]'
}

grow_partition_if_needed() {
  local device_path="$1"
  local device_label="$2"
  local part_num=""
  local parent_name=""
  local parent_disk=""
  local grow_output=""

  part_num="$(get_partition_number "${device_path}")"
  if [[ -z "${part_num}" ]]; then
    log_info "${device_label} ${device_path} is not a partition, skipping growpart"
    return
  fi

  parent_name="$(get_parent_block_name "${device_path}")"
  if [[ -z "${parent_name}" ]]; then
    log_error "Cannot determine parent disk for ${device_path}"
    exit 1
  fi

  parent_disk="/dev/${parent_name}"
  log_info "Growing partition ${device_path} to the end of disk ${parent_disk}"

  if grow_output="$(growpart "${parent_disk}" "${part_num}" 2>&1)"; then
    [[ -n "${grow_output}" ]] && log_info "${grow_output}"
    log_success "Partition ${device_path} grown"
  else
    if [[ "${grow_output}" == *"NOCHANGE:"* ]]; then
      log_warn "${grow_output}"
      log_info "Partition already fills the disk, continuing to grow the filesystem"
    else
      log_error "${grow_output}"
      exit 1
    fi
  fi

  partprobe "${parent_disk}" || true
}

grow_root_filesystem() {
  local filesystem="$1"
  local device_path="$2"

  case "${filesystem}" in
    xfs)
      need_cmd xfs_growfs
      log_info "Online-growing XFS root filesystem"
      xfs_growfs /
      log_success "XFS root filesystem grown"
      ;;
    ext4|ext3|ext2)
      need_cmd resize2fs
      log_info "Growing ext root filesystem"
      resize2fs "${device_path}"
      log_success "ext root filesystem grown"
      ;;
    *)
      log_error "Unsupported root filesystem type: ${filesystem}"
      exit 1
      ;;
  esac
}

if [[ "${EUID}" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo bash "$0" "$@"
  fi

  log_error "This script must be run as root."
  exit 1
fi

need_cmd findmnt
need_cmd lsblk
need_cmd growpart
need_cmd partprobe

root_source="$(readlink -f "$(findmnt -no SOURCE /)")"
root_fstype="$(findmnt -no FSTYPE /)"
root_type="$(lsblk -no TYPE "${root_source}" | tr -d '[:space:]')"

log_info "Current root device     : ${root_source}"
log_info "Current root filesystem : ${root_fstype}"

if [[ "${root_type}" == "lvm" ]]; then
  need_cmd lvs
  need_cmd pvs
  need_cmd pvresize
  need_cmd lvextend

  lv_path="${root_source}"
  vg_name="$(lvs --noheadings -o vg_name "${lv_path}" | xargs)"
  pv_path="$(pvs --noheadings -o pv_name --select "vg_name=${vg_name}" | awk 'NF { print $1; exit }')"

  if [[ -z "${pv_path}" ]]; then
    log_error "Cannot locate a physical volume for root volume ${lv_path}"
    exit 1
  fi

  grow_partition_if_needed "${pv_path}" "Physical volume"

  log_info "Resizing LVM physical volume ${pv_path}"
  pvresize "${pv_path}"
  log_success "Physical volume resized"

  log_info "Extending root logical volume and filesystem"
  lvextend -r -l +100%FREE "${lv_path}"
  log_success "Root logical volume extended"
else
  part_path="${root_source}"
  grow_partition_if_needed "${part_path}" "Root device"
  grow_root_filesystem "${root_fstype}" "${part_path}"
fi

log_success "Root disk expansion finished, current usage:"
df -h /
