#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
ENV_FILE="${ONE_CLICK_ENV_FILE:-${SCRIPT_DIR}/.env}"
if [[ -f "${ENV_FILE}" ]]; then
  load_env_file "${ENV_FILE}"
fi

WORK_ROOT="${ONE_CLICK_WORK_ROOT:-${SCRIPT_DIR}/.work}"
RUNTIME_LAYOUT_DIR="${ONE_CLICK_RUNTIME_LAYOUT_DIR:-${WORK_ROOT}/runtime-layout}"
CORE_BIN_DIR="${WORK_ROOT}/core-bin"
PACKAGE_ROOT="${WORK_ROOT}/sandbox-package"
PACKAGE_TAR="${WORK_ROOT}/sandbox-package.tar.gz"
RAW_ARTIFACTS_DIR="${SCRIPT_DIR}/assets/kernel-artifacts"
CUBE_PROXY_TEMPLATE_DIR="${SCRIPT_DIR}/cubeproxy"
CUBE_COREDNS_TEMPLATE_DIR="${SCRIPT_DIR}/coredns"
CUBE_SUPPORT_TEMPLATE_DIR="${SCRIPT_DIR}/support"
CUBE_WEBUI_TEMPLATE_DIR="${SCRIPT_DIR}/webui"
CUBE_PROXY_SOURCE_DIR="${ONE_CLICK_CUBE_PROXY_SOURCE_DIR:-${ROOT_DIR}/CubeProxy}"
WEB_SOURCE_DIR="${ONE_CLICK_WEB_SOURCE_DIR:-${ROOT_DIR}/web}"
WEB_DIST_OVERRIDE="${ONE_CLICK_WEB_DIST_DIR:-}"
MKCERT_BIN_ASSET="${ONE_CLICK_MKCERT_BIN:-${SCRIPT_DIR}/assets/bin/mkcert}"
CUBE_KERNEL_VMLINUX="${ONE_CLICK_CUBE_KERNEL_VMLINUX:-${RAW_ARTIFACTS_DIR}/vmlinux}"
KERNEL_ARTIFACT_ZIP="${WORK_ROOT}/cube-kernel-scf.zip"
DIST_VERSION="${ONE_CLICK_DIST_VERSION:-$(latest_git_revision "${ROOT_DIR}")}"
DIST_ROOT="${SCRIPT_DIR}/dist/cube-sandbox-one-click-${DIST_VERSION}"
DIST_TAR="${SCRIPT_DIR}/dist/cube-sandbox-one-click-${DIST_VERSION}.tar.gz"

CUBEMASTER_BUILD_MODE="${ONE_CLICK_CUBEMASTER_BUILD_MODE:-local}"
CUBELET_BUILD_MODE="${ONE_CLICK_CUBELET_BUILD_MODE:-local}"
API_BUILD_MODE="${ONE_CLICK_CUBE_API_BUILD_MODE:-local}"
NETWORK_AGENT_BUILD_MODE="${ONE_CLICK_NETWORK_AGENT_BUILD_MODE:-local}"

CUBEMASTER_BIN_OVERRIDE="${ONE_CLICK_CUBEMASTER_BIN:-}"
CUBEMASTERCLI_BIN_OVERRIDE="${ONE_CLICK_CUBEMASTERCLI_BIN:-}"
CUBELET_BIN_OVERRIDE="${ONE_CLICK_CUBELET_BIN:-}"
CUBECLI_BIN_OVERRIDE="${ONE_CLICK_CUBECLI_BIN:-}"
API_BIN_OVERRIDE="${ONE_CLICK_CUBE_API_BIN:-}"
NETWORK_AGENT_BIN_OVERRIDE="${ONE_CLICK_NETWORK_AGENT_BIN:-}"

build_go_binary() {
  local workdir="$1"
  local mode="$2"
  local output="$3"
  shift 3
  case "${mode}" in
    local)
      require_cmd go
      (cd "${workdir}" && go mod download && go build -o "${output}" "$@") >&2
      ;;
    *)
      die "unsupported build mode: ${mode}"
      ;;
  esac
}

build_rust_binary() {
  local workdir="$1"
  local mode="$2"
  local binary_name="$3"
  local output="$4"
  case "${mode}" in
    local)
      require_cmd cargo
      (cd "${workdir}" && cargo build --release --locked --bin "${binary_name}") >&2
      copy_file "${workdir}/target/release/${binary_name}" "${output}"
      ;;
    *)
      die "unsupported build mode: ${mode}"
      ;;
  esac
}

package_kernel_artifact_zip() {
  local src_vmlinux="$1"
  local output_zip="$2"
  require_cmd python3
  python3 - "${src_vmlinux}" "${output_zip}" <<'PY'
import os
import sys
import zipfile

src_path = sys.argv[1]
zip_path = sys.argv[2]

os.makedirs(os.path.dirname(zip_path), exist_ok=True)
with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED) as zf:
    zf.write(src_path, arcname="vmlinux")
PY
}

build_web_dist() {
  local output_dir="$1"
  rm -rf "${output_dir}"
  mkdir -p "${output_dir}"

  if [[ -n "${WEB_DIST_OVERRIDE}" ]]; then
    log "using prebuilt web dist: ${WEB_DIST_OVERRIDE}"
    ensure_dir "${WEB_DIST_OVERRIDE}"
    copy_dir_contents "${WEB_DIST_OVERRIDE}" "${output_dir}"
  else
    log "building web dashboard"
    require_cmd npm
    ensure_dir "${WEB_SOURCE_DIR}"
    (cd "${WEB_SOURCE_DIR}" && npm ci && npm run build) >&2
    copy_dir_contents "${WEB_SOURCE_DIR}/dist" "${output_dir}"
  fi

  ensure_file "${output_dir}/index.html"
}

ensure_kernel_vmlinux "${CUBE_KERNEL_VMLINUX}" "${RAW_ARTIFACTS_DIR}"
ensure_dir "${CUBE_PROXY_TEMPLATE_DIR}"
ensure_dir "${CUBE_COREDNS_TEMPLATE_DIR}"
ensure_dir "${CUBE_SUPPORT_TEMPLATE_DIR}"
ensure_dir "${CUBE_WEBUI_TEMPLATE_DIR}"
ensure_dir "${CUBE_PROXY_SOURCE_DIR}"

log "building runtime layout"
"${SCRIPT_DIR}/build-vm-assets.sh"

log "packaging fixed kernel artifact zip"
package_kernel_artifact_zip "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux" "${KERNEL_ARTIFACT_ZIP}"

rm -rf "${CORE_BIN_DIR}" "${PACKAGE_ROOT}" "${PACKAGE_TAR}" "${DIST_ROOT}" "${DIST_TAR}"
mkdir -p "${CORE_BIN_DIR}"

if [[ -z "${CUBEMASTER_BIN_OVERRIDE}" ]]; then
  log "building cubemaster"
  build_go_binary "${ROOT_DIR}/CubeMaster" "${CUBEMASTER_BUILD_MODE}" "${CORE_BIN_DIR}/cubemaster" ./cmd/cubemaster
else
  log "using prebuilt cubemaster: ${CUBEMASTER_BIN_OVERRIDE}"
  copy_file "${CUBEMASTER_BIN_OVERRIDE}" "${CORE_BIN_DIR}/cubemaster"
fi

if [[ -z "${CUBEMASTERCLI_BIN_OVERRIDE}" ]]; then
  log "building cubemastercli"
  build_go_binary "${ROOT_DIR}/CubeMaster" "${CUBEMASTER_BUILD_MODE}" "${CORE_BIN_DIR}/cubemastercli" ./cmd/cubemastercli
else
  log "using prebuilt cubemastercli: ${CUBEMASTERCLI_BIN_OVERRIDE}"
  copy_file "${CUBEMASTERCLI_BIN_OVERRIDE}" "${CORE_BIN_DIR}/cubemastercli"
fi

if [[ -z "${CUBELET_BIN_OVERRIDE}" ]]; then
  log "building cubelet"
  build_go_binary "${ROOT_DIR}/Cubelet" "${CUBELET_BUILD_MODE}" "${CORE_BIN_DIR}/cubelet" ./cmd/cubelet
else
  log "using prebuilt cubelet: ${CUBELET_BIN_OVERRIDE}"
  copy_file "${CUBELET_BIN_OVERRIDE}" "${CORE_BIN_DIR}/cubelet"
fi

if [[ -z "${CUBECLI_BIN_OVERRIDE}" ]]; then
  log "building cubecli"
  build_go_binary "${ROOT_DIR}/Cubelet" "${CUBELET_BUILD_MODE}" "${CORE_BIN_DIR}/cubecli" ./cmd/cubecli
else
  log "using prebuilt cubecli: ${CUBECLI_BIN_OVERRIDE}"
  copy_file "${CUBECLI_BIN_OVERRIDE}" "${CORE_BIN_DIR}/cubecli"
fi

if [[ -z "${API_BIN_OVERRIDE}" ]]; then
  log "building cube-api"
  build_rust_binary "${ROOT_DIR}/CubeAPI" "${API_BUILD_MODE}" "cube-api" "${CORE_BIN_DIR}/cube-api"
else
  log "using prebuilt cube-api: ${API_BIN_OVERRIDE}"
  copy_file "${API_BIN_OVERRIDE}" "${CORE_BIN_DIR}/cube-api"
fi

if [[ -z "${NETWORK_AGENT_BIN_OVERRIDE}" ]]; then
  log "building network-agent"
  build_go_binary "${ROOT_DIR}/network-agent" "${NETWORK_AGENT_BUILD_MODE}" "${CORE_BIN_DIR}/network-agent" ./cmd/network-agent
else
  log "using prebuilt network-agent: ${NETWORK_AGENT_BIN_OVERRIDE}"
  copy_file "${NETWORK_AGENT_BIN_OVERRIDE}" "${CORE_BIN_DIR}/network-agent"
fi

mkdir -p \
  "${PACKAGE_ROOT}/network-agent/bin" \
  "${PACKAGE_ROOT}/network-agent/state" \
  "${PACKAGE_ROOT}/CubeAPI/bin" \
  "${PACKAGE_ROOT}/CubeMaster/bin" \
  "${PACKAGE_ROOT}/Cubelet/bin" \
  "${PACKAGE_ROOT}/Cubelet/config" \
  "${PACKAGE_ROOT}/Cubelet/dynamicconf" \
  "${PACKAGE_ROOT}/cubeproxy" \
  "${PACKAGE_ROOT}/coredns" \
  "${PACKAGE_ROOT}/webui" \
  "${PACKAGE_ROOT}/webui/dist" \
  "${PACKAGE_ROOT}/support" \
  "${PACKAGE_ROOT}/support/bin" \
  "${PACKAGE_ROOT}/cube-vs/network" \
  "${PACKAGE_ROOT}/cube-snapshot" \
  "${PACKAGE_ROOT}/scripts/one-click" \
  "${PACKAGE_ROOT}/sql"

copy_file "${CORE_BIN_DIR}/network-agent" "${PACKAGE_ROOT}/network-agent/bin/network-agent"
copy_file "${ROOT_DIR}/configs/single-node/network-agent.yaml" "${PACKAGE_ROOT}/network-agent/network-agent.yaml"

copy_file "${CORE_BIN_DIR}/cube-api" "${PACKAGE_ROOT}/CubeAPI/bin/cube-api"

copy_file "${CORE_BIN_DIR}/cubemaster" "${PACKAGE_ROOT}/CubeMaster/bin/cubemaster"
copy_file "${CORE_BIN_DIR}/cubemastercli" "${PACKAGE_ROOT}/CubeMaster/bin/cubemastercli"
copy_file "${ROOT_DIR}/configs/single-node/cubemaster.yaml" "${PACKAGE_ROOT}/CubeMaster/conf.yaml"

copy_file "${CORE_BIN_DIR}/cubelet" "${PACKAGE_ROOT}/Cubelet/bin/cubelet"
copy_file "${CORE_BIN_DIR}/cubecli" "${PACKAGE_ROOT}/Cubelet/bin/cubecli"
if [[ -f "${ROOT_DIR}/Cubelet/contrib/nicl" ]]; then
  copy_file "${ROOT_DIR}/Cubelet/contrib/nicl" "${PACKAGE_ROOT}/Cubelet/bin/nicl"
  chmod +x "${PACKAGE_ROOT}/Cubelet/bin/nicl"
fi
if [[ -f "${ROOT_DIR}/Cubelet/contrib/cubelet-code-deploy.sh" ]]; then
  copy_file "${ROOT_DIR}/Cubelet/contrib/cubelet-code-deploy.sh" "${PACKAGE_ROOT}/Cubelet/bin/cubelet-code-deploy.sh"
  chmod +x "${PACKAGE_ROOT}/Cubelet/bin/cubelet-code-deploy.sh"
fi
copy_dir_contents "${ROOT_DIR}/Cubelet/config" "${PACKAGE_ROOT}/Cubelet/config"
copy_dir_contents "${ROOT_DIR}/Cubelet/dynamicconf" "${PACKAGE_ROOT}/Cubelet/dynamicconf"

copy_dir_contents "${CUBE_PROXY_TEMPLATE_DIR}" "${PACKAGE_ROOT}/cubeproxy"
copy_dir_contents "${CUBE_COREDNS_TEMPLATE_DIR}" "${PACKAGE_ROOT}/coredns"
copy_dir_contents "${CUBE_WEBUI_TEMPLATE_DIR}" "${PACKAGE_ROOT}/webui"
copy_dir_contents "${CUBE_PROXY_SOURCE_DIR}" "${PACKAGE_ROOT}/cubeproxy/build-context"
copy_file "${CUBE_PROXY_TEMPLATE_DIR}/Dockerfile.oneclick" "${PACKAGE_ROOT}/cubeproxy/build-context/Dockerfile.oneclick"
rm -f \
  "${PACKAGE_ROOT}/cubeproxy/Dockerfile.oneclick" \
  "${PACKAGE_ROOT}/cubeproxy/build-context/Dockerfile" \
  "${PACKAGE_ROOT}/cubeproxy/build-context/Makefile"
build_web_dist "${PACKAGE_ROOT}/webui/dist"
copy_dir_contents "${CUBE_SUPPORT_TEMPLATE_DIR}" "${PACKAGE_ROOT}/support"
copy_file "${MKCERT_BIN_ASSET}" "${PACKAGE_ROOT}/support/bin/mkcert"

copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-shim" "${PACKAGE_ROOT}/cube-shim"
copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf" "${PACKAGE_ROOT}/cube-kernel-scf"
copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-image" "${PACKAGE_ROOT}/cube-image"

copy_dir_contents "${SCRIPT_DIR}/scripts/one-click" "${PACKAGE_ROOT}/scripts/one-click"
copy_dir_contents "${SCRIPT_DIR}/sql" "${PACKAGE_ROOT}/sql"

find "${PACKAGE_ROOT}" -type f -path "*/bin/*" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/one-click" -type f -name "*.sh" -exec chmod +x {} \;

mkdir -p "$(dirname "${PACKAGE_TAR}")"
tar -C "${WORK_ROOT}" -czf "${PACKAGE_TAR}" "sandbox-package"

mkdir -p "${DIST_ROOT}/assets/package" "${DIST_ROOT}/assets/kernel-artifacts" "${DIST_ROOT}/lib"
copy_file "${SCRIPT_DIR}/README.md" "${DIST_ROOT}/README.md"
copy_file "${SCRIPT_DIR}/install.sh" "${DIST_ROOT}/install.sh"
copy_file "${SCRIPT_DIR}/install-compute.sh" "${DIST_ROOT}/install-compute.sh"
copy_file "${SCRIPT_DIR}/down.sh" "${DIST_ROOT}/down.sh"
copy_file "${SCRIPT_DIR}/smoke.sh" "${DIST_ROOT}/smoke.sh"
copy_file "${SCRIPT_DIR}/online-install.sh" "${DIST_ROOT}/online-install.sh"
copy_file "${SCRIPT_DIR}/env.example" "${DIST_ROOT}/env.example"
copy_file "${SCRIPT_DIR}/lib/common.sh" "${DIST_ROOT}/lib/common.sh"
copy_file "${PACKAGE_TAR}" "${DIST_ROOT}/assets/package/sandbox-package.tar.gz"
copy_file "${KERNEL_ARTIFACT_ZIP}" "${DIST_ROOT}/assets/kernel-artifacts/cube-kernel-scf.zip"
chmod +x \
  "${DIST_ROOT}/install.sh" \
  "${DIST_ROOT}/install-compute.sh" \
  "${DIST_ROOT}/down.sh" \
  "${DIST_ROOT}/smoke.sh" \
  "${DIST_ROOT}/online-install.sh"

cat > "${DIST_ROOT}/VERSION.txt" <<EOF
repo=${ROOT_DIR}
revision=${DIST_VERSION}
built_at=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
EOF

tar -C "${SCRIPT_DIR}/dist" -czf "${DIST_TAR}" "cube-sandbox-one-click-${DIST_VERSION}"
log "release bundle ready: ${DIST_TAR}"
