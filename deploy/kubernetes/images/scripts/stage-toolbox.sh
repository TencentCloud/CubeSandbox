#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Stage the cube-node image toolbox onto a hostPath directory so that
# containerd-shim-cube-rs / cube-runtime binaries live on a filesystem that is
# never unmounted when the cube-node Pod is deleted or upgraded.
#
# This mirrors one-click install.sh upgrade semantics:
#   stop services -> selectively rm package trees -> cp new binaries -> restart
# Runtime data under the install prefix is left alone (same as one-click, which
# does NOT rm cube-snapshot / cubebox_os_image / cubeletmnt).
# Running shims keep their already-mapped inodes after unlink; the hostPath
# filesystem itself is never torn down, so microVMs survive Pod rebuilds.
set -euo pipefail

IMAGE_TOOLBOX_ROOT="${IMAGE_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
HOST_TOOLBOX_ROOT="${HOST_TOOLBOX_ROOT:-/host-toolbox}"

# Top-level names that one-click install.sh leaves across upgrade. Chart must
# not wipe them when refreshing package binaries from the cube-node image.
PRESERVE_NAMES=(
  cube-snapshot
  cubebox_os_image
  cubeletmnt
  cube-vs
)

log() { printf '[stage-toolbox] %s\n' "$*"; }
fail() { printf '[stage-toolbox] ERROR: %s\n' "$*" >&2; exit 1; }

should_preserve() {
  local name="$1"
  local preserved
  for preserved in "${PRESERVE_NAMES[@]}"; do
    if [[ "${name}" == "${preserved}" ]]; then
      return 0
    fi
  done
  return 1
}

[[ -d "${IMAGE_TOOLBOX_ROOT}" ]] || fail "image toolbox missing: ${IMAGE_TOOLBOX_ROOT}"
[[ -n "${HOST_TOOLBOX_ROOT}" ]] || fail "HOST_TOOLBOX_ROOT is empty"
[[ "${HOST_TOOLBOX_ROOT}" != "/" ]] || fail "refusing to stage onto /"
[[ "${HOST_TOOLBOX_ROOT}" != "${IMAGE_TOOLBOX_ROOT}" ]] || fail "HOST_TOOLBOX_ROOT must differ from IMAGE_TOOLBOX_ROOT so the image copy stays visible"

mkdir -p "${HOST_TOOLBOX_ROOT}"

log "staging ${IMAGE_TOOLBOX_ROOT} -> ${HOST_TOOLBOX_ROOT} (selective overwrite, preserve runtime dirs)"

# Remove only non-preserved top-level entries. Running shims that still
# reference unlinked package inodes keep running; new cubelet uses the fresh copy.
while IFS= read -r -d '' entry; do
  name="$(basename "${entry}")"
  if should_preserve "${name}"; then
    log "preserving ${name}"
    continue
  fi
  rm -rf "${entry}"
done < <(find "${HOST_TOOLBOX_ROOT}" -mindepth 1 -maxdepth 1 -print0)

# Copy package trees from the image. Do not replace preserved host dirs that
# already exist (e.g. a populated cube-snapshot) with empty image stubs.
while IFS= read -r -d '' entry; do
  name="$(basename "${entry}")"
  dest="${HOST_TOOLBOX_ROOT}/${name}"
  if should_preserve "${name}" && [[ -e "${dest}" ]]; then
    log "skipping image copy over preserved ${name}"
    continue
  fi
  rm -rf "${dest}"
  cp -a "${entry}" "${dest}"
done < <(find "${IMAGE_TOOLBOX_ROOT}" -mindepth 1 -maxdepth 1 -print0)

# Keep the same executable bits one-click install.sh enforces.
chmod +x \
  "${HOST_TOOLBOX_ROOT}/Cubelet/bin/cubelet" \
  "${HOST_TOOLBOX_ROOT}/Cubelet/bin/cubecli" \
  "${HOST_TOOLBOX_ROOT}/network-agent/bin/network-agent" \
  "${HOST_TOOLBOX_ROOT}/network-agent/bin/cubevsmapdump" \
  "${HOST_TOOLBOX_ROOT}/cube-shim/bin/cube-runtime" \
  "${HOST_TOOLBOX_ROOT}/cube-shim/bin/containerd-shim-cube-rs" \
  2>/dev/null || true

for required in \
  Cubelet/bin/cubelet \
  network-agent/bin/network-agent \
  cube-shim/bin/containerd-shim-cube-rs \
  cube-shim/bin/cube-runtime
do
  [[ -x "${HOST_TOOLBOX_ROOT}/${required}" ]] || fail "missing executable after stage: ${required}"
done

log "toolbox staged successfully at ${HOST_TOOLBOX_ROOT}"
