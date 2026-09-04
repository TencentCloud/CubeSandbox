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

# Empty pre-created dest (DirectoryOrCreate) must take the mv fast path.
toolbox_empty="${TMP_DIR}/toolbox-empty"
data_empty="${TMP_DIR}/data-empty/cubebox_os_image"
mkdir -p "${toolbox_empty}/cubebox_os_image" "${data_empty}"
echo via-mv >"${toolbox_empty}/cubebox_os_image/via-mv.txt"
ensure_cubebox_os_image_on_data "${toolbox_empty}" "${data_empty}"
[[ -L "${toolbox_empty}/cubebox_os_image" ]] || {
  echo "empty-dest migrate should leave a symlink" >&2
  exit 1
}
[[ -f "${data_empty}/via-mv.txt" ]] || {
  echo "empty-dest migrate should mv content into data dir" >&2
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
[[ ! -e "${old_data}" ]] || {
  echo "retarget should remove previous trusted data dir after success" >&2
  exit 1
}

# Aliased spellings of the same data dir must not attempt self-migrate.
toolbox_alias="${TMP_DIR}/toolbox-alias"
data_alias_real="${TMP_DIR}/data-alias/cubebox_os_image"
mkdir -p "${toolbox_alias}" "${data_alias_real}"
echo kept >"${data_alias_real}/kept.txt"
ln -s "${data_alias_real}" "${toolbox_alias}/cubebox_os_image"
# Pass a path that resolves to the same directory via /./
ensure_cubebox_os_image_on_data "${toolbox_alias}" "${TMP_DIR}/data-alias/./cubebox_os_image"
[[ -L "${toolbox_alias}/cubebox_os_image" ]] || {
  echo "aliased data dir should keep a symlink" >&2
  exit 1
}
[[ -f "${data_alias_real}/kept.txt" ]] || {
  echo "aliased data dir must not self-migrate/destroy content" >&2
  exit 1
}

# Unrelated dir under same parent (e.g. /data/cubelet) must NOT be migrated/deleted.
toolbox_untrusted="${TMP_DIR}/toolbox-untrusted"
data_untrusted_root="${TMP_DIR}/data-untrusted"
untrusted="${data_untrusted_root}/cubelet"
new_untrusted="${data_untrusted_root}/cubebox_os_image"
mkdir -p "${toolbox_untrusted}" "${untrusted}"
echo live >"${untrusted}/state.txt"
ln -s "${untrusted}" "${toolbox_untrusted}/cubebox_os_image"
ensure_cubebox_os_image_on_data "${toolbox_untrusted}" "${new_untrusted}"
[[ -L "${toolbox_untrusted}/cubebox_os_image" ]] || {
  echo "untrusted retarget should still leave a symlink" >&2
  exit 1
}
[[ "$(readlink "${toolbox_untrusted}/cubebox_os_image")" == "${new_untrusted}" ]] || {
  echo "untrusted retarget should re-point without migrating" >&2
  exit 1
}
[[ -f "${untrusted}/state.txt" ]] || {
  echo "untrusted target must not be deleted" >&2
  exit 1
}
[[ ! -f "${new_untrusted}/state.txt" ]] || {
  echo "untrusted target must not be copied into data dir" >&2
  exit 1
}

# Failed retarget migrate must keep the prior symlink (WARN+continue contract).
toolbox_retarget_fail="${TMP_DIR}/toolbox-retarget-fail"
data_root_fail="${TMP_DIR}/data-retarget-fail"
old_fail="${data_root_fail}/cubebox_os_image_old"
new_fail="${data_root_fail}/cubebox_os_image"
bin_stub_retarget="${TMP_DIR}/bin-stub-retarget"
mkdir -p "${toolbox_retarget_fail}" "${old_fail}" "${new_fail}" "${bin_stub_retarget}"
echo cached >"${old_fail}/cached.ext4"
echo existing >"${new_fail}/existing.txt"
ln -s "${old_fail}" "${toolbox_retarget_fail}/cubebox_os_image"
cat >"${bin_stub_retarget}/cp" <<'EOF'
#!/bin/sh
echo "stub cp failing for retarget test" >&2
exit 1
EOF
chmod +x "${bin_stub_retarget}/cp"
if PATH="${bin_stub_retarget}:${PATH}" ensure_cubebox_os_image_on_data "${toolbox_retarget_fail}" "${new_fail}"; then
  echo "retarget ensure should fail when merge-copy cannot write" >&2
  exit 1
fi
[[ -L "${toolbox_retarget_fail}/cubebox_os_image" ]] || {
  echo "failed retarget must keep prior symlink" >&2
  exit 1
}
[[ "$(readlink "${toolbox_retarget_fail}/cubebox_os_image")" == "${old_fail}" ]] || {
  echo "failed retarget must keep prior symlink target" >&2
  exit 1
}
[[ -f "${old_fail}/cached.ext4" ]] || {
  echo "failed retarget must leave prior data intact" >&2
  exit 1
}

toolbox3="${TMP_DIR}/toolbox3"
mkdir -p "${toolbox3}"
unset CUBE_CUBEBOX_OS_IMAGE_ON_DATA
ensure_cubebox_os_image_on_data "${toolbox3}" "${TMP_DIR}/data3/cubebox_os_image"
[[ ! -e "${toolbox3}/cubebox_os_image" ]] || {
  echo "default (unset) toggle must not create cubebox_os_image" >&2
  exit 1
}

# Failed merge-copy must preserve the source directory (no silent rm -rf).
# Destination must already hold content so migrate takes the cp path (not mv).
# Use a stub `cp` so the failure is deterministic even when running as root
# (root bypasses chmod a-w on the destination).
toolbox_cpfail="${TMP_DIR}/toolbox-cpfail"
data_cpfail="${TMP_DIR}/data-cpfail/cubebox_os_image"
bin_stub="${TMP_DIR}/bin-stub"
mkdir -p "${toolbox_cpfail}/cubebox_os_image" "${data_cpfail}" "${bin_stub}"
echo keep >"${toolbox_cpfail}/cubebox_os_image/keep.txt"
echo existing >"${data_cpfail}/existing.txt"
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
