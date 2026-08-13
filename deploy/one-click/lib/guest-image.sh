#!/usr/bin/env bash
#
# Guest rootfs image helpers shared by build-guest-image.sh and build-vm-assets.sh.
# Requires lib/common.sh already sourced, plus:
#   ROOT_DIR, CUBE_VERSION, CUBE_INIT_BIN_OVERRIDE / CUBE_INIT_BUILD_MODE
#   CUBE_AGENT_BIN_OVERRIDE, CUBE_AGENT_BUILD_MODE (for cube-agent.ext4)
#   GUEST_IMAGE_WORK_DIR, GUEST_ROOTFS_DIR, GUEST_ROOTFS_TAR
#   GUEST_IMAGE_DOCKERFILE, GUEST_IMAGE_CONTEXT_DIR, GUEST_IMAGE_REF, GUEST_IMAGE_VERSION
# This file is a sourced library; do not set shell options here.

find_built_binary() {
  local base_dir="$1"
  local name="$2"
  local path
  path="$(python3 - "$base_dir" "$name" <<'PY'
import os
import sys

base_dir = sys.argv[1]
name = sys.argv[2]
matches = []
for root, _, files in os.walk(base_dir):
    for file_name in files:
        if file_name != name:
            continue
        path = os.path.join(root, file_name)
        if os.access(path, os.X_OK):
            matches.append(path)
matches.sort(key=lambda p: os.path.getmtime(p))
print(matches[-1] if matches else "")
PY
)"
  [[ -n "${path}" ]] || die "failed to locate built binary '${name}' under ${base_dir}"
  printf '%s\n' "${path}"
}

copy_binary_with_deps() {
  local src_bin="$1"
  local dst_path="$2"
  local dst_root="$3"
  local loader
  local dep
  local ldd_output

  mkdir -p "${dst_root}$(dirname "${dst_path}")"
  cp -L "${src_bin}" "${dst_root}${dst_path}" 2>/dev/null || {
    require_cmd sudo
    sudo cp -L "${src_bin}" "${dst_root}${dst_path}"
  }
  chmod +x "${dst_root}${dst_path}" 2>/dev/null || {
    require_cmd sudo
    sudo chmod +x "${dst_root}${dst_path}"
  }

  ldd_output="$(ldd "${src_bin}" 2>/dev/null || true)"
  while IFS= read -r dep; do
    [[ -n "${dep}" ]] || continue
    mkdir -p "${dst_root}$(dirname "${dep}")"
    cp -L "${dep}" "${dst_root}${dep}" 2>/dev/null || {
      require_cmd sudo
      sudo cp -L "${dep}" "${dst_root}${dep}"
    }
  done < <(printf '%s\n' "${ldd_output}" | awk '
    /=> \// { print $3 }
    /^\// { print $1 }
  ' | sort -u)

  loader="$(printf '%s\n' "${ldd_output}" | awk '/ld-linux|ld-musl/ { print $1 }' | tail -n 1)"
  if [[ -n "${loader}" && -f "${loader}" ]]; then
    mkdir -p "${dst_root}$(dirname "${loader}")"
    cp -L "${loader}" "${dst_root}${loader}" 2>/dev/null || {
      require_cmd sudo
      sudo cp -L "${loader}" "${dst_root}${loader}"
    }
  fi
}

build_cube_agent() {
  if [[ -n "${CUBE_AGENT_BIN_OVERRIDE}" ]]; then
    ensure_file "${CUBE_AGENT_BIN_OVERRIDE}"
    log "using prebuilt cube-agent: ${CUBE_AGENT_BIN_OVERRIDE}"
    printf '%s\n' "${CUBE_AGENT_BIN_OVERRIDE}"
    return 0
  fi

  case "${CUBE_AGENT_BUILD_MODE}" in
    local)
      require_cmd make
      log "building cube-agent via make"
      (cd "${ROOT_DIR}/agent" && make) >&2
      ;;
    docker)
      require_cmd make
      require_cmd docker
      log "building cube-agent via make all-docker"
      (cd "${ROOT_DIR}/agent" && make all-docker) >&2
      ;;
    *)
      die "unsupported ONE_CLICK_CUBE_AGENT_BUILD_MODE: ${CUBE_AGENT_BUILD_MODE}"
      ;;
  esac

  find_built_binary "${ROOT_DIR}/agent/target" "cube-agent"
}

ensure_mkfs_ext4_supports_populate_dir() {
  local help_text
  help_text="$(mkfs.ext4 -h 2>&1 || true)"
  [[ "${help_text}" == *"-d "* || "${help_text}" == *"-d"* ]] || \
    die "mkfs.ext4 does not support -d; e2fsprogs is too old for guest image creation"
}

directory_size_bytes() {
  local dir_path="$1"
  python3 - "$dir_path" <<'PY'
import os
import sys

total = 0
for root, dirs, files in os.walk(sys.argv[1]):
    for name in dirs + files:
        path = os.path.join(root, name)
        try:
            stat = os.lstat(path)
        except FileNotFoundError:
            continue
        total += stat.st_size
print(total)
PY
}

calculate_guest_image_size_bytes() {
  local rootfs_size_bytes="$1"
  local size_step_bytes="$((256 * 1024 * 1024))"
  local reserved_bytes="$((64 * 1024 * 1024))"
  local requested_bytes

  requested_bytes="$((rootfs_size_bytes + reserved_bytes))"
  printf '%s\n' "$(( ((requested_bytes + size_step_bytes - 1) / size_step_bytes) * size_step_bytes ))"
}

run_mkfs_ext4_with_optional_sudo() {
  if [[ "${EUID}" -eq 0 ]]; then
    mkfs.ext4 "$@"
    return 0
  fi

  if mkfs.ext4 "$@" >/dev/null 2>&1; then
    return 0
  fi

  require_cmd sudo
  sudo mkfs.ext4 "$@"
}

run_as_root() {
  if [[ "${EUID}" -eq 0 ]]; then
    "$@"
    return $?
  fi

  # Try without sudo first so command substitution still captures stdout.
  if "$@" 2>/dev/null; then
    return 0
  fi

  require_cmd sudo
  sudo "$@"
}

# Locale-stable dumpe2fs wrapper: dumpe2fs translates field names under
# non-C locales, which would break the awk parsing in shrink_ext4_image.
dump_ext4_header() {
  local img="$1"
  if [[ "${EUID}" -eq 0 ]]; then
    LC_ALL=C dumpe2fs -h "${img}" 2>/dev/null
    return $?
  fi

  if LC_ALL=C dumpe2fs -h "${img}" 2>/dev/null; then
    return 0
  fi

  require_cmd sudo
  sudo LC_ALL=C dumpe2fs -h "${img}" 2>/dev/null
}

SHRINK_RESERVED_BYTES="${ONE_CLICK_GUEST_IMAGE_RESERVED_BYTES:-$((32 * 1024 * 1024))}"

# The cube hypervisor exposes the guest image as a pmem device, and the device
# manager rejects pmem regions whose size is not a multiple of 2 MiB (matches
# the guest hugepage granularity, see hypervisor/vmm/src/device_manager.rs:
# `if size % 0x20_0000 != 0 { Err(PmemSizeNotAligned) }`). The shrink path
# below must therefore round the final image size *up* to a 2 MiB boundary
# instead of stopping at ext4's natural block alignment (4 KiB), otherwise
# template launch fails with PmemSizeNotAligned.
PMEM_ALIGN_BYTES=$((2 * 1024 * 1024))

# Shrink the ext4 image to its minimum size, then grow it by RESERVED bytes of
# free headroom so the guest still has room for runtime writes.
shrink_ext4_image() {
  local img="$1"
  local reserved_bytes="${2:-${SHRINK_RESERVED_BYTES}}"
  local dumpe2fs_out block_size min_blocks reserved_blocks target_blocks final_bytes min_bytes
  local pmem_align_blocks

  run_as_root e2fsck -fy "${img}" >&2 || true
  run_as_root resize2fs -M "${img}" >&2

  dumpe2fs_out="$(dump_ext4_header "${img}")"
  block_size="$(printf '%s\n' "${dumpe2fs_out}" | awk -F': *' '/^Block size/ {print $2; exit}')"
  min_blocks="$(printf '%s\n' "${dumpe2fs_out}" | awk -F': *' '/^Block count/ {print $2; exit}')"

  if [[ -z "${block_size}" || -z "${min_blocks}" ]]; then
    die "failed to parse ext4 metadata from ${img}"
  fi

  # ext4 block sizes are always a power of two (1/2/4 KiB), so 2 MiB is an
  # exact multiple of every legal block size. Verify defensively so a future
  # exotic block size produces a clear error instead of a subtly misaligned
  # image that only fails inside the VMM.
  if (( PMEM_ALIGN_BYTES % block_size != 0 )); then
    die "pmem alignment ${PMEM_ALIGN_BYTES} not a multiple of ext4 block size ${block_size}"
  fi
  pmem_align_blocks="$(( PMEM_ALIGN_BYTES / block_size ))"

  reserved_blocks="$(( (reserved_bytes + block_size - 1) / block_size ))"
  target_blocks="$(( min_blocks + reserved_blocks ))"
  # Round target_blocks UP to the pmem alignment so the resulting image size
  # (target_blocks * block_size) is a multiple of 2 MiB. Rounding up only ever
  # grows the headroom (worst case <2 MiB extra), so it cannot truncate live
  # filesystem data.
  target_blocks="$(( ((target_blocks + pmem_align_blocks - 1) / pmem_align_blocks) * pmem_align_blocks ))"
  final_bytes="$(( target_blocks * block_size ))"
  min_bytes="$(( min_blocks * block_size ))"

  # Defensive sanity check: truncating below the shrunk filesystem size would
  # chop live FS data. With reserved_blocks >= 0 this should never trigger,
  # but we want a clear failure if future refactors break the invariant.
  if (( final_bytes < min_bytes )); then
    die "shrink target ${final_bytes} smaller than ext4 minimum ${min_bytes}"
  fi
  if (( final_bytes % PMEM_ALIGN_BYTES != 0 )); then
    die "shrink target ${final_bytes} not aligned to pmem boundary ${PMEM_ALIGN_BYTES}"
  fi

  # The resulting ext4 file is sparse: ext4 free space inside the image
  # corresponds to filesystem holes on the host. Packagers that don't
  # preserve sparseness (e.g. plain tar without --sparse) will inflate
  # the file back to its apparent size, but gzip still compresses the
  # zeroed extents efficiently.
  run_as_root truncate -s "${final_bytes}" "${img}"
  run_as_root resize2fs "${img}" "${target_blocks}" >&2
  run_as_root e2fsck -fy "${img}" >&2 || true

  local human_final human_reserved
  human_final="$(numfmt --to=iec --suffix=B "${final_bytes}" 2>/dev/null || echo "${final_bytes}")"
  human_reserved="$(numfmt --to=iec --suffix=B "${reserved_bytes}" 2>/dev/null || echo "${reserved_bytes}")"
  log "guest image shrunk to ${human_final} (reserved ${human_reserved} headroom, 2MiB pmem aligned)"
}

remove_path_with_optional_sudo() {
  if [[ "$#" -eq 0 ]]; then
    return 0
  fi

  if [[ "${EUID}" -eq 0 ]]; then
    rm -rf "$@"
    return 0
  fi

  rm -rf "$@" 2>/dev/null || {
    require_cmd sudo
    sudo rm -rf "$@"
  }
}

inject_init_into_guest_rootfs() {
  local guest_rootfs_dir="$1"
  local init_path="${guest_rootfs_dir}/sbin/init"
  local init_backup_path="${guest_rootfs_dir}/sbin/init.original"
  local rc_local_path="${guest_rootfs_dir}/etc/rc.local"
  local rc_local_tmp="${GUEST_IMAGE_WORK_DIR}/rc.local"
  local hostname_tmp="${GUEST_IMAGE_WORK_DIR}/hostname"
  local hosts_tmp="${GUEST_IMAGE_WORK_DIR}/hosts"
  local resolv_tmp="${GUEST_IMAGE_WORK_DIR}/resolv.conf"

  ensure_file "${INIT_BIN}"

  mkdir -p "${guest_rootfs_dir}/sbin" "${guest_rootfs_dir}/etc"

  if [[ -e "${init_path}" || -L "${init_path}" ]]; then
    remove_path_with_optional_sudo "${init_backup_path}"
    mv -f "${init_path}" "${init_backup_path}" 2>/dev/null || {
      require_cmd sudo
      sudo mv -f "${init_path}" "${init_backup_path}"
    }
  fi

  copy_binary_with_deps "${INIT_BIN}" "/sbin/init" "${guest_rootfs_dir}"

  if [[ ! -e "${rc_local_path}" ]]; then
    cat > "${rc_local_tmp}" <<'EOF'
#!/bin/sh
exit 0
EOF
    cp -f "${rc_local_tmp}" "${rc_local_path}" 2>/dev/null || {
      require_cmd sudo
      sudo cp -f "${rc_local_tmp}" "${rc_local_path}"
    }
    chmod +x "${rc_local_path}" 2>/dev/null || {
      require_cmd sudo
      sudo chmod +x "${rc_local_path}"
    }
  fi

  cat > "${hostname_tmp}" <<'EOF'
localhost
EOF
  cp -f "${hostname_tmp}" "${guest_rootfs_dir}/etc/hostname" 2>/dev/null || {
    require_cmd sudo
    sudo cp -f "${hostname_tmp}" "${guest_rootfs_dir}/etc/hostname"
  }

  cat > "${hosts_tmp}" <<'EOF'
127.0.0.1 localhost
EOF
  cp -f "${hosts_tmp}" "${guest_rootfs_dir}/etc/hosts" 2>/dev/null || {
    require_cmd sudo
    sudo cp -f "${hosts_tmp}" "${guest_rootfs_dir}/etc/hosts"
  }

  cat > "${resolv_tmp}" <<'EOF'
nameserver 119.29.29.29
EOF
  if [[ -L "${guest_rootfs_dir}/etc/resolv.conf" ]]; then
    remove_path_with_optional_sudo "${guest_rootfs_dir}/etc/resolv.conf"
  fi
  cp -f "${resolv_tmp}" "${guest_rootfs_dir}/etc/resolv.conf" 2>/dev/null || {
    require_cmd sudo
    sudo cp -f "${resolv_tmp}" "${guest_rootfs_dir}/etc/resolv.conf"
  }
}

build_cube_init() {
  if [[ -n "${CUBE_INIT_BIN_OVERRIDE:-}" ]]; then
    ensure_file "${CUBE_INIT_BIN_OVERRIDE}"
    log "using prebuilt cube-init: ${CUBE_INIT_BIN_OVERRIDE}"
    printf '%s\n' "${CUBE_INIT_BIN_OVERRIDE}"
    return 0
  fi

  case "${CUBE_INIT_BUILD_MODE:-local}" in
    local)
      require_cmd make
      log "building cube-init via make"
      (cd "${ROOT_DIR}/guest-init" && make) >&2
      ;;
    docker)
      require_cmd make
      require_cmd docker
      log "building cube-init via make in builder"
      (cd "${ROOT_DIR}" && make cube-init) >&2
      find_built_binary "${ROOT_DIR}/_output/bin" "cube-init"
      return 0
      ;;
    *)
      die "unsupported ONE_CLICK_CUBE_INIT_BUILD_MODE: ${CUBE_INIT_BUILD_MODE}"
      ;;
  esac

  find_built_binary "${ROOT_DIR}/guest-init/target" "cube-init"
}

# Build an independent cube-agent.ext4 containing only /cube-agent (2 MiB aligned).
# Does NOT package e2fsprogs tools. Agent binary must be a static ELF.
install_static_agent_binary() {
  local src_bin="$1"
  local dst_root="$2"
  local dst="${dst_root}/cube-agent"
  local ldd_out=""

  ensure_file "${src_bin}"
  mkdir -p "${dst_root}"

  # Refuse dynamic binaries so host ABI/libs never leak into the plane file.
  if command -v ldd >/dev/null 2>&1; then
    ldd_out="$(ldd "${src_bin}" 2>&1 || true)"
    if printf '%s\n' "${ldd_out}" | grep -qE '=>|[[:space:]]/lib|ld-linux|ld-musl'; then
      die "cube-agent for agent.ext4 must be a static ELF (got dynamic deps): ${src_bin}"
    fi
  fi
  if command -v file >/dev/null 2>&1; then
    if ! file -b "${src_bin}" | grep -qiE 'ELF.*(statically linked|static-pie|static )'; then
      # Some file(1) versions only say "ELF 64-bit LSB executable" for musl static;
      # if ldd already confirmed non-dynamic, accept. Otherwise fail closed when
      # ldd was unavailable.
      if ! command -v ldd >/dev/null 2>&1; then
        die "cube-agent for agent.ext4 must be a static ELF (file(1) check failed): ${src_bin}"
      fi
    fi
  fi

  install -m 0755 "${src_bin}" "${dst}"
  [[ -x "${dst}" ]] || die "failed to install ${dst}"
}

build_agent_ext4_artifacts() {
  local agent_bin="$1"
  local output_img="$2"
  local output_version="$3"
  local work_dir="${GUEST_IMAGE_WORK_DIR:-${ONE_CLICK_WORK_ROOT:-.}/agent-ext4-build}/agent-rootfs"
  local rootfs_size_bytes
  local image_size_bytes
  local stage_dir
  local stage_img
  local stage_version

  ensure_file "${agent_bin}"
  mkdir -p "$(dirname "${output_img}")" "$(dirname "${output_version}")"
  remove_path_with_optional_sudo "${work_dir}"
  mkdir -p "${work_dir}"

  install_static_agent_binary "${agent_bin}" "${work_dir}"

  rootfs_size_bytes="$(directory_size_bytes "${work_dir}")"
  # Agent ext4 is tiny; start from at least 16 MiB then shrink+align.
  image_size_bytes="$(( rootfs_size_bytes + 8 * 1024 * 1024 ))"
  if (( image_size_bytes < 16 * 1024 * 1024 )); then
    image_size_bytes="$((16 * 1024 * 1024))"
  fi
  # Round up to 2 MiB for truncate before mkfs.
  image_size_bytes="$(( ((image_size_bytes + PMEM_ALIGN_BYTES - 1) / PMEM_ALIGN_BYTES) * PMEM_ALIGN_BYTES ))"

  # Build into a staging directory then rename for a more atomic publish.
  stage_dir="$(dirname "${output_img}")/.cube-agent-stage.$$"
  remove_path_with_optional_sudo "${stage_dir}"
  mkdir -p "${stage_dir}"
  stage_img="${stage_dir}/cube-agent.ext4"
  stage_version="${stage_dir}/version"

  truncate -s "${image_size_bytes}" "${stage_img}"
  run_mkfs_ext4_with_optional_sudo -F -b 4096 -d "${work_dir}" "${stage_img}" >&2

  # Minimal reserved headroom for agent.ext4 (still 2 MiB aligned by shrink).
  SHRINK_RESERVED_BYTES="${ONE_CLICK_AGENT_EXT4_RESERVED_BYTES:-$((2 * 1024 * 1024))}" \
    shrink_ext4_image "${stage_img}"

  printf '%s\n' "${CUBE_VERSION}" > "${stage_version}"
  remove_path_with_optional_sudo "${work_dir}"

  mv -f "${stage_img}" "${output_img}"
  mv -f "${stage_version}" "${output_version}"
  remove_path_with_optional_sudo "${stage_dir}"
  log "cube-agent.ext4 ready: ${output_img}"
}


build_guest_image_artifacts() {
  local output_img="$1"
  local output_version="$2"
  local rootfs_size_bytes
  local image_size_bytes
  local guest_container_id=""

  ensure_dir "${GUEST_IMAGE_CONTEXT_DIR}"
  ensure_file "${GUEST_IMAGE_DOCKERFILE}"

  mkdir -p "${GUEST_IMAGE_WORK_DIR}" "$(dirname "${output_img}")" "$(dirname "${output_version}")"
  remove_path_with_optional_sudo "${GUEST_ROOTFS_DIR}" "${GUEST_ROOTFS_TAR}"

  log "building guest image from ${GUEST_IMAGE_DOCKERFILE}"
  docker build -t "${GUEST_IMAGE_REF}" -f "${GUEST_IMAGE_DOCKERFILE}" "${GUEST_IMAGE_CONTEXT_DIR}" >&2

  guest_container_id="$(docker create "${GUEST_IMAGE_REF}")"
  trap 'if [[ -n "${guest_container_id:-}" ]]; then docker rm -f "${guest_container_id}" >/dev/null 2>&1 || true; fi' RETURN

  log "exporting guest rootfs from ${GUEST_IMAGE_REF}"
  docker export -o "${GUEST_ROOTFS_TAR}" "${guest_container_id}" >&2

  mkdir -p "${GUEST_ROOTFS_DIR}"
  tar -xf "${GUEST_ROOTFS_TAR}" -C "${GUEST_ROOTFS_DIR}"
  inject_init_into_guest_rootfs "${GUEST_ROOTFS_DIR}"

  rootfs_size_bytes="$(directory_size_bytes "${GUEST_ROOTFS_DIR}")"
  image_size_bytes="$(calculate_guest_image_size_bytes "${rootfs_size_bytes}")"

  truncate -s "${image_size_bytes}" "${output_img}"
  # Force 4K block size: CubeShim boots the kernel with rootflags=dax, which
  # does not support 1K block sizes and would panic at boot time.
  run_mkfs_ext4_with_optional_sudo -F -b 4096 -d "${GUEST_ROOTFS_DIR}" "${output_img}" >&2

  shrink_ext4_image "${output_img}"

  printf '%s\n' "${GUEST_IMAGE_VERSION}" > "${output_version}"

  docker rm -f "${guest_container_id}" >/dev/null 2>&1 || true
  guest_container_id=""
  remove_path_with_optional_sudo "${GUEST_ROOTFS_DIR}" "${GUEST_ROOTFS_TAR}"
  trap - RETURN
}

