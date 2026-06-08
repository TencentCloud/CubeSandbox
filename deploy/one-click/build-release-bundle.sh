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
CUBE_SYSTEMD_TEMPLATE_DIR="${SCRIPT_DIR}/systemd"
CUBE_PROXY_SOURCE_DIR="${ONE_CLICK_CUBE_PROXY_SOURCE_DIR:-${ROOT_DIR}/CubeProxy}"
CUBE_EGRESS_SOURCE_DIR="${ONE_CLICK_CUBE_EGRESS_SOURCE_DIR:-${ROOT_DIR}/CubeEgress}"
WEB_SOURCE_DIR="${ONE_CLICK_WEB_SOURCE_DIR:-${ROOT_DIR}/web}"
WEB_DIST_OVERRIDE="${ONE_CLICK_WEB_DIST_DIR:-}"
MKCERT_BIN_ASSET="${ONE_CLICK_MKCERT_BIN:-${SCRIPT_DIR}/assets/bin/mkcert}"
CUBE_KERNEL_VMLINUX="${ONE_CLICK_CUBE_KERNEL_VMLINUX:-${RAW_ARTIFACTS_DIR}/vmlinux}"
KERNEL_ARTIFACT_ZIP="${WORK_ROOT}/cube-kernel-scf.zip"

CUBE_VERSION_FROM_ENV="${CUBE_VERSION:-}"
LATEST_RELEASE_TAG="$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
: "${CUBE_VERSION:=${LATEST_RELEASE_TAG:-0.0.0-dev}}"
: "${CUBE_COMMIT:=$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')}"
: "${CUBE_BUILD_TIME:=$(date -u +'%Y-%m-%dT%H:%M:%SZ')}"
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME

DIST_VERSION="${ONE_CLICK_DIST_VERSION:-${CUBE_VERSION_FROM_ENV:-${LATEST_RELEASE_TAG:-$(latest_git_revision "${ROOT_DIR}")}}}"
DIST_ROOT="${SCRIPT_DIR}/dist/cube-sandbox-one-click-${DIST_VERSION}"
DIST_TAR="${SCRIPT_DIR}/dist/cube-sandbox-one-click-${DIST_VERSION}.tar.gz"

CUBEMASTER_BUILD_MODE="${ONE_CLICK_CUBEMASTER_BUILD_MODE:-local}"
CUBELET_BUILD_MODE="${ONE_CLICK_CUBELET_BUILD_MODE:-local}"
API_BUILD_MODE="${ONE_CLICK_CUBE_API_BUILD_MODE:-local}"
NETWORK_AGENT_BUILD_MODE="${ONE_CLICK_NETWORK_AGENT_BUILD_MODE:-local}"
CUBEVSMAPDUMP_BUILD_MODE="${ONE_CLICK_CUBEVSMAPDUMP_BUILD_MODE:-local}"
CUBE_PROXY_SIDECAR_BUILD_MODE="${ONE_CLICK_CUBE_PROXY_SIDECAR_BUILD_MODE:-local}"

CUBEMASTER_BIN_OVERRIDE="${ONE_CLICK_CUBEMASTER_BIN:-}"
CUBEMASTERCLI_BIN_OVERRIDE="${ONE_CLICK_CUBEMASTERCLI_BIN:-}"
CUBELET_BIN_OVERRIDE="${ONE_CLICK_CUBELET_BIN:-}"
CUBECLI_BIN_OVERRIDE="${ONE_CLICK_CUBECLI_BIN:-}"
API_BIN_OVERRIDE="${ONE_CLICK_CUBE_API_BIN:-}"
NETWORK_AGENT_BIN_OVERRIDE="${ONE_CLICK_NETWORK_AGENT_BIN:-}"
CUBEVSMAPDUMP_BIN_OVERRIDE="${ONE_CLICK_CUBEVSMAPDUMP_BIN:-}"
CUBE_PROXY_SIDECAR_BIN_OVERRIDE="${ONE_CLICK_CUBE_PROXY_SIDECAR_BIN:-}"

go_version_ldflags() {
  local version_pkg="$1"
  printf -- "-s -w -X '%s.Version=%s' -X '%s.Commit=%s' -X '%s.BuildTime=%s'" \
    "${version_pkg}" "${CUBE_VERSION}" \
    "${version_pkg}" "${CUBE_COMMIT}" \
    "${version_pkg}" "${CUBE_BUILD_TIME}"
}

build_go_binary() {
  local workdir="$1"
  local mode="$2"
  local output="$3"
  local version_pkg="$4"
  shift 4

  local ldflags="-s -w"
  if [[ -n "${version_pkg}" ]]; then
    ldflags="$(go_version_ldflags "${version_pkg}")"
  fi

  case "${mode}" in
    local)
      require_cmd go
      (cd "${workdir}" && go mod download && go build -ldflags "${ldflags}" -o "${output}" "$@") >&2
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

build_or_copy_go_binary() {
  local name="$1"
  local override_path="$2"
  local workdir="$3"
  local mode="$4"
  local output="$5"
  local package="$6"
  local version_pkg="${7:-}"  # optional: Go import path for ldflags injection

  if [[ -n "${override_path}" ]]; then
    log "using prebuilt ${name}: ${override_path}"
    copy_file "${override_path}" "${output}"
    return 0
  fi

  log "building ${name}"
  build_go_binary "${workdir}" "${mode}" "${output}" "${version_pkg}" "${package}"
}

build_or_copy_rust_binary() {
  local name="$1"
  local override_path="$2"
  local workdir="$3"
  local mode="$4"
  local output="$5"

  if [[ -n "${override_path}" ]]; then
    log "using prebuilt ${name}: ${override_path}"
    copy_file "${override_path}" "${output}"
    return 0
  fi

  log "building ${name}"
  build_rust_binary "${workdir}" "${mode}" "${name}" "${output}"
}

# ---------------------------------------------------------------------------
# generate_release_manifest
#
# Produces a machine-readable release-manifest.json in DIST_ROOT with:
#   - release_version (the git tag or DIST_VERSION)
#   - per-component version / commit / build_time / sha256 digest
#   - guest-image version + digest
#   - kernel version metadata
#
# Prerequisites: CORE_BIN_DIR and RUNTIME_LAYOUT_DIR must be fully populated.
# Call after build-vm-assets.sh completes and all binaries are in place.
# ---------------------------------------------------------------------------
generate_release_manifest() {
  local dist_root="$1"
  local release_version="$2"
  local output="${dist_root}/release-manifest.json"

  require_cmd python3

  log "generating release manifest: ${output}"

  local cube_version="${CUBE_VERSION}"
  local cube_commit="${CUBE_COMMIT}"
  local cube_build_time="${CUBE_BUILD_TIME}"

  # Guest-image version file (single line, read by CubeShim::get_image_version()).
  local guest_image_version="unknown"
  local guest_image_ver_file="${RUNTIME_LAYOUT_DIR}/cube-image/version"
  if [[ -f "${guest_image_ver_file}" ]]; then
    guest_image_version="$(head -n1 "${guest_image_ver_file}" | tr -d '[:space:]')"
  fi

  local guest_agent_version="${cube_version}"
  local guest_agent_ver_file="${RUNTIME_LAYOUT_DIR}/cube-image/agent-version"
  if [[ -f "${guest_agent_ver_file}" ]]; then
    guest_agent_version="$(head -n1 "${guest_agent_ver_file}" | tr -d '[:space:]')"
  fi

  # Guest-image path
  local guest_image_path="${RUNTIME_LAYOUT_DIR}/cube-image/cube-guest-image-cpu.img"

  # Kernel paths
  local kernel_vmlinux="${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-bm"
  local kernel_pvm_vmlinux="${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-pvm"

  # Kernel versions (use CI env or hardcoded tags from release-one-click.yml).
  local kernel_version="${KERNEL_TAG:-unknown}"
  local kernel_pvm_version="${PVM_KERNEL_TAG:-unknown}"
  if [[ "${kernel_version}" == "unknown" ]]; then
    log "WARNING: KERNEL_TAG is not set; release manifest will record kernel.version=unknown"
  fi
  if [[ -f "${kernel_pvm_vmlinux}" && "${kernel_pvm_version}" == "unknown" ]]; then
    log "WARNING: PVM_KERNEL_TAG is not set; release manifest will record kernel.pvm_version=unknown"
  fi

  # Agent binary: prefer CI override, then search known build output paths.
  local agent_bin="${ONE_CLICK_CUBE_AGENT_BIN:-}"
  if [[ -z "${agent_bin}" ]]; then
    for candidate in \
      "${ROOT_DIR}/agent/target/x86_64-unknown-linux-musl/release/cube-agent" \
      "${ROOT_DIR}/agent/target/release/cube-agent"; do
      if [[ -f "${candidate}" ]]; then
        agent_bin="${candidate}"
        break
      fi
    done
  fi

  # Shim + runtime binaries: already copied to RUNTIME_LAYOUT_DIR by build-vm-assets.sh.
  local shim_bin="${RUNTIME_LAYOUT_DIR}/cube-shim/bin/containerd-shim-cube-rs"
  local runtime_bin="${RUNTIME_LAYOUT_DIR}/cube-shim/bin/cube-runtime"

  python3 - "${output}" "${release_version}" "${cube_version}" "${cube_commit}" "${cube_build_time}" \
      "${guest_image_version}" "${guest_agent_version}" "${kernel_version}" "${kernel_pvm_version}" \
      "${CORE_BIN_DIR}" \
      "${agent_bin}" "${shim_bin}" "${runtime_bin}" \
      "${guest_image_path}" "${kernel_vmlinux}" "${kernel_pvm_vmlinux}" <<'PY'
import json, os, sys, hashlib

output_path       = sys.argv[1]
release_version   = sys.argv[2]
cube_version      = sys.argv[3]
cube_commit       = sys.argv[4]
cube_build_time   = sys.argv[5]
guest_image_ver   = sys.argv[6]
guest_agent_ver   = sys.argv[7]
kernel_version    = sys.argv[8]
kernel_pvm_version = sys.argv[9]
core_bin_dir      = sys.argv[10]
agent_bin         = sys.argv[11]
shim_bin          = sys.argv[12]
runtime_bin       = sys.argv[13]
guest_image_path  = sys.argv[14]
kernel_vmlinux    = sys.argv[15]
kernel_pvm_vmlinux = sys.argv[16] if len(sys.argv) > 16 else ""

def sha256_hex(path):
    """Return sha256:hexdigest for an existing file."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            chunk = f.read(65536)
            if not chunk:
                break
            h.update(chunk)
    return "sha256:" + h.hexdigest()

def required_sha256(path):
    if not path or not os.path.isfile(path):
        raise FileNotFoundError(f"required release artifact is missing: {path}")
    return sha256_hex(path)

def optional_sha256(path):
    if not path or not os.path.isfile(path):
        return None
    return sha256_hex(path)

components = {}

# ── Go binaries from CORE_BIN_DIR ──
for name in ["cubemaster", "cubemastercli", "cubelet", "cubecli", "network-agent"]:
    path = os.path.join(core_bin_dir, name)
    components[name] = {
        "version": cube_version,
        "commit": cube_commit,
        "build_time": cube_build_time,
        "digest_sha256": required_sha256(path),
    }

# ── cube-api from CORE_BIN_DIR ──
components["cube-api"] = {
    "version": cube_version,
    "commit": cube_commit,
    "build_time": cube_build_time,
    "digest_sha256": required_sha256(os.path.join(core_bin_dir, "cube-api")),
}

# ── Rust binaries from build-vm-assets.sh ──
components["cube-agent"] = {
    "version": cube_version,
    "commit": cube_commit,
    "build_time": cube_build_time,
    "digest_sha256": required_sha256(agent_bin),
}
components["containerd-shim-cube-rs"] = {
    "version": cube_version,
    "commit": cube_commit,
    "build_time": cube_build_time,
    "digest_sha256": required_sha256(shim_bin),
}
components["cube-runtime"] = {
    "version": cube_version,
    "commit": cube_commit,
    "build_time": cube_build_time,
    "digest_sha256": required_sha256(runtime_bin),
}

# ── Docker-based components (no single-binary digest) ──
components["cube-egress"] = {
    "version": cube_version,
    "commit": cube_commit,
    "build_time": cube_build_time,
}

# ── Guest image ──
guest_image = {
    "version": guest_image_ver,
    "digest_sha256": required_sha256(guest_image_path),
    "base_image": os.environ.get("ONE_CLICK_GUEST_IMAGE_REF", "cube-sandbox-guest-image:one-click"),
    "agent_version": guest_agent_ver,
}

# ── Kernel ──
kernel = {"version": kernel_version}
if kernel_vmlinux:
    kernel["vmlinux_digest_sha256"] = required_sha256(kernel_vmlinux)
pvm_digest = optional_sha256(kernel_pvm_vmlinux)
if pvm_digest:
    kernel["pvm_version"] = kernel_pvm_version
    kernel["vmlinux_pvm_digest_sha256"] = pvm_digest

manifest = {
    "release_version": release_version,
    "built_at": cube_build_time,
    "built_by": "github-actions" if os.environ.get("GITHUB_ACTIONS") == "true" else "manual",
    "git_commit": cube_commit,
    "components": components,
    "guest_image": guest_image,
    "kernel": kernel,
}

if not kernel.get("vmlinux_digest_sha256"):
    raise ValueError("missing kernel vmlinux digest")

os.makedirs(os.path.dirname(output_path), exist_ok=True)
with open(output_path, "w") as f:
    json.dump(manifest, f, indent=2)
    f.write("\n")
PY

  ensure_file "${output}"
  log "release manifest written: ${output}"
}

package_kernel_artifact_zip() {
  local src_vmlinux="$1"
  local output_zip="$2"
  local src_pvm_vmlinux="${3:-}"
  require_cmd python3
  python3 - "${src_vmlinux}" "${output_zip}" "${src_pvm_vmlinux}" <<'PY'
import os
import sys
import zipfile

src_path = sys.argv[1]
zip_path = sys.argv[2]
pvm_src_path = sys.argv[3] if len(sys.argv) > 3 else ""

os.makedirs(os.path.dirname(zip_path), exist_ok=True)
with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED) as zf:
    zf.write(src_path, arcname="vmlinux")
    zf.write(src_path, arcname="vmlinux-bm")
    if pvm_src_path and os.path.isfile(pvm_src_path):
        zf.write(pvm_src_path, arcname="vmlinux-pvm")
PY
}

generate_cube_proxy_nginx_template() {
  local src="$1"
  local dst="$2"
  ensure_file "${src}"
  mkdir -p "$(dirname "${dst}")"

  local header
  header=$(cat <<'EOF'
# NOTE: This file is auto-generated by deploy/one-click/build-release-bundle.sh
# from CubeProxy/nginx.conf. DO NOT edit by hand; modify the upstream
# CubeProxy/nginx.conf and re-run the bundle build.
#
# The configuration below is provided for reference only and is NOT meant for
# production use as-is. Tune worker_processes, buffer/limit sizes, timeouts and
# certificate paths according to your environment.
#
# The HTTP server block (port __CUBE_PROXY_HTTP_PORT__) intentionally proxies to
# the backend instead of redirecting to HTTPS, because some upstream clients
# require plain HTTP. If you need a 301 redirect, override the HTTP server
# block in your own deployment.
EOF
)

  {
    printf '%s\n\n' "${header}"
    sed \
      -e 's|^worker_processes [0-9]\+;|worker_processes auto;|' \
      -e 's|^\(\s*listen \)8081\( reuseport;\)|\1__CUBE_PROXY_HTTP_PORT__\2|' \
      -e 's|^\(\s*listen \)8080\( ssl reuseport;\)|\1__CUBE_PROXY_HTTPS_PORT__\2|' \
      -e 's|^\(\s*set \$host_proxy_port \)8081;|\1__CUBE_PROXY_HTTP_PORT__;|' \
      -e 's|^\(\s*set \$host_proxy_port \)8080;|\1__CUBE_PROXY_HTTPS_PORT__;|' \
      -e 's|/usr/local/openresty/nginx/certs/cube\.app+3\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_CERT__|' \
      -e 's|/usr/local/openresty/nginx/certs/cube\.app+3-key\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_KEY__|' \
      "${src}"
  } > "${dst}"

  for token in __CUBE_PROXY_HTTP_PORT__ __CUBE_PROXY_HTTPS_PORT__ __CUBE_PROXY_SSL_CERT__ __CUBE_PROXY_SSL_KEY__; do
    if ! grep -q -F "${token}" "${dst}"; then
      die "generated nginx.conf.template is missing placeholder ${token}; upstream CubeProxy/nginx.conf may have changed"
    fi
  done
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
ensure_dir "${CUBE_SYSTEMD_TEMPLATE_DIR}"
ensure_dir "${CUBE_PROXY_SOURCE_DIR}"

log "building runtime layout"
"${SCRIPT_DIR}/build-vm-assets.sh"

log "packaging fixed kernel artifact zip"
package_kernel_artifact_zip \
  "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-bm" \
  "${KERNEL_ARTIFACT_ZIP}" \
  "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-pvm"

rm -rf "${CORE_BIN_DIR}" "${PACKAGE_ROOT}" "${PACKAGE_TAR}" "${DIST_ROOT}" "${DIST_TAR}"
mkdir -p "${CORE_BIN_DIR}"

CUBEMASTER_VERSION_PKG="github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
CUBELET_VERSION_PKG="github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
NETAGENT_VERSION_PKG="github.com/tencentcloud/CubeSandbox/network-agent/pkg/version"

build_or_copy_go_binary \
  "cubemaster" "${CUBEMASTER_BIN_OVERRIDE}" \
  "${ROOT_DIR}/CubeMaster" "${CUBEMASTER_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubemaster" ./cmd/cubemaster "${CUBEMASTER_VERSION_PKG}"
build_or_copy_go_binary \
  "cubemastercli" "${CUBEMASTERCLI_BIN_OVERRIDE}" \
  "${ROOT_DIR}/CubeMaster" "${CUBEMASTER_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubemastercli" ./cmd/cubemastercli "${CUBEMASTER_VERSION_PKG}"
build_or_copy_go_binary \
  "cubelet" "${CUBELET_BIN_OVERRIDE}" \
  "${ROOT_DIR}/Cubelet" "${CUBELET_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubelet" ./cmd/cubelet "${CUBELET_VERSION_PKG}"
build_or_copy_go_binary \
  "cubecli" "${CUBECLI_BIN_OVERRIDE}" \
  "${ROOT_DIR}/Cubelet" "${CUBELET_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubecli" ./cmd/cubecli "${CUBELET_VERSION_PKG}"
build_or_copy_rust_binary \
  "cube-api" "${API_BIN_OVERRIDE}" \
  "${ROOT_DIR}/CubeAPI" "${API_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cube-api"
build_or_copy_go_binary \
  "network-agent" "${NETWORK_AGENT_BIN_OVERRIDE}" \
  "${ROOT_DIR}/network-agent" "${NETWORK_AGENT_BUILD_MODE}" \
  "${CORE_BIN_DIR}/network-agent" ./cmd/network-agent "${NETAGENT_VERSION_PKG}"
build_or_copy_go_binary \
  "cubevsmapdump" "${CUBEVSMAPDUMP_BIN_OVERRIDE}" \
  "${ROOT_DIR}/CubeNet/cubevs" "${CUBEVSMAPDUMP_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubevsmapdump" ./cmd/cubevsmapdump
# Auto-pause sidecar ships embedded inside the cube-proxy container image
# (CubeProxy/Dockerfile COPY bin/cube-proxy-sidecar). The cube-proxy image
# is openresty:alpine-fat (musl libc), so the binary MUST be statically
# linked — a default `go build` produces a glibc-linked binary that fails
# at exec with rc=127 / "required file not found" on musl. Force
# CGO_ENABLED=0 + -tags netgo,osusergo to get a pure-Go static binary.
# Skip the generic build_or_copy_go_binary helper (which doesn't expose
# these knobs) and call build directly here.
if [[ -n "${CUBE_PROXY_SIDECAR_BIN_OVERRIDE}" ]]; then
  log "using prebuilt cube-proxy-sidecar: ${CUBE_PROXY_SIDECAR_BIN_OVERRIDE}"
  copy_file "${CUBE_PROXY_SIDECAR_BIN_OVERRIDE}" "${CORE_BIN_DIR}/cube-proxy-sidecar"
else
  log "building cube-proxy-sidecar (static, CGO_ENABLED=0)"
  case "${CUBE_PROXY_SIDECAR_BUILD_MODE}" in
    local)
      require_cmd go
      (cd "${ROOT_DIR}/CubeProxy/sidecar" && \
        go mod download && \
        CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
          go build -trimpath -tags 'netgo osusergo' -ldflags '-s -w' \
            -o "${CORE_BIN_DIR}/cube-proxy-sidecar" ./cmd/sidecar) >&2
      ;;
    *)
      die "unsupported cube-proxy-sidecar build mode: ${CUBE_PROXY_SIDECAR_BUILD_MODE}"
      ;;
  esac
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
  "${PACKAGE_ROOT}/systemd" \
  "${PACKAGE_ROOT}/cube-vs/network" \
  "${PACKAGE_ROOT}/cube-snapshot" \
  "${PACKAGE_ROOT}/scripts/one-click" \
  "${PACKAGE_ROOT}/scripts/systemd" \
  "${PACKAGE_ROOT}/scripts/cube-egress" \
  "${PACKAGE_ROOT}/cube-egress" \
  "${PACKAGE_ROOT}/sql"

copy_file "${CORE_BIN_DIR}/network-agent" "${PACKAGE_ROOT}/network-agent/bin/network-agent"
copy_file "${CORE_BIN_DIR}/cubevsmapdump" "${PACKAGE_ROOT}/network-agent/bin/cubevsmapdump"
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
copy_dir_contents "${CUBE_SYSTEMD_TEMPLATE_DIR}" "${PACKAGE_ROOT}/systemd"
copy_dir_contents "${CUBE_PROXY_SOURCE_DIR}" "${PACKAGE_ROOT}/cubeproxy/build-context"
rm -f "${PACKAGE_ROOT}/cubeproxy/build-context/Makefile"
# The build-context only needs the prebuilt sidecar binary, not the Go
# source: the runtime image uses `COPY bin/cube-proxy-sidecar` (no Go
# toolchain in the runtime stage). Strip the source tree to keep the
# release bundle lean.
rm -rf "${PACKAGE_ROOT}/cubeproxy/build-context/sidecar"
# Drop the prebuilt sidecar binary into the build context so the Dockerfile's
# COPY bin/cube-proxy-sidecar step has something to grab. Path matches the
# in-tree CubeProxy/Makefile prebuild-sidecar layout (CubeProxy/bin/) so the
# Dockerfile is identical for both build flows.
mkdir -p "${PACKAGE_ROOT}/cubeproxy/build-context/bin"
copy_file \
  "${CORE_BIN_DIR}/cube-proxy-sidecar" \
  "${PACKAGE_ROOT}/cubeproxy/build-context/bin/cube-proxy-sidecar"
chmod +x "${PACKAGE_ROOT}/cubeproxy/build-context/bin/cube-proxy-sidecar"
generate_cube_proxy_nginx_template \
  "${CUBE_PROXY_SOURCE_DIR}/nginx.conf" \
  "${PACKAGE_ROOT}/cubeproxy/nginx.conf.template"
build_web_dist "${PACKAGE_ROOT}/webui/dist"
copy_dir_contents "${CUBE_SUPPORT_TEMPLATE_DIR}" "${PACKAGE_ROOT}/support"
copy_file "${MKCERT_BIN_ASSET}" "${PACKAGE_ROOT}/support/bin/mkcert"

copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-shim" "${PACKAGE_ROOT}/cube-shim"
copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf" "${PACKAGE_ROOT}/cube-kernel-scf"
copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-image" "${PACKAGE_ROOT}/cube-image"

# Ship the entire scripts/one-click directory: the systemd unit scripts
# delegate container lifecycle to the compose-based up-/down- helpers
# (e.g. up-cube-proxy.sh, up-support.sh, up-webui.sh and their compose-lib
# siblings), so the runtime needs every script in this directory.
copy_dir_contents "${SCRIPT_DIR}/scripts/one-click" "${PACKAGE_ROOT}/scripts/one-click"
copy_dir_contents "${SCRIPT_DIR}/scripts/systemd" "${PACKAGE_ROOT}/scripts/systemd"
# cube-diag is the documented diagnostic entry point (see docs/guide/service-management.md);
# it must ship in the release bundle so the install layout exposes
# ${INSTALL_PREFIX}/scripts/cube-diag/collect-logs.sh.
copy_dir_contents "${SCRIPT_DIR}/scripts/cube-diag" "${PACKAGE_ROOT}/scripts/cube-diag"
# CubeEgress's host-side iptables/sysctl init script. Lives in the
# CubeEgress repo subtree (CubeEgress/scripts/) — copy a single file
# rather than the whole dir so we don't pull in the legacy
# cube-proxy-net.service unit that conflicts with our deploy/one-click
# integration.
copy_file "${CUBE_EGRESS_SOURCE_DIR}/scripts/cube-proxy-iptables-init.sh" \
          "${PACKAGE_ROOT}/scripts/cube-egress/cube-proxy-iptables-init.sh"
mkdir -p "${PACKAGE_ROOT}/scripts/common"
copy_file "${SCRIPT_DIR}/scripts/common/validation.sh" "${PACKAGE_ROOT}/scripts/common/validation.sh"

# Host-side version marker for cube-egress: cubelet's versioninfo.Collector
# detects the component by the presence of cube-egress/version and reports
# the content as the installed version. Must match cube_version so the
# declared-vs-actual comparison in CubeMaster's version matrix works.
printf '%s\n' "${DIST_VERSION}" > "${PACKAGE_ROOT}/cube-egress/version"

copy_dir_contents "${SCRIPT_DIR}/sql" "${PACKAGE_ROOT}/sql"

find "${PACKAGE_ROOT}" -type f -path "*/bin/*" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/one-click" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/systemd" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/common" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/cube-diag" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/cube-egress" -type f -name "*.sh" -exec chmod +x {} \;

mkdir -p "$(dirname "${PACKAGE_TAR}")"
tar -C "${WORK_ROOT}" -czf "${PACKAGE_TAR}" "sandbox-package"

mkdir -p "${DIST_ROOT}/assets/package" "${DIST_ROOT}/assets/kernel-artifacts" "${DIST_ROOT}/lib" "${DIST_ROOT}/scripts/common"
copy_file "${SCRIPT_DIR}/README.md" "${DIST_ROOT}/README.md"
copy_file "${SCRIPT_DIR}/install.sh" "${DIST_ROOT}/install.sh"
copy_file "${SCRIPT_DIR}/install-compute.sh" "${DIST_ROOT}/install-compute.sh"
copy_file "${SCRIPT_DIR}/down.sh" "${DIST_ROOT}/down.sh"
copy_file "${SCRIPT_DIR}/smoke.sh" "${DIST_ROOT}/smoke.sh"
copy_file "${SCRIPT_DIR}/online-install.sh" "${DIST_ROOT}/online-install.sh"
copy_file "${SCRIPT_DIR}/env.example" "${DIST_ROOT}/env.example"
copy_file "${SCRIPT_DIR}/lib/common.sh" "${DIST_ROOT}/lib/common.sh"
copy_file "${SCRIPT_DIR}/scripts/common/validation.sh" "${DIST_ROOT}/scripts/common/validation.sh"
copy_file "${PACKAGE_TAR}" "${DIST_ROOT}/assets/package/sandbox-package.tar.gz"
copy_file "${KERNEL_ARTIFACT_ZIP}" "${DIST_ROOT}/assets/kernel-artifacts/cube-kernel-scf.zip"
chmod +x \
  "${DIST_ROOT}/install.sh" \
  "${DIST_ROOT}/install-compute.sh" \
  "${DIST_ROOT}/down.sh" \
  "${DIST_ROOT}/smoke.sh" \
  "${DIST_ROOT}/online-install.sh"

cat > "${DIST_ROOT}/VERSION.txt" <<EOF
release_version=${DIST_VERSION}
git_commit=${CUBE_COMMIT}
built_at=${CUBE_BUILD_TIME}
manifest=release-manifest.json
EOF

# Generate machine-readable release manifest (M1-2).
# Depends on: CORE_BIN_DIR, RUNTIME_LAYOUT_DIR, and CUBE_*_PATH vars
# exported by build-vm-assets.sh.
generate_release_manifest "${DIST_ROOT}" "${DIST_VERSION}"

tar -C "${SCRIPT_DIR}/dist" -czf "${DIST_TAR}" "cube-sandbox-one-click-${DIST_VERSION}"
log "release bundle ready: ${DIST_TAR}"
