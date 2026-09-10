#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Guard: cubebox_os_image softlink helper migrates real dirs, is idempotent,
# and respects CUBE_CUBEBOX_OS_IMAGE_ON_DATA=0.
set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/cubebox_os_image.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

toolbox="${TMP_DIR}/toolbox"
data="${TMP_DIR}/data/cubebox_os_image"
mkdir -p "${toolbox}"

CUBE_CUBEBOX_OS_IMAGE_ON_DATA=1
ensure_cubebox_os_image_on_data "${toolbox}" "${data}"
[[ -L "${toolbox}/cubebox_os_image" ]] || {
  echo "expected symlink at ${toolbox}/cubebox_os_image" >&2
  exit 1
}
[[ "$(readlink "${toolbox}/cubebox_os_image")" == "${data}" ]] || {
  echo "symlink target mismatch" >&2
  exit 1
}
[[ -d "${data}" ]] || {
  echo "data dir missing" >&2
  exit 1
}

ensure_cubebox_os_image_on_data "${toolbox}" "${data}"

toolbox2="${TMP_DIR}/toolbox2"
data2="${TMP_DIR}/data2/cubebox_os_image"
mkdir -p "${toolbox2}/cubebox_os_image"
echo payload >"${toolbox2}/cubebox_os_image/keep.txt"
ensure_cubebox_os_image_on_data "${toolbox2}" "${data2}"
[[ -L "${toolbox2}/cubebox_os_image" ]] || {
  echo "migration should leave a symlink" >&2
  exit 1
}
[[ -f "${data2}/keep.txt" ]] || {
  echo "migration should move content to data dir" >&2
  exit 1
}

toolbox_retarget="${TMP_DIR}/toolbox-retarget"
data_root="${TMP_DIR}/data-retarget"
old_data="${data_root}/cubebox_os_image_old"
new_data="${data_root}/cubebox_os_image"
mkdir -p "${toolbox_retarget}" "${old_data}"
echo cached >"${old_data}/cached.ext4"
ln -s "${old_data}" "${toolbox_retarget}/cubebox_os_image"
ensure_cubebox_os_image_on_data "${toolbox_retarget}" "${new_data}"
[[ -L "${toolbox_retarget}/cubebox_os_image" ]] || {
  echo "retarget should leave a symlink" >&2
  exit 1
}
[[ "$(readlink "${toolbox_retarget}/cubebox_os_image")" == "${new_data}" ]] || {
  echo "retarget should point at the new data dir" >&2
  exit 1
}
[[ -f "${new_data}/cached.ext4" ]] || {
  echo "retarget should migrate trusted symlink content" >&2
  exit 1
}

toolbox3="${TMP_DIR}/toolbox3"
mkdir -p "${toolbox3}"
CUBE_CUBEBOX_OS_IMAGE_ON_DATA=0
ensure_cubebox_os_image_on_data "${toolbox3}" "${TMP_DIR}/data3/cubebox_os_image"
[[ ! -e "${toolbox3}/cubebox_os_image" ]] || {
  echo "disabled toggle must not create cubebox_os_image" >&2
  exit 1
}

# Failed merge-copy must preserve the source directory (no silent rm -rf).
# Use a stub `cp` so the failure is deterministic even when running as root
# (root bypasses chmod a-w on the destination).
toolbox_cpfail="${TMP_DIR}/toolbox-cpfail"
data_cpfail="${TMP_DIR}/data-cpfail/cubebox_os_image"
bin_stub="${TMP_DIR}/bin-stub"
mkdir -p "${toolbox_cpfail}/cubebox_os_image" "${data_cpfail}" "${bin_stub}"
echo keep >"${toolbox_cpfail}/cubebox_os_image/keep.txt"
cat >"${bin_stub}/cp" <<'EOF'
#!/bin/sh
echo "stub cp failing for test" >&2
exit 1
EOF
chmod +x "${bin_stub}/cp"
CUBE_CUBEBOX_OS_IMAGE_ON_DATA=1
if PATH="${bin_stub}:${PATH}" ensure_cubebox_os_image_on_data "${toolbox_cpfail}" "${data_cpfail}"; then
  echo "ensure should fail when merge-copy cannot write" >&2
  exit 1
fi
[[ -f "${toolbox_cpfail}/cubebox_os_image/keep.txt" ]] || {
  echo "failed migrate must leave source cubebox_os_image intact" >&2
  exit 1
}
[[ ! -L "${toolbox_cpfail}/cubebox_os_image" ]] || {
  echo "failed migrate must not replace source with symlink" >&2
  exit 1
}

echo "cubebox_os_image softlink helper OK"
