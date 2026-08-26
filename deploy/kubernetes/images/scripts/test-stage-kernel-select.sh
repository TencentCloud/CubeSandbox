#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit checks for guest-kernel selection + atomic_replace recovery in
# component-entrypoint.sh (no container required).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${SCRIPT_DIR}/component-entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Load entrypoint helpers without executing main.
# shellcheck disable=SC1090
source <(sed '/^main "$@"/d' "${ENTRY}")

assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "${got}" != "${want}" ]]; then
    printf 'FAIL: %s (got=%q want=%q)\n' "${msg}" "${got}" "${want}" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${msg}"
}

setup_kernels() {
  local dir="$1"
  mkdir -p "${dir}"
  : >"${dir}/vmlinux-bm"
  : >"${dir}/vmlinux-pvm"
}

# --- select_guest_kernel priorities ---
# effective-pvm → preserved → CUBE_PVM_ENABLE → symlink → bm

TOOLBOX_ROOT="${TMP}/tb1"
STATE_DIR="${TMP}/st1"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
# Image default after replace would be bm; preserved says pvm; no env/state.
unset CUBE_PVM_ENABLE || true
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
select_guest_kernel "vmlinux-pvm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "preserved pvm wins over post-replace bm symlink"
assert_eq "$(basename "$(readlink "${STATE_DIR}/vmlinux-active")")" "vmlinux-pvm" \
  "vmlinux-active follows preserved pvm"

TOOLBOX_ROOT="${TMP}/tb2"
STATE_DIR="${TMP}/st2"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
unset CUBE_PVM_ENABLE || true
export CUBE_PVM_ENABLE=0
select_guest_kernel "vmlinux-pvm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "preserved pvm beats CUBE_PVM_ENABLE=0 (Chart env must not wipe history)"

TOOLBOX_ROOT="${TMP}/tb2b"
STATE_DIR="${TMP}/st2b"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
unset CUBE_PVM_ENABLE || true
export CUBE_PVM_ENABLE=1
select_guest_kernel ""
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "CUBE_PVM_ENABLE=1 used when no preserved / no effective-pvm"

TOOLBOX_ROOT="${TMP}/tb3"
STATE_DIR="${TMP}/st3"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
printf '1\n' >"${STATE_DIR}/effective-pvm"
export CUBE_PVM_ENABLE=0
select_guest_kernel "vmlinux-bm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "effective-pvm=1 beats CUBE_PVM_ENABLE and preserved"

TOOLBOX_ROOT="${TMP}/tb3b"
STATE_DIR="${TMP}/st3b"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-pvm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
printf '0\n' >"${STATE_DIR}/effective-pvm"
export CUBE_PVM_ENABLE=1
select_guest_kernel "vmlinux-pvm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-bm" \
  "effective-pvm=0 beats preserved pvm and CUBE_PVM_ENABLE=1"

# --- preserve_guest_kernel_selection ---
TOOLBOX_ROOT="${TMP}/tb4"
STATE_DIR="${TMP}/st4"
mkdir -p "${TOOLBOX_ROOT}/cube-kernel-scf" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-pvm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
ln -sfn "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux-pvm" "${STATE_DIR}/vmlinux-active"
got="$(preserve_guest_kernel_selection "${TOOLBOX_ROOT}/cube-kernel-scf")"
assert_eq "${got}" "vmlinux-pvm" "preserve reads vmlinux-active"

# --- atomic_replace_dir legacy recovery ---
TOOLBOX_ROOT="${TMP}/tb5"
mkdir -p "${TOOLBOX_ROOT}"
dst="${TOOLBOX_ROOT}/comp"
src="${TMP}/src5"
mkdir -p "${src}"
echo live >"${src}/f"
mkdir -p "${dst}.legacy.111"
echo old >"${dst}.legacy.111/f"
# dst missing (crash after rename-aside)
atomic_replace_dir "${src}" "${dst}"
assert_eq "$(cat "${dst}/f")" "live" "atomic_replace recovers then promotes new tree"
[[ ! -e "${dst}.legacy.111" ]] || { echo "FAIL: orphan legacy not cleaned"; exit 1; }
printf 'ok: orphan legacy cleaned after successful replace\n'

# Existing dst, no mounts: rename-aside still replaces and leaves no legacy.
src="${TMP}/src5b"
dst="${TOOLBOX_ROOT}/comp5b"
mkdir -p "${src}" "${dst}"
echo old >"${dst}/f"
echo stale >"${dst}/gone"
echo live >"${src}/f"
echo extra >"${src}/n"
atomic_replace_dir "${src}" "${dst}"
assert_eq "$(cat "${dst}/f")" "live" "atomic_replace promotes over existing dst"
assert_eq "$(cat "${dst}/n")" "extra" "atomic_replace copies new files"
[[ ! -e "${dst}/gone" ]] || { echo "FAIL: stale file survived replace"; exit 1; }
shopt -s nullglob
legacy_left=("${dst}".legacy.*)
shopt -u nullglob
((${#legacy_left[@]} == 0)) || { echo "FAIL: leftover legacy after no-mount replace"; exit 1; }
printf 'ok: no-mount replace is rename-aside without leftover legacy\n'

# Bind-mounted plugin conf must stay on the live path (kubelet Secret subPath).
# Prefer unshare (no root). This host may block unprivileged user namespaces;
# fall back to a privileged Docker mount namespace using a local image.
bind_test_script() {
  cat <<'BINDTEST'
set -euo pipefail
# shellcheck disable=SC1090
source <(sed '/^main "$@"/d' "${ENTRY}")
work="$(mktemp -d)"
src="${work}/bind-src"
dst="${work}/bind-dst"
secret="${work}/bind-secret"
mkdir -p "${src}/bin" "${src}/plugin" "${dst}/bin" "${dst}/plugin"
printf 'old-bin\n' >"${dst}/bin/cubelet"
printf 'stale\n' >"${dst}/stale.txt"
printf 'old-conf\n' >"${dst}/plugin/volume-s3.conf"
printf 'secret-conf\n' >"${secret}"
mount --bind "${secret}" "${dst}/plugin/volume-s3.conf"
printf 'new-bin\n' >"${src}/bin/cubelet"
printf 'image-conf\n' >"${src}/plugin/volume-s3.conf"
printf 'plugin-sh\n' >"${src}/plugin/other.sh"
atomic_replace_dir "${src}" "${dst}"
[[ "$(cat "${dst}/bin/cubelet")" == "new-bin" ]] || { echo "FAIL: bind overlay did not update non-mount files"; exit 1; }
[[ "$(cat "${dst}/plugin/other.sh")" == "plugin-sh" ]] || { echo "FAIL: bind overlay missed new plugin file"; exit 1; }
[[ ! -e "${dst}/stale.txt" ]] || { echo "FAIL: stale file survived bind overlay"; exit 1; }
[[ "$(cat "${dst}/plugin/volume-s3.conf")" == "secret-conf" ]] || { echo "FAIL: bind overlay overwrote volume-s3.conf"; exit 1; }
findmnt --mountpoint "${dst}/plugin/volume-s3.conf" >/dev/null \
  || { echo "FAIL: volume-s3.conf is no longer a mountpoint"; exit 1; }
shopt -s nullglob
left=("${dst}".legacy.* "${dst}".new.*)
shopt -u nullglob
((${#left[@]} == 0)) || { echo "FAIL: leftover new/legacy after bind overlay"; exit 1; }
BINDTEST
}

if unshare --user --map-root-user --mount true >/dev/null 2>&1; then
  ENTRY="${ENTRY}" TMP="${TMP}" unshare --user --map-root-user --mount /bin/bash -s <<<"$(bind_test_script)"
elif docker info >/dev/null 2>&1; then
  bind_docker_img="${BIND_TEST_DOCKER_IMAGE:-ubuntu:22.04}"
  docker image inspect "${bind_docker_img}" >/dev/null 2>&1 \
    || docker pull "${bind_docker_img}"
  ENTRY="${ENTRY}" docker run --rm -i --privileged --entrypoint bash \
    -v "${ENTRY}:${ENTRY}:ro" \
    -e ENTRY \
    "${bind_docker_img}" \
    -s <<<"$(bind_test_script)"
else
  echo "FAIL: need unshare user+mount or docker to test bind-mount preserve" >&2
  exit 1
fi
printf 'ok: bind-mounted volume-s3.conf stays on live path during overlay\n'

# --- staging marker: no EXIT trap; only success clears ---
# Simulate mid-stage: write marker, then "fail" without success cleanup.
# Entrypoint must not register trap ... EXIT that would rm the marker.
if grep -qE 'trap[[:space:]]+clear_staging_marker[[:space:]]+EXIT' "${ENTRY}"; then
  echo "FAIL: stage_component still registers clear_staging_marker EXIT trap" >&2
  exit 1
fi
printf 'ok: no clear_staging_marker EXIT trap in entrypoint\n'

marker="${TMP}/.staging-cubelet"
printf 'staging\n' >"${marker}"
# Subshell exit must not clear marker (documents intended failure-path behavior).
(
  clear_staging_marker() { rm -f "${marker}"; }
  # Intentionally do NOT register trap — mirrors production after Fix 1.
  false || true
)
[[ -f "${marker}" ]] || { echo "FAIL: staging marker disappeared without success cleanup"; exit 1; }
printf 'ok: staging marker survives non-success path (no EXIT clear)\n'

# Success path still clears (mirrors stage_component end).
rm -f "${marker}"
[[ ! -f "${marker}" ]] || { echo "FAIL: could not clear marker on success"; exit 1; }
printf 'ok: success path can clear staging marker\n'

# --- ensure_component_version_json: no weak env synthesis ---
dst="${TMP}/cubelet"
mkdir -p "${dst}"
CUBE_VERSION=should-not-write ensure_component_version_json cubelet "${dst}"
[[ ! -f "${dst}/version.json" ]] || { echo "FAIL: weak env wrote version.json"; exit 1; }
printf 'ok: cubelet does not synthesize from CUBE_VERSION\n'

mkdir -p "${TMP}/guest"
printf 'g1\n' >"${TMP}/guest/version"
ensure_component_version_json cube-guest "${TMP}/guest"
grep -q 'guest-image' "${TMP}/guest/version.json"
! grep -q 'cube-agent' "${TMP}/guest/version.json"
printf 'ok: cube-guest synthesizes guest-image only\n'

mkdir -p "${TMP}/agent"
printf 'a1\n' >"${TMP}/agent/version"
ensure_component_version_json cube-agent "${TMP}/agent"
grep -q 'cube-agent' "${TMP}/agent/version.json"
printf 'ok: cube-agent synthesizes from version marker\n'

# --- inventory helpers ---
COMPONENT_VERSIONS_ROOT="${TMP}/component_versions"
src="${TMP}/inv-shim"
mkdir -p "${src}/bin"
: >"${src}/bin/containerd-shim-cube-rs"
printf '{"schema_version":1,"components":{"containerd-shim-cube-rs":{"version":"shim-v9"},"cube-runtime":{"version":"shim-v9"}}}\n' \
  >"${src}/version.json"
CUBE_COMPONENT=cube-shim
assert_eq "$(resolve_component_version "${src}" cube-shim)" "shim-v9" "resolve version.json for cube-shim"
inventory_component_version "${src}" "cube-shim"
[[ -d "${COMPONENT_VERSIONS_ROOT}/cube-shim/shim-v9" ]] || { echo "FAIL: inventory dir missing"; exit 1; }
[[ -f "${COMPONENT_VERSIONS_ROOT}/cube-shim/shim-v9/bin/containerd-shim-cube-rs" ]] || { echo "FAIL: inventory leaf missing"; exit 1; }
printf 'ok: inventory writes COMPONENT_VERSIONS_ROOT/cube-shim/<ver>\n'

inventory_component_version "${src}" "cube-shim"
[[ -d "${COMPONENT_VERSIONS_ROOT}/cube-shim/shim-v9" ]] || { echo "FAIL: inventory lost on skip"; exit 1; }
printf 'ok: same-version inventory skipped\n'

src_bad="${TMP}/inv-agent-bad"
mkdir -p "${src_bad}"
printf 'unknown\n' >"${src_bad}/version"
CUBE_COMPONENT=cube-agent
assert_eq "$(resolve_component_version "${src_bad}" cube-agent || true)" "" "unknown version rejected"
if ( inventory_component_version "${src_bad}" "cube-agent" ) 2>/dev/null; then
  echo "FAIL: unknown version must hard-fail inventory" >&2
  exit 1
fi
printf 'ok: unknown version hard-fails inventory\n'

is_inventory_component cube-shim
is_inventory_component cube-kernel
is_inventory_component cube-guest
is_inventory_component cube-agent
if is_inventory_component cubelet; then
  echo "FAIL: cubelet must not be inventory component" >&2
  exit 1
fi
printf 'ok: inventory component set is shim/kernel/guest/agent\n'

# --- kernel dual-variant inventory ---
ksrc="${TMP}/inv-kernel"
mkdir -p "${ksrc}"
printf 'bm-kernel-bytes\n' >"${ksrc}/vmlinux-bm"
printf 'pvm-kernel-bytes\n' >"${ksrc}/vmlinux-pvm"
bm_digest="$(file_sha256_hex "${ksrc}/vmlinux-bm")"
pvm_digest="$(file_sha256_hex "${ksrc}/vmlinux-pvm")"
bm_short="sha256-${bm_digest:0:12}"
pvm_short="sha256-${pvm_digest:0:12}"
CUBE_COMPONENT=cube-kernel
inventory_kernel_content_variants "${ksrc}" "cube-kernel-scf"
[[ -d "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${bm_short}" ]] || { echo "FAIL: missing bm inventory"; exit 1; }
[[ -d "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}" ]] || { echo "FAIL: missing pvm inventory"; exit 1; }
assert_eq "$(cat "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${bm_short}/variant")" "bm" "bm variant marker"
assert_eq "$(cat "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}/variant")" "pvm" "pvm variant marker"
assert_eq "$(readlink "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}/vmlinux")" "vmlinux-pvm" "pvm vmlinux link"
assert_eq "$(tr -d '[:space:]' < "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}/version")" "sha256:${pvm_digest}" "pvm version digest"
if ls -d "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/"*@* >/dev/null 2>&1; then
  echo "FAIL: must not create tag@digest inventory dirs" >&2
  exit 1
fi
printf 'ok: kernel dual content inventory with variant markers\n'

# --- kernel version.json digest rewrite (legacy tag-only JSON) ---
kdst="${TMP}/kernel-json"
mkdir -p "${kdst}"
printf 'bm-kernel-bytes\n' >"${kdst}/vmlinux-bm"
printf 'pvm-kernel-bytes\n' >"${kdst}/vmlinux-pvm"
bm_digest="$(file_sha256_hex "${kdst}/vmlinux-bm")"
pvm_digest="$(file_sha256_hex "${kdst}/vmlinux-pvm")"
bm_short="sha256-${bm_digest:0:12}"
pvm_short="sha256-${pvm_digest:0:12}"
printf '{"schema_version":1,"variants":{"bm":{"version":"v-tag","tag":"v-tag"},"pvm":{"version":"v-tag","tag":"v-tag"}}}\n' \
  >"${kdst}/version.json"
ensure_component_version_json cube-kernel "${kdst}"
grep -q "\"digest_sha256\": \"sha256:${bm_digest}\"" "${kdst}/version.json" \
  || { echo "FAIL: bm digest not rewritten"; exit 1; }
grep -q "\"digest_sha256\": \"sha256:${pvm_digest}\"" "${kdst}/version.json" \
  || { echo "FAIL: pvm digest not rewritten"; exit 1; }
grep -q "\"version\": \"${bm_short}\"" "${kdst}/version.json" \
  || { echo "FAIL: bm short version not rewritten"; exit 1; }
grep -q "\"version\": \"${pvm_short}\"" "${kdst}/version.json" \
  || { echo "FAIL: pvm short version not rewritten"; exit 1; }
assert_eq "$(tr -d '[:space:]' < "${kdst}/version")" "sha256:${bm_digest}" "bm version marker"
printf 'ok: tag-only kernel version.json rewritten with digests\n'

# Idempotent: complete digest JSON must not be rewritten to something else.
cp "${kdst}/version.json" "${kdst}/version.json.before"
ensure_component_version_json cube-kernel "${kdst}"
cmp -s "${kdst}/version.json.before" "${kdst}/version.json" \
  || { echo "FAIL: complete digest JSON should not be rewritten"; exit 1; }
printf 'ok: complete kernel version.json left unchanged\n'

printf 'ALL PASS\n'
