#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Big Pod / Installer per-component entrypoint (REV3.1).
# Env:
#   CUBE_COMPONENT  cubelet|cube-shim|cube-kernel|cube-guest|cube-agent
#   CUBE_ROLE       install|run
#     install — artifact-only components on cube-node-installer Pod: stage + pause
#     run     — cubelet on Big Pod: self-stage then start the process
#   IMAGE_ROOT      default /opt/cube-image
#   TOOLBOX_ROOT    default /usr/local/services/cubetoolbox
set -euo pipefail

IMAGE_ROOT="${IMAGE_ROOT:-/opt/cube-image}"
TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
COMPONENT_VERSIONS_ROOT="${COMPONENT_VERSIONS_ROOT:-/data/cubelet/root/component_versions}"
CUBE_COMPONENT="${CUBE_COMPONENT:-}"
CUBE_ROLE="${CUBE_ROLE:-install}"
CUBE_PID_DIR="${CUBE_PID_DIR:-/run/cube-node}"
CUBE_HOST_CGROUP_NAME="${CUBE_HOST_CGROUP_NAME:-cube-sandbox-runtime}"
CUBE_COMPONENT_FINGERPRINT="${CUBE_COMPONENT_FINGERPRINT:-}"
STATE_DIR="${STATE_DIR:-/var/lib/cube-node-bootstrap}"

log() { printf '[cube-component:%s:%s] %s\n' "${CUBE_COMPONENT:-?}" "${CUBE_ROLE}" "$*"; }
fail() { printf '[cube-component:%s:%s] ERROR: %s\n' "${CUBE_COMPONENT:-?}" "${CUBE_ROLE}" "$*" >&2; exit 1; }

# Components staged into COMPONENT_VERSIONS_ROOT before toolbox replace.
is_inventory_component() {
  case "$1" in
    cube-shim|cube-kernel|cube-guest|cube-agent) return 0 ;;
    *) return 1 ;;
  esac
}

# Read "version" under a nested JSON object key (no jq).
json_object_version() {
  local file="$1" key="$2"
  local collapsed
  [[ -f "${file}" ]] || return 0
  collapsed="$(tr '\n' ' ' < "${file}" 2>/dev/null || true)"
  [[ -n "${collapsed}" ]] || return 0
  printf '%s' "${collapsed}" | sed -n \
    "s/.*\"${key}\"[[:space:]]*:[[:space:]]*{[^}]*\"version\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" \
    | head -n1
}

# Prefer version.json, else single-line version file. Empty means fail upstream.
resolve_component_version() {
  local src="$1"
  local component="$2"
  local json="${src}/version.json"
  local ver="" key

  if [[ -f "${json}" ]]; then
    case "${component}" in
      cube-shim)
        for key in containerd-shim-cube-rs cube-runtime; do
          ver="$(json_object_version "${json}" "${key}")"
          [[ -n "${ver}" ]] && break
        done
        ;;
      cube-kernel)
        ;;
      cube-guest)
        ver="$(json_object_version "${json}" "guest-image")"
        ;;
      cube-agent)
        ver="$(json_object_version "${json}" "cube-agent")"
        ;;
    esac
  fi

  if [[ -z "${ver}" && -f "${src}/version" ]]; then
    ver="$(tr -d '[:space:]' < "${src}/version" 2>/dev/null || true)"
  fi

  ver="$(printf '%s' "${ver}" | tr -d '[:space:]')"
  case "${ver}" in
    ""|unknown|UNKNOWN) return 0 ;;
  esac
  if [[ "${ver}" == */* || "${ver}" == *..* ]]; then
    return 0
  fi
  printf '%s\n' "${ver}"
}

# Copy src into COMPONENT_VERSIONS_ROOT/<rel>/<version>/ (skip if present).
inventory_component_version() {
  local src="$1"
  local rel="$2"
  local ver dst parent
  ver="$(resolve_component_version "${src}" "${CUBE_COMPONENT}")"
  [[ -n "${ver}" ]] || fail "cannot resolve version for ${CUBE_COMPONENT} under ${src} (need version.json or version; unknown forbidden)"
  dst="${COMPONENT_VERSIONS_ROOT}/${rel}/${ver}"
  if [[ -d "${dst}" ]]; then
    log "inventory skip (exists): ${dst}"
    return 0
  fi
  parent="$(dirname "${dst}")"
  mkdir -p "${parent}"
  log "inventory ${src} -> ${dst}"
  atomic_replace_dir "${src}" "${dst}"
}

file_sha256_hex() {
  local path="$1"
  local digest=""
  [[ -f "${path}" ]] || return 1
  if command -v sha256sum >/dev/null 2>&1; then
    digest="$(sha256sum -- "${path}" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    digest="$(shasum -a 256 -- "${path}" | awk '{print $1}')"
  else
    return 1
  fi
  [[ -n "${digest}" ]] || return 1
  printf '%s\n' "${digest}"
}

# Inventory each present kernel variant by content short-hash.
inventory_kernel_content_variants() {
  local src="$1"
  local rel="$2"
  local file variant digest short_key dst tmp parent

  [[ -d "${src}" ]] || fail "kernel image bypass missing: ${src}"

  for variant in bm pvm; do
    file="${src}/vmlinux-${variant}"
    [[ -f "${file}" ]] || continue
    digest="$(file_sha256_hex "${file}")" || fail "cannot hash ${file}"
    short_key="sha256-${digest:0:12}"
    dst="${COMPONENT_VERSIONS_ROOT}/${rel}/${short_key}"
    if [[ -d "${dst}" ]]; then
      log "inventory skip (exists): ${dst}"
      continue
    fi
    parent="$(dirname "${dst}")"
    mkdir -p "${parent}"
    tmp="${dst}.new.$$"
    rm -rf "${tmp}"
    mkdir -p "${tmp}"
    cp -a "${file}" "${tmp}/vmlinux-${variant}"
    ln -sfn "vmlinux-${variant}" "${tmp}/vmlinux"
    printf '%s\n' "${variant}" > "${tmp}/variant"
    printf 'sha256:%s\n' "${digest}" > "${tmp}/version"
    if [[ -e "${dst}" ]]; then
      rm -rf "${tmp}"
      log "inventory skip (race exists): ${dst}"
      continue
    fi
    mv "${tmp}" "${dst}"
    log "inventory kernel ${variant} -> ${dst}"
  done
  if [[ ! -f "${src}/vmlinux-bm" && ! -f "${src}/vmlinux-pvm" ]]; then
    fail "cube-kernel inventory requires vmlinux-bm and/or vmlinux-pvm under ${src}"
  fi
}

apply_effective_pvm_from_state() {
  local path="${STATE_DIR}/effective-pvm"
  local val
  [[ -f "${path}" ]] || return 0
  val="$(tr -d '[:space:]' < "${path}" 2>/dev/null || true)"
  case "${val}" in
    0|1)
      CUBE_PVM_ENABLE="${val}"
      export CUBE_PVM_ENABLE
      log "CUBE_PVM_ENABLE overridden from ${path}=${val}"
      ;;
  esac
}

component_relpath() {
  case "$1" in
    cubelet) echo "Cubelet" ;;
    cube-shim) echo "cube-shim" ;;
    cube-kernel) echo "cube-kernel-scf" ;;
    cube-guest) echo "cube-image" ;;
    cube-agent) echo "cube-agent" ;;
    *) fail "unknown CUBE_COMPONENT=$1" ;;
  esac
}

component_sentinel() {
  case "$1" in
    cubelet) echo "${TOOLBOX_ROOT}/.staged-cubelet" ;;
    cube-shim) echo "${TOOLBOX_ROOT}/.staged-cube-shim" ;;
    cube-kernel) echo "${TOOLBOX_ROOT}/.staged-cube-kernel" ;;
    cube-guest) echo "${TOOLBOX_ROOT}/.staged-cube-guest" ;;
    cube-agent) echo "${TOOLBOX_ROOT}/.staged-cube-agent" ;;
    *) fail "unknown CUBE_COMPONENT=$1" ;;
  esac
}

wait_sentinel() {
  local path="$1"
  local name="$2"
  local i
  for i in $(seq 1 300); do
    if [[ -f "${path}" ]]; then
      log "sentinel ready: ${name} (${path})"
      return 0
    fi
    sleep 1
  done
  fail "timeout waiting for sentinel ${name} at ${path}"
}

# Absolute mountpoints at dst or under it (from this mount namespace).
# mountinfo field 5 is the mountpoint; optional fields follow mount options.
list_mounts_under() {
  local dst="$1"
  local canon=""
  canon="$(readlink -f "${dst}" 2>/dev/null || true)"
  [[ -n "${canon}" ]] || return 0
  awk -v p="${canon}" '
    {
      mp = $5
      gsub(/\\040/, " ", mp)
      gsub(/\\011/, "\t", mp)
      gsub(/\\012/, "\n", mp)
      gsub(/\\134/, "\\", mp)
      if (mp == p || index(mp, p "/") == 1) print mp
    }
  ' /proc/self/mountinfo
}

path_is_exact_mount() {
  local path="$1"
  local m
  for m in "${_CUBE_PRESERVE_MOUNTS[@]}"; do
    [[ "${path}" == "${m}" ]] && return 0
  done
  return 1
}

# True when path is a preserved mount or an ancestor directory of one.
# Never unlink/rename these: kubelet file binds follow the dentry.
path_is_mount_or_ancestor() {
  local path="$1"
  local m
  for m in "${_CUBE_PRESERVE_MOUNTS[@]}"; do
    [[ "${path}" == "${m}" || "${m}" == "${path}"/* ]] && return 0
  done
  return 1
}

# Overlay src onto dst without renaming dst (keeps bind mounts on live paths).
overlay_dir_preserving_mounts() {
  local src="$1"
  local dst="$2"
  local rel path_src path_dst

  while IFS= read -r -d '' rel; do
    rel="${rel#./}"
    [[ -n "${rel}" ]] || continue
    path_src="${src}/${rel}"
    path_dst="${dst}/${rel}"
    if path_is_exact_mount "${path_dst}"; then
      continue
    fi
    if [[ -d "${path_src}" && ! -L "${path_src}" ]]; then
      mkdir -p "${path_dst}"
      chmod --reference="${path_src}" "${path_dst}" 2>/dev/null || true
      continue
    fi
    mkdir -p "$(dirname "${path_dst}")"
    cp -a "${path_src}" "${path_dst}"
  done < <(cd "${src}" && find . -mindepth 1 -print0)

  while IFS= read -r -d '' rel; do
    rel="${rel#./}"
    [[ -n "${rel}" ]] || continue
    path_dst="${dst}/${rel}"
    path_src="${src}/${rel}"
    [[ -e "${path_dst}" || -L "${path_dst}" ]] || continue
    if path_is_mount_or_ancestor "${path_dst}"; then
      continue
    fi
    if [[ -e "${path_src}" || -L "${path_src}" ]]; then
      continue
    fi
    rm -rf "${path_dst}"
  done < <(cd "${dst}" && find . -mindepth 1 -depth -print0)
}

# Promote a staged tree into place without rm -rf of the live directory.
# Rename-aside then rename-in leaves a brief ENOENT window; we keep a
# ".staging-<component>" marker for the whole window so the collector marks
# inventory_incomplete (and Master will not hard-delete). Requires src/dst on
# the same filesystem.
#
# If kubelet (or anything else) has bind-mounted files under dst — typically
# Cubelet/plugin/volume-s3.conf / volume-cos.conf — renaming dst would move
# those mounts onto Cubelet.legacy.* and rm -rf would EBUSY, while the new
# tree would lose the Secret. Overlay in place instead and skip mountpoints.
atomic_replace_dir() {
  local src="$1"
  local dst="$2"
  local parent new legacy recovered="" canon=""
  local -a _CUBE_PRESERVE_MOUNTS=()
  parent="$(dirname "${dst}")"
  mkdir -p "${parent}"

  # Crash recovery first: dst missing after rename-aside — restore newest legacy.
  if [[ ! -e "${dst}" ]]; then
    recovered="$(ls -1dt "${dst}.legacy."* 2>/dev/null | head -n1 || true)"
    if [[ -n "${recovered}" && -d "${recovered}" ]]; then
      log "recovering ${dst} from ${recovered}"
      mv "${recovered}" "${dst}" || fail "cannot restore ${dst} from ${recovered}"
    fi
  fi

  # Drop remaining orphans from prior crashed stages (same component is not concurrent).
  rm -rf "${dst}.new."* "${dst}.legacy."* 2>/dev/null || true

  new="${dst}.new.$$"
  legacy="${dst}.legacy.$$"
  rm -rf "${new}" "${legacy}"
  cp -a "${src}" "${new}"
  if [[ ! -e "${dst}" ]]; then
    mv "${new}" "${dst}"
    return 0
  fi

  mapfile -t _CUBE_PRESERVE_MOUNTS < <(list_mounts_under "${dst}")
  if ((${#_CUBE_PRESERVE_MOUNTS[@]} > 0)); then
    canon="$(readlink -f "${dst}" 2>/dev/null || printf '%s' "${dst}")"
    if path_is_exact_mount "${canon}"; then
      rm -rf "${new}"
      fail "cannot replace ${dst}: destination is a mount point"
    fi
    log "preserving ${#_CUBE_PRESERVE_MOUNTS[@]} bind mount(s) under ${dst}; overlay instead of rename-aside"
    overlay_dir_preserving_mounts "${new}" "${dst}"
    rm -rf "${new}"
    return 0
  fi

  if mv -T "${dst}" "${legacy}" 2>/dev/null || mv "${dst}" "${legacy}"; then
    if mv -T "${new}" "${dst}" 2>/dev/null || mv "${new}" "${dst}"; then
      rm -rf "${legacy}"
      return 0
    fi
    mv -T "${legacy}" "${dst}" 2>/dev/null || mv "${legacy}" "${dst}" || true
    rm -rf "${new}"
    fail "failed to promote staged tree into ${dst}"
  fi
  fail "cannot replace ${dst} (rename-aside failed)"
}

# Return vmlinux-bm|vmlinux-pvm from a symlink path, or empty.
kernel_symlink_target() {
  local link="$1"
  local base
  [[ -L "${link}" ]] || return 0
  base="$(basename "$(readlink "${link}")")"
  case "${base}" in
    vmlinux-bm|vmlinux-pvm) printf '%s\n' "${base}" ;;
  esac
}

# Capture active guest-kernel selection before a whole-tree replace.
preserve_guest_kernel_selection() {
  local dir="$1"
  local state_dir="${STATE_DIR:-/var/lib/cube-node-bootstrap}"
  local t=""
  t="$(kernel_symlink_target "${state_dir}/vmlinux-active")"
  if [[ -z "${t}" ]]; then
    t="$(kernel_symlink_target "${dir}/vmlinux")"
  fi
  printf '%s\n' "${t}"
}

stage_component() {
  local rel="$1"
  local src="${IMAGE_ROOT}/${rel}"
  local dst="${TOOLBOX_ROOT}/${rel}"
  local sentinel staging_marker preserved_kernel=""
  sentinel="$(component_sentinel "${CUBE_COMPONENT}")"
  staging_marker="${TOOLBOX_ROOT}/.staging-${CUBE_COMPONENT}"

  [[ -d "${src}" ]] || fail "image bypass missing: ${src}"
  mkdir -p "${TOOLBOX_ROOT}"
  # Mark in-flight before clearing the ready sentinel so collectors see incomplete
  # during the rename-aside ENOENT window. Cleared only on success — keep marker
  # on failure/crash so Incomplete is not a false negative.
  printf 'staging\n' > "${staging_marker}.tmp"
  mv -f "${staging_marker}.tmp" "${staging_marker}"
  rm -f "${sentinel}"

  if [[ "${CUBE_COMPONENT}" == "cube-kernel" ]]; then
    preserved_kernel="$(preserve_guest_kernel_selection "${dst}")"
    [[ -n "${preserved_kernel}" ]] && log "preserved guest kernel selection: ${preserved_kernel}"
  fi

  if is_inventory_component "${CUBE_COMPONENT}"; then
    if [[ "${CUBE_COMPONENT}" == "cube-kernel" ]]; then
      inventory_kernel_content_variants "${src}" "${rel}"
    else
      inventory_component_version "${src}" "${rel}"
    fi
  fi

  log "staging ${src} -> ${dst} (atomic replace)"
  atomic_replace_dir "${src}" "${dst}"

  case "${CUBE_COMPONENT}" in
    cubelet)
      chmod +x "${dst}/bin/cubelet" "${dst}/bin/cubecli" 2>/dev/null || true
      [[ -x "${dst}/bin/cubelet" ]] || fail "missing cubelet after stage"
      [[ -x "${dst}/bin/cubecli" ]] || fail "missing cubecli after stage"
      ;;
    cube-shim)
      chmod +x "${dst}/bin/cube-runtime" "${dst}/bin/containerd-shim-cube-rs" 2>/dev/null || true
      [[ -x "${dst}/bin/containerd-shim-cube-rs" ]] || fail "missing shim after stage"
      [[ -x "${dst}/bin/cube-runtime" ]] || fail "missing cube-runtime after stage"
      # containerd resolves io.containerd.cube.rs via PATH (same as one-click install.sh).
      mkdir -p /usr/local/bin
      ln -sf "${dst}/bin/containerd-shim-cube-rs" /usr/local/bin/containerd-shim-cube-rs
      ln -sf "${dst}/bin/cube-runtime" /usr/local/bin/cube-runtime
      ;;
    cube-kernel)
      [[ -e "${dst}/vmlinux-bm" || -e "${dst}/vmlinux-pvm" || -e "${dst}/vmlinux" ]] \
        || fail "missing guest kernel files under ${dst}"
      apply_effective_pvm_from_state
      select_guest_kernel "${preserved_kernel}"
      ;;
    cube-guest)
      [[ -d "${dst}" ]] || fail "missing guest image dir ${dst}"
      [[ -f "${dst}/cube-guest-image-cpu.img" ]] || fail "missing ${dst}/cube-guest-image-cpu.img"
      ;;
    cube-agent)
      [[ -f "${dst}/cube-agent.ext4" ]] || fail "missing ${dst}/cube-agent.ext4"
      [[ -f "${dst}/version" ]] || fail "missing ${dst}/version"
      ;;
  esac

  ensure_component_version_json "${CUBE_COMPONENT}" "${dst}"

  # Digest: informational change marker (completeness is write-order: cp then sentinel).
  {
    printf 'component=%s\n' "${CUBE_COMPONENT}"
    printf 'staged_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    find "${dst}" -type f -printf '%P %s %T@\n' 2>/dev/null | sort | head -n 50 || true
  } > "${sentinel}.tmp"
  mv -f "${sentinel}.tmp" "${sentinel}"
  rm -f "${staging_marker}"
  log "wrote sentinel ${sentinel}"
}

# Best-effort: if the image did not bake version.json, synthesize from guest markers.
json_escape() {
  # Minimal JSON string escape for version tokens (no control chars expected).
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# Atomic tmp+mv write; body is read from stdin.
write_atomic() {
  local dest="$1"
  local tmp="${dest}.tmp.$$"
  cat > "${tmp}"
  mv -f "${tmp}" "${dest}"
}

# True when kernel version.json is missing digests for present vmlinux variants.
kernel_version_json_needs_digest_rewrite() {
  local dst="$1"
  local json="${dst}/version.json"
  local collapsed=""

  [[ -f "${dst}/vmlinux-bm" || -f "${dst}/vmlinux-pvm" ]] || return 1
  if [[ ! -f "${json}" ]]; then
    return 0
  fi
  collapsed="$(tr '\n' ' ' < "${json}" 2>/dev/null || true)"
  if [[ -f "${dst}/vmlinux-bm" ]]; then
    printf '%s' "${collapsed}" | grep -Eq '"bm"[[:space:]]*:[[:space:]]*\{[^}]*"digest_sha256"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]{64}"' \
      || return 0
  fi
  if [[ -f "${dst}/vmlinux-pvm" ]]; then
    printf '%s' "${collapsed}" | grep -Eq '"pvm"[[:space:]]*:[[:space:]]*\{[^}]*"digest_sha256"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]{64}"' \
      || return 0
  fi
  return 1
}

write_kernel_version_json_from_content() {
  local dst="$1"
  local json="${dst}/version.json"
  local bm_digest pvm_digest bm_short pvm_short first=1

  {
    printf '{\n  "schema_version": 1,\n  "variants": {\n'
    if [[ -f "${dst}/vmlinux-bm" ]]; then
      bm_digest="$(file_sha256_hex "${dst}/vmlinux-bm")" || true
      if [[ -n "${bm_digest}" ]]; then
        bm_short="sha256-${bm_digest:0:12}"
        printf '    "bm": {"version": "%s", "digest_sha256": "sha256:%s"}' \
          "$(json_escape "${bm_short}")" "$(json_escape "${bm_digest}")"
        first=0
        printf 'sha256:%s\n' "${bm_digest}" > "${dst}/version"
      fi
    fi
    if [[ -f "${dst}/vmlinux-pvm" ]]; then
      pvm_digest="$(file_sha256_hex "${dst}/vmlinux-pvm")" || true
      if [[ -n "${pvm_digest}" ]]; then
        pvm_short="sha256-${pvm_digest:0:12}"
        [[ "${first}" == "1" ]] || printf ','
        printf '\n    "pvm": {"version": "%s", "digest_sha256": "sha256:%s"}' \
          "$(json_escape "${pvm_short}")" "$(json_escape "${pvm_digest}")"
      fi
    fi
    printf '\n  }\n}\n'
  } | write_atomic "${json}"
  log "wrote content-addressed ${json}"
}

ensure_component_version_json() {
  local component="$1"
  local dst="$2"
  local json="${dst}/version.json"

  case "${component}" in
    cube-guest)
      [[ -f "${json}" ]] && return 0
      local img_ver
      img_ver="$(tr -d '[:space:]' < "${dst}/version" 2>/dev/null || true)"
      [[ -n "${img_ver}" ]] || return 0
      {
        printf '{\n  "schema_version": 1,\n  "components": {\n'
        printf '    "guest-image": {"version": "%s"}\n' "$(json_escape "${img_ver}")"
        printf '  }\n}\n'
      } | write_atomic "${json}"
      log "synthesized ${json}"
      ;;
    cube-agent)
      [[ -f "${json}" ]] && return 0
      local agent_ver
      agent_ver="$(tr -d '[:space:]' < "${dst}/version" 2>/dev/null || true)"
      [[ -n "${agent_ver}" ]] || return 0
      {
        printf '{\n  "schema_version": 1,\n  "components": {\n'
        printf '    "cube-agent": {"version": "%s"}\n' "$(json_escape "${agent_ver}")"
        printf '  }\n}\n'
      } | write_atomic "${json}"
      log "synthesized ${json}"
      ;;
    cube-kernel)
      if kernel_version_json_needs_digest_rewrite "${dst}"; then
        write_kernel_version_json_from_content "${dst}"
      fi
      ;;
  esac
}

stage_cubevs_tools() {
  local src="${IMAGE_ROOT}/cube-vs/network/bin/cubevsmapdump"
  local dst_dir="${TOOLBOX_ROOT}/cube-vs/network/bin"
  local dst="${dst_dir}/cubevsmapdump"
  local tmp="${dst}.tmp.$$"
  [[ -f "${src}" ]] || fail "image bypass missing: ${src}"
  mkdir -p "${dst_dir}"
  cp "${src}" "${tmp}"
  chmod +x "${tmp}"
  mv -f "${tmp}" "${dst}"
  [[ -x "${dst}" ]] || fail "missing cubevsmapdump after cube-vs install"
  if [[ -f "${IMAGE_ROOT}/cube-vs/version.json" ]]; then
    cp "${IMAGE_ROOT}/cube-vs/version.json" "${TOOLBOX_ROOT}/cube-vs/version.json.tmp.$$"
    mv -f "${TOOLBOX_ROOT}/cube-vs/version.json.tmp.$$" "${TOOLBOX_ROOT}/cube-vs/version.json"
  fi
  mkdir -p /usr/local/bin
  ln -sf "${dst}" /usr/local/bin/cubevsmapdump
  log "installed cubevsmapdump -> ${dst}"
}

run_install() {
  stage_component "$(component_relpath "${CUBE_COMPONENT}")"
  log "install complete; pausing"
  exec sleep infinity
}

sed_escape_replacement() {
  printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g' -e 's/[/]/\\\//g'
}

detect_primary_interface() {
  ip route get 1.1.1.1 2>/dev/null | awk '
    {
      for (i = 1; i <= NF; i++) {
        if ($i == "dev" && (i + 1) <= NF) {
          print $(i + 1)
          exit
        }
      }
    }'
}

# select_guest_kernel [preserved_target]
# preserved_target is vmlinux-bm|vmlinux-pvm captured before whole-tree replace.
select_guest_kernel() {
  local preserved="${1:-}"
  local dir="${TOOLBOX_ROOT}/cube-kernel-scf"
  local target=""
  local state_dir="${STATE_DIR:-/var/lib/cube-node-bootstrap}"
  # 1) bootstrap effective-pvm wins
  if [[ -f "${state_dir}/effective-pvm" ]]; then
    case "$(tr -d '[:space:]' < "${state_dir}/effective-pvm" 2>/dev/null || true)" in
      1) target="vmlinux-pvm" ;;
      0) target="vmlinux-bm" ;;
    esac
  fi
  # 2) else restore pre-replace / on-disk selection (node history; beats Chart env)
  if [[ -z "${target}" ]]; then
    case "${preserved}" in
      vmlinux-bm|vmlinux-pvm) target="${preserved}" ;;
    esac
  fi
  # 3) else honor CUBE_PVM_ENABLE when explicitly set (first install / Chart intent)
  if [[ -z "${target}" && -n "${CUBE_PVM_ENABLE+x}" ]]; then
    case "${CUBE_PVM_ENABLE}" in
      1|true|TRUE|yes|YES) target="vmlinux-pvm" ;;
      0|false|FALSE|no|NO) target="vmlinux-bm" ;;
    esac
  fi
  # 4) else keep post-replace artifact symlink if already valid
  if [[ -z "${target}" ]]; then
    target="$(kernel_symlink_target "${dir}/vmlinux")"
  fi
  # 5) first-install default
  [[ -n "${target}" ]] || target="vmlinux-bm"
  [[ -f "${dir}/${target}" ]] || fail "missing guest kernel: ${dir}/${target}"
  ln -sfn "${target}" "${dir}/vmlinux"
  mkdir -p "${state_dir}"
  ln -sfn "${dir}/${target}" "${state_dir}/vmlinux-active"
  log "selected guest kernel: ${dir}/vmlinux -> ${target} (vmlinux-active updated)"
}

patch_common_yaml_list() {
  local key="$1"
  local raw_values="$2"
  local conf="${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
  [[ -f "${conf}" ]] || return 0
  [[ -n "${raw_values//[[:space:],;]/}" ]] || return 0
  local tmp_file
  tmp_file="$(mktemp)"
  awk -v key="${key}" -v raw_values="${raw_values}" '
    BEGIN {
      gsub(/[,;]/, " ", raw_values)
      count = split(raw_values, raw, /[[:space:]]+/)
      for (i = 1; i <= count; i++) {
        if (raw[i] != "") values[++value_count] = raw[i]
      }
    }
    function emit(indent,    i) {
      print indent key ":"
      for (i = 1; i <= value_count; i++) print indent "  - " values[i]
    }
    {
      if ($0 ~ ("^[[:space:]]*" key ":")) {
        match($0, /^[[:space:]]*/)
        emit(substr($0, 1, RLENGTH))
        in_block = 1
        next
      }
      if (in_block) {
        if ($0 ~ /^[[:space:]]+- /) next
        in_block = 0
      }
      print
    }
  ' "${conf}" > "${tmp_file}"
  mv -f "${tmp_file}" "${conf}"
}

configure_sandbox_dns() {
  if [[ "${CUBE_SANDBOX_DNS_FOLLOW_NODE:-false}" == "true" && -z "${CUBE_SANDBOX_DNS_SERVERS:-}" ]]; then
    CUBE_SANDBOX_DNS_SERVERS="$(
      awk '
        $1 == "nameserver" {
          ip = $2
          if (ip ~ /^127\./) next
          if (ip ~ /^169\.254\./) next
          if (ip == "::1") next
          if (seen[ip]++) next
          if (n++) printf ","
          printf "%s", ip
        }
      ' /etc/resolv.conf
    )"
    log "sandbox DNS follow-node nameservers: ${CUBE_SANDBOX_DNS_SERVERS:-<empty>}"
    if [[ -z "${CUBE_SANDBOX_DNS_SEARCHES:-}" ]]; then
      CUBE_SANDBOX_DNS_SEARCHES="$(
        awk '
          $1 ~ /^#/ { next }
          $1 == "search" {
            for (i = 2; i <= NF; i++) {
              if ($i ~ /^[#;]/) break
              if (seen[$i]++) continue
              if (n++) printf ","
              printf "%s", $i
            }
          }
        ' /etc/resolv.conf
      )"
      log "sandbox DNS follow-node searches: ${CUBE_SANDBOX_DNS_SEARCHES:-<empty>}"
    fi
    if [[ -z "${CUBE_SANDBOX_DNS_OPTIONS:-}" ]]; then
      CUBE_SANDBOX_DNS_OPTIONS="$(
        awk '
          $1 ~ /^#/ { next }
          $1 == "options" {
            for (i = 2; i <= NF; i++) {
              if ($i ~ /^[#;]/) break
              if (seen[$i]++) continue
              if (n++) printf ","
              printf "%s", $i
            }
          }
        ' /etc/resolv.conf
      )"
      log "sandbox DNS follow-node options: ${CUBE_SANDBOX_DNS_OPTIONS:-<empty>}"
    fi
  fi
  patch_common_yaml_list default_dns_servers "${CUBE_SANDBOX_DNS_SERVERS:-}"
  patch_common_yaml_list default_dns_searches "${CUBE_SANDBOX_DNS_SEARCHES:-}"
  patch_common_yaml_list default_dns_options "${CUBE_SANDBOX_DNS_OPTIONS:-}"
}

write_pidfile() {
  local name="$1"
  local pid="$2"
  local starttime tmp_file
  starttime="$(awk '{print $22}' "/proc/${pid}/stat" 2>/dev/null || true)"
  [[ "${pid}" =~ ^[0-9]+$ && "${starttime}" =~ ^[0-9]+$ ]] \
    || fail "cannot record ${name} process identity pid=${pid}"
  mkdir -p "${CUBE_PID_DIR}"
  tmp_file="${CUBE_PID_DIR}/${name}.pid.tmp.$$"
  printf '%s %s\n' "${pid}" "${starttime}" > "${tmp_file}"
  mv -f "${tmp_file}" "${CUBE_PID_DIR}/${name}.pid"
}

read_pidfile() {
  local name="$1"
  local file="${CUBE_PID_DIR}/${name}.pid"
  local pid starttime extra
  [[ -f "${file}" ]] || return 1
  read -r pid starttime extra < "${file}" || return 1
  [[ "${pid}" =~ ^[0-9]+$ && "${starttime}" =~ ^[0-9]+$ && -z "${extra:-}" ]] \
    || return 1
  printf '%s %s\n' "${pid}" "${starttime}"
}

process_identity_matches() {
  local pid="$1"
  local starttime="$2"
  local expected_bin="$3"
  local current_starttime exe exe_name expected_name
  [[ "${pid}" =~ ^[0-9]+$ && "${starttime}" =~ ^[0-9]+$ ]] || return 1
  current_starttime="$(awk '{print $22}' "/proc/${pid}/stat" 2>/dev/null || true)"
  [[ "${current_starttime}" == "${starttime}" ]] || return 1
  exe="$(readlink "/proc/${pid}/exe" 2>/dev/null || true)"
  exe="${exe% (deleted)}"
  exe_name="${exe##*/}"
  expected_name="${expected_bin##*/}"
  [[ -n "${exe_name}" && "${exe_name}" == "${expected_name}" ]]
}

stop_owned_process() {
  local name="$1"
  local expected_bin="$2"
  local timeout_seconds="${3:-30}"
  local file="${CUBE_PID_DIR}/${name}.pid"
  local identity pid starttime deadline

  [[ -f "${file}" ]] || return 0
  identity="$(read_pidfile "${name}" 2>/dev/null || true)"
  if [[ -z "${identity}" ]]; then
    log "removing invalid ${name} pidfile"
    rm -f "${file}"
    return 0
  fi
  read -r pid starttime <<< "${identity}"
  if ! process_identity_matches "${pid}" "${starttime}" "${expected_bin}"; then
    log "removing stale ${name} pidfile pid=${pid}"
    rm -f "${file}"
    return 0
  fi

  log "stopping ${name} pid=${pid}"
  kill -TERM "${pid}"
  deadline=$((SECONDS + timeout_seconds))
  while process_identity_matches "${pid}" "${starttime}" "${expected_bin}"; do
    if (( SECONDS >= deadline )); then
      log "${name} pid=${pid} did not exit after ${timeout_seconds}s"
      return 1
    fi
    sleep 0.1
  done
  rm -f "${file}"
}

stop_process_identity() {
  local name="$1"
  local pid="$2"
  local starttime="$3"
  local expected_bin="$4"
  local timeout_seconds="${5:-30}"
  local deadline

  process_identity_matches "${pid}" "${starttime}" "${expected_bin}" || return 0
  log "stopping ${name} pid=${pid}"
  kill -TERM "${pid}"
  deadline=$((SECONDS + timeout_seconds))
  while process_identity_matches "${pid}" "${starttime}" "${expected_bin}"; do
    if (( SECONDS >= deadline )); then
      log "${name} pid=${pid} did not exit after ${timeout_seconds}s"
      return 1
    fi
    sleep 0.1
  done
}

# Remove the standalone NetworkAgent left by releases that ran it in a
# separate container. Its pidfile contains a PID from the old container PID
# namespace, so discover it by the exact executable instead of trusting that
# stale PID. Revalidate PID, start time, and executable before signaling.
stop_legacy_network_agents() {
  local expected_bin="${TOOLBOX_ROOT}/network-agent/bin/network-agent"
  local proc pid starttime exe argv0

  for proc in /proc/[0-9]*; do
    pid="${proc##*/}"
    exe="$(readlink "${proc}/exe" 2>/dev/null || true)"
    exe="${exe% (deleted)}"
    case "${exe}" in
      "${expected_bin}"|"${TOOLBOX_ROOT}"/network-agent.legacy.*/bin/network-agent) ;;
      *) continue ;;
    esac
    argv0="$(tr '\0' '\n' < "${proc}/cmdline" 2>/dev/null | head -n1 || true)"
    [[ "${argv0}" == "${expected_bin}" ]] || continue
    starttime="$(awk '{print $22}' "${proc}/stat" 2>/dev/null || true)"
    process_identity_matches "${pid}" "${starttime}" "${expected_bin}" || continue
    log "stopping legacy standalone network-agent pid=${pid}"
    kill -TERM "${pid}"
  done
  rm -f "${CUBE_PID_DIR}/network-agent.pid" "${CUBE_PID_DIR}/network-agent.fingerprint"
}

STARTING_PID=""
STARTING_STARTTIME=""
STARTING_PGID=""
cleanup_starting_process() {
  local status=$?
  local pgid=""
  trap - EXIT TERM INT HUP
  if [[ -n "${STARTING_PGID}" && "${STARTING_PGID}" == "${STARTING_PID}" ]]; then
    pgid="${STARTING_PGID}"
    kill -CONT -- "-${pgid}" 2>/dev/null || true
    kill -TERM -- "-${pgid}" 2>/dev/null || true
  elif [[ -n "${STARTING_PID}" ]] &&
       process_identity_matches "${STARTING_PID}" "${STARTING_STARTTIME}" \
         "$(readlink "/proc/${STARTING_PID}/exe" 2>/dev/null || true)"; then
      kill -CONT "${STARTING_PID}" 2>/dev/null || true
      kill -TERM "${STARTING_PID}" 2>/dev/null || true
  fi
  if [[ -n "${STARTING_PID}" ]]; then
    for _ in $(seq 1 50); do
      if [[ -n "${pgid}" ]]; then
        kill -0 -- "-${pgid}" 2>/dev/null || break
      else
        kill -0 "${STARTING_PID}" 2>/dev/null || break
      fi
      sleep 0.1
    done
    if [[ -n "${pgid}" ]] && kill -0 -- "-${pgid}" 2>/dev/null; then
      kill -KILL -- "-${pgid}" 2>/dev/null || true
    elif kill -0 "${STARTING_PID}" 2>/dev/null; then
      kill -KILL "${STARTING_PID}" 2>/dev/null || true
    fi
  fi
  [[ -z "${STARTING_PID}" ]] || wait "${STARTING_PID}" 2>/dev/null || true
  exit "${status}"
}

# Move Cubelet out of the Kubernetes Pod cgroups before it starts. CubeShim and
# VMM processes inherit this host-level cgroup and therefore are not killed
# when kubelet removes the Big Pod cgroups during a rolling update.
move_pid_to_host_cgroups() {
  local pid="$1"
  local cgroup_root="${CUBE_CGROUP_ROOT:-/sys/fs/cgroup}"
  local controller_root target source_value moved=0 mode=v1

  [[ "${pid}" =~ ^[0-9]+$ ]] || fail "invalid pid for host cgroup: ${pid}"
  [[ "${CUBE_HOST_CGROUP_NAME}" =~ ^[a-zA-Z0-9_.-]+$ ]] \
    || fail "invalid CUBE_HOST_CGROUP_NAME=${CUBE_HOST_CGROUP_NAME}"

  if [[ -f "${cgroup_root}/cgroup.controllers" && -f "${cgroup_root}/cgroup.procs" ]]; then
    mode=v2
    target="${cgroup_root}/${CUBE_HOST_CGROUP_NAME}"
    mkdir -p "${target}"
    printf '%s\n' "${pid}" > "${target}/cgroup.procs" \
      || fail "cannot move pid ${pid} to ${target}"
    grep -qx "${pid}" "${target}/cgroup.procs" \
      || fail "pid ${pid} is not present in ${target}/cgroup.procs after move"
    moved=1
  else
    for controller_root in "${cgroup_root}"/*; do
      [[ -f "${controller_root}/tasks" ]] || continue
      target="${controller_root}/${CUBE_HOST_CGROUP_NAME}"
      mkdir -p "${target}"

      # A cgroup v1 child cpuset cannot accept tasks until both masks are initialized.
      if [[ -f "${controller_root}/cpuset.cpus" ]]; then
        source_value="$(cat "${controller_root}/cpuset.cpus")"
        [[ -n "${source_value}" ]] && printf '%s\n' "${source_value}" > "${target}/cpuset.cpus"
      fi
      if [[ -f "${controller_root}/cpuset.mems" ]]; then
        source_value="$(cat "${controller_root}/cpuset.mems")"
        [[ -n "${source_value}" ]] && printf '%s\n' "${source_value}" > "${target}/cpuset.mems"
      fi

      printf '%s\n' "${pid}" > "${target}/tasks" \
        || fail "cannot move pid ${pid} to ${target}"
      grep -qx "${pid}" "${target}/tasks" \
        || fail "pid ${pid} is not present in ${target}/tasks after move"
      moved=$((moved + 1))
    done
  fi

  [[ "${moved}" -gt 0 ]] || fail "no writable host cgroup hierarchy found"
  log "moved pid=${pid} to host cgroup /${CUBE_HOST_CGROUP_NAME} mode=${mode} hierarchies=${moved}"
}

component_fingerprint() {
  local bin="$1"
  if [[ -n "${CUBE_COMPONENT_FINGERPRINT}" ]]; then
    printf '%s\n' "${CUBE_COMPONENT_FINGERPRINT}"
    return
  fi
  sha256sum "${bin}" | awk '{print $1}'
}

REUSED_HOST_PID=""
REUSED_HOST_STARTTIME=""
reuse_host_process() {
  local name="$1"
  local fingerprint="$2"
  local health_url="$3"
  local pid_file="${CUBE_PID_DIR}/${name}.pid"
  local fingerprint_file="${CUBE_PID_DIR}/${name}.fingerprint"
  local identity pid starttime saved

  REUSED_HOST_PID=""
  REUSED_HOST_STARTTIME=""
  [[ -f "${pid_file}" && -f "${fingerprint_file}" ]] || return 1
  identity="$(read_pidfile "${name}" 2>/dev/null || true)"
  saved="$(cat "${fingerprint_file}" 2>/dev/null || true)"
  [[ -n "${identity}" && "${saved}" == "${fingerprint}" ]] || return 1
  read -r pid starttime <<< "${identity}"
  process_identity_matches "${pid}" "${starttime}" \
    "${TOOLBOX_ROOT}/network-agent/bin/network-agent" || return 1
  curl -fsS "${health_url}" >/dev/null 2>&1 || return 1
  REUSED_HOST_PID="${pid}"
  REUSED_HOST_STARTTIME="${starttime}"
  return 0
}



run_cubelet() {
  local bin="${TOOLBOX_ROOT}/Cubelet/bin/cubelet"
  local cfg="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
  local dyn="${CUBELET_DYNAMICCONF:-${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml}"
  local pid pid_starttime launch

  # Self-stage (no separate cubelet-install container). CubeVS tools are bundled
  # with the cubelet image so cube-node-installer does not need an extra
  # CubeVS installer container.
  stage_component "$(component_relpath cubelet)"
  stage_cubevs_tools
  wait_sentinel "$(component_sentinel cube-shim)" "cube-shim"
  wait_sentinel "$(component_sentinel cube-kernel)" "cube-kernel"
  wait_sentinel "$(component_sentinel cube-guest)" "cube-guest"
  wait_sentinel "$(component_sentinel cube-agent)" "cube-agent"


  [[ -x "${bin}" ]] || fail "missing ${bin}"
  [[ -f "${cfg}" ]] || fail "missing ${cfg}"
  [[ -f "${dyn}" ]] || fail "missing ${dyn}"
  [[ -n "${CUBE_OPS_ENDPOINT:-}" ]] || fail "CUBE_OPS_ENDPOINT is required"
  [[ -n "${CUBE_MASTER_HTTP_ADDR:-}" ]] || fail "CUBE_MASTER_HTTP_ADDR is required"
  [[ -n "${CUBE_SANDBOX_NODE_ID:-}${CUBE_SANDBOX_NODE_IP:-}" ]] || fail "CUBE_SANDBOX_NODE_ID or CUBE_SANDBOX_NODE_IP is required"
  [[ -n "${CUBE_SANDBOX_ENDPOINT_IP:-}" ]] || fail "CUBE_SANDBOX_ENDPOINT_IP is required"

  apply_effective_pvm_from_state
  select_guest_kernel "$(preserve_guest_kernel_selection "${TOOLBOX_ROOT}/cube-kernel-scf")"

  local ops_esc master_http_esc
  ops_esc="$(sed_escape_replacement "${CUBE_OPS_ENDPOINT}")"
  master_http_esc="$(sed_escape_replacement "${CUBE_MASTER_HTTP_ADDR}")"
  sed -i -e "s#^\([[:space:]]*meta_server_endpoint:[[:space:]]*\).*#\1\"${ops_esc}\"#" "${dyn}"
  sed -i -e "s#^\([[:space:]]*cubemaster_http_addr:[[:space:]]*\).*#\1\"${master_http_esc}\"#" "${dyn}"
  configure_sandbox_dns

  if [[ -z "${CUBE_SANDBOX_ETH_NAME:-}" && "${CUBE_SANDBOX_AUTO_DETECT_ETH:-true}" == "true" ]]; then
    CUBE_SANDBOX_ETH_NAME="$(detect_primary_interface || true)"
  fi
  if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
    local eth_esc
    eth_esc="$(sed_escape_replacement "${CUBE_SANDBOX_ETH_NAME}")"
    sed -i "s/eth_name = \"[^\"]*\"/eth_name = \"${eth_esc}\"/" "${cfg}"
  fi
  if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR:-}" ]]; then
    local cidr_esc
    cidr_esc="$(sed_escape_replacement "${CUBE_SANDBOX_NETWORK_CIDR}")"
    sed -i "s|cidr = \"[^\"]*\"|cidr = \"${cidr_esc}\"|" "${cfg}"
  fi
  if [[ -n "${CUBE_EGRESS_ADMIN_PORT:-}" ]]; then
    [[ "${CUBE_EGRESS_ADMIN_PORT}" =~ ^[0-9]+$ ]] || fail "CUBE_EGRESS_ADMIN_PORT must be a positive integer"
    local egress_admin_url="http://127.0.0.1:${CUBE_EGRESS_ADMIN_PORT}"
    local egress_admin_url_esc
    egress_admin_url_esc="$(sed_escape_replacement "${egress_admin_url}")"
    sed -i "s|cube_egress_admin_url = \"[^\"]*\"|cube_egress_admin_url = \"${egress_admin_url_esc}\"|" "${cfg}"
  fi
  if [[ -n "${CUBE_ROUTER_ENABLE:-}" ]]; then
    local router_enable="false"
    [[ "${CUBE_ROUTER_ENABLE}" == "1" || "${CUBE_ROUTER_ENABLE}" == "true" ]] \
      && router_enable="true"
    sed -i -E \
      "s/^([[:space:]]*)cube_router_enable = .*/\\1cube_router_enable = ${router_enable}/" \
      "${cfg}"
  fi
  if [[ -n "${CUBE_ROUTER_CIDR:-}" ]]; then
    local router_cidr_esc
    router_cidr_esc="$(sed_escape_replacement "${CUBE_ROUTER_CIDR}")"
    sed -i -E \
      "s|^([[:space:]]*)cube_router_cidr = \"[^\"]*\"|\\1cube_router_cidr = \"${router_cidr_esc}\"|" \
      "${cfg}"
  fi
  if [[ -n "${CUBE_TAP_INIT_NUM:-}" ]]; then
    [[ "${CUBE_TAP_INIT_NUM}" =~ ^[0-9]+$ ]] || fail "CUBE_TAP_INIT_NUM must be a non-negative integer"
    sed -i "s/tap_init_num = [0-9]\+/tap_init_num = ${CUBE_TAP_INIT_NUM}/" "${cfg}"
  fi
  if [[ -n "${CUBE_CGROUP_POOL_SIZE:-}" ]]; then
    [[ "${CUBE_CGROUP_POOL_SIZE}" =~ ^[0-9]+$ ]] || fail "CUBE_CGROUP_POOL_SIZE must be a non-negative integer"
    sed -i "s/pool_size = [0-9]\+/pool_size = ${CUBE_CGROUP_POOL_SIZE}/" "${cfg}"
  fi
  if [[ -n "${CUBE_WORKFLOW_CONCURRENT:-}" ]]; then
    [[ "${CUBE_WORKFLOW_CONCURRENT}" =~ ^[0-9]+$ ]] || fail "CUBE_WORKFLOW_CONCURRENT must be a non-negative integer"
    sed -i "s/concurrent = [0-9]\+/concurrent = ${CUBE_WORKFLOW_CONCURRENT}/g" "${cfg}"
  fi

  mkdir -p \
    /tmp/cube \
    /data/log/Cubelet \
    /data/log/CubeShim \
    /data/log/CubeVmm \
    /data/cube-shim/disks \
    /data/snapshot_pack/disks \
    /data/cubelet/state \
    "${TOOLBOX_ROOT}/cube-snapshot" \
    "${TOOLBOX_ROOT}/cube-vs/network"
  [[ -x "${TOOLBOX_ROOT}/cube-vs/network/bin/cubevsmapdump" ]] || fail "missing cubevsmapdump after cube-vs stage"
  mkdir -p /usr/local/bin
  ln -sf "${TOOLBOX_ROOT}/cube-vs/network/bin/cubevsmapdump" /usr/local/bin/cubevsmapdump

  if ! findmnt --mountpoint /data/cubelet/state >/dev/null 2>&1; then
    mount --bind /data/cubelet/state /data/cubelet/state
    log "bound /data/cubelet/state to hostPath (skip state tmpfs)"
  fi

  stop_legacy_network_agents

  stop_owned_process cubelet "${bin}" 30 \
    || fail "old cubelet did not exit; refusing to start a second instance"

  log "starting cubelet node_id=${CUBE_SANDBOX_NODE_ID:-} endpoint=${CUBE_SANDBOX_ENDPOINT_IP}"
  # Use a private process group so a failed pre-start can clean up any children
  # without signalling unrelated host processes.
  setsid bash -c '
    kill -STOP "$$"
    exec "$@"
  ' cube-component-launcher \
    "${bin}" --config "${cfg}" --dynamic-conf-path "${dyn}" &
  launch=$!
  STARTING_PID="${launch}"
  STARTING_STARTTIME="$(awk '{print $22}' "/proc/${launch}/stat")"
  trap cleanup_starting_process EXIT TERM INT HUP
  for i in $(seq 1 50); do
    grep -q '^State:.*T' "/proc/${launch}/status" 2>/dev/null && break
    [[ "${i}" -lt 50 ]] || fail "cubelet launcher did not stop for host cgroup move"
    sleep 0.02
  done
  STARTING_PGID="${launch}"
  move_pid_to_host_cgroups "${launch}"
  kill -CONT "${launch}"

  for i in $(seq 1 60); do
    listener="$(ss -lntp 2>/dev/null || true)"
    listener_pid="$(awk '
      /:9999[[:space:]]/ && match($0, /pid=[0-9]+,/) {
        value=substr($0, RSTART + 4, RLENGTH - 5)
        print value
        exit
      }
    ' <<< "${listener}")"
    if [[ -n "${listener_pid}" ]]; then
      listener_pgid="$(ps -o pgid= -p "${listener_pid}" 2>/dev/null | tr -d '[:space:]')"
      listener_starttime="$(awk '{print $22}' "/proc/${listener_pid}/stat" 2>/dev/null || true)"
      if [[ "${listener_pgid}" == "${launch}" ]] &&
         process_identity_matches "${listener_pid}" "${listener_starttime}" "${bin}"; then
        pid="${listener_pid}"
        pid_starttime="${listener_starttime}"
        write_pidfile cubelet "${pid}"
        log "cubelet ready pid=${pid}"
        break
      fi
    fi
    if ! kill -0 -- "-${launch}" >/dev/null 2>&1; then
      fail "cubelet exited before listening on 9999"
    fi
    [[ "${i}" -lt 60 ]] || fail "cubelet did not become ready"
    sleep 1
  done

  STARTING_PID=""
  STARTING_STARTTIME=""
  STARTING_PGID=""
  trap - EXIT TERM INT HUP
  cleanup() {
    stop_process_identity cubelet "${pid}" "${pid_starttime}" "${bin}" 30 \
      || log "cubelet did not exit before shutdown timeout"
    if [[ "$(read_pidfile cubelet 2>/dev/null || true)" == "${pid} ${pid_starttime}" ]]; then
      rm -f "${CUBE_PID_DIR}/cubelet.pid"
    fi
  }
  trap cleanup TERM INT HUP EXIT

  while process_identity_matches "${pid}" \
    "$(awk '{print $2}' "${CUBE_PID_DIR}/cubelet.pid" 2>/dev/null || echo 0)" \
    "${bin}"; do
    sleep 10
  done
  fail "cubelet exited"
}

main() {
  if [[ "${1:-}" == "stop-owned-process" ]]; then
    [[ "$#" -eq 3 ]] || fail "usage: stop-owned-process NAME EXPECTED_BIN"
    stop_owned_process "$2" "$3" 30
    return
  fi
  [[ -n "${CUBE_COMPONENT}" ]] || fail "CUBE_COMPONENT is required"
  case "${CUBE_ROLE}" in
    install) run_install ;;
    run)
      case "${CUBE_COMPONENT}" in
        cubelet) run_cubelet ;;
        *) fail "CUBE_ROLE=run not supported for ${CUBE_COMPONENT}" ;;
      esac
      ;;
    *) fail "unknown CUBE_ROLE=${CUBE_ROLE}" ;;
  esac
}

if [[ "${CUBE_COMPONENT_ENTRYPOINT_LIB_ONLY:-false}" != "true" ]]; then
  main "$@"
fi
