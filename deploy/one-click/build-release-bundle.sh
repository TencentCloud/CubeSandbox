#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
load_build_env

# tar_czf: create a gzip-compressed tarball, using pigz (parallel gzip) when it
# is installed and falling back to tar's built-in single-threaded gzip
# otherwise. The output is a standard .tar.gz in both cases, so consumers /
# `tar xzf` are unaffected -- pigz only speeds up compression of the large guest
# image + kernel payloads on multi-core hosts. ONE_CLICK_DISABLE_PIGZ=1 forces
# the portable single-tool gzip path for environments that want a consistent,
# single-threaded compressor rather than pigz's parallel block framing. (Note:
# neither path is strictly bit-for-bit reproducible -- gzip still records an
# mtime/OS byte in its header.)
tar_czf() {
  local out="$1"
  local base_dir="$2"
  local member="$3"
  if [[ "${ONE_CLICK_DISABLE_PIGZ:-}" != "1" ]] && command -v pigz >/dev/null 2>&1; then
    # Run the pipeline in a subshell with pipefail set there, so a tar failure
    # in the pipe is caught even if a caller ever clears the global setting --
    # and the option change stays scoped to the subshell instead of leaking back
    # into the caller. This produces the shipped tarball.
    ( set -o pipefail; tar -C "${base_dir}" -cf - "${member}" | pigz > "${out}" )
  else
    tar -C "${base_dir}" -czf "${out}" "${member}"
  fi
}

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
CUBE_LCM_TEMPLATE_DIR="${SCRIPT_DIR}/cube-lifecycle-manager"
CUBE_PROXY_SOURCE_DIR="${ONE_CLICK_CUBE_PROXY_SOURCE_DIR:-${ROOT_DIR}/CubeProxy}"
CUBE_EGRESS_SOURCE_DIR="${ONE_CLICK_CUBE_EGRESS_SOURCE_DIR:-${ROOT_DIR}/CubeEgress}"
WEB_SOURCE_DIR="${ONE_CLICK_WEB_SOURCE_DIR:-${ROOT_DIR}/web}"
WEB_DIST_OVERRIDE="${ONE_CLICK_WEB_DIST_DIR:-}"
MKCERT_BIN_ASSET="${ONE_CLICK_MKCERT_BIN:-${SCRIPT_DIR}/assets/bin/mkcert-v1.4.4-linux-$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')}"
CUBE_KERNEL_VMLINUX="${ONE_CLICK_CUBE_KERNEL_VMLINUX:-${RAW_ARTIFACTS_DIR}/vmlinux}"
KERNEL_ARTIFACT_ZIP="${WORK_ROOT}/cube-kernel-scf.zip"

CUBE_VERSION_FROM_ENV="${CUBE_VERSION:-}"
LATEST_RELEASE_TAG="$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
: "${CUBE_VERSION:=${LATEST_RELEASE_TAG:-0.0.0-dev}}"
: "${CUBE_COMMIT:=$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')}"
# Anchor to the HEAD commit date (UTC) so repeated builds on the same commit
# embed a byte-identical value and reuse incremental caches; keep the manifest
# built_at in step with the binaries' BuildTime when this script is invoked
# directly. The builder wrapper exports the same value and pre-empts this. Falls
# back to wall-clock outside a git tree; still overridable via the environment.
: "${CUBE_BUILD_TIME:=$(TZ=UTC0 git -C "${ROOT_DIR}" show -s --format=%cd --date=format-local:'%Y-%m-%dT%H:%M:%SZ' HEAD 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%SZ')}"
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME

DIST_VERSION="${ONE_CLICK_DIST_VERSION:-${CUBE_VERSION_FROM_ENV:-${LATEST_RELEASE_TAG:-$(latest_git_revision "${ROOT_DIR}")}}}"
DIST_ROOT="${SCRIPT_DIR}/dist/cube-sandbox-one-click-${DIST_VERSION}"
DIST_TAR="${SCRIPT_DIR}/dist/cube-sandbox-one-click-${DIST_VERSION}.tar.gz"

CUBEMASTER_BUILD_MODE="${ONE_CLICK_CUBEMASTER_BUILD_MODE:-local}"
CUBELET_BUILD_MODE="${ONE_CLICK_CUBELET_BUILD_MODE:-local}"
API_BUILD_MODE="${ONE_CLICK_CUBE_API_BUILD_MODE:-local}"
CUBE_OPS_BUILD_MODE="${ONE_CLICK_CUBE_OPS_BUILD_MODE:-local}"
CUBEVSMAPDUMP_BUILD_MODE="${ONE_CLICK_CUBEVSMAPDUMP_BUILD_MODE:-local}"

CUBEMASTER_BIN_OVERRIDE="${ONE_CLICK_CUBEMASTER_BIN:-}"
CUBEMASTERCLI_BIN_OVERRIDE="${ONE_CLICK_CUBEMASTERCLI_BIN:-}"
CUBELET_BIN_OVERRIDE="${ONE_CLICK_CUBELET_BIN:-}"
CUBECLI_BIN_OVERRIDE="${ONE_CLICK_CUBECLI_BIN:-}"
API_BIN_OVERRIDE="${ONE_CLICK_CUBE_API_BIN:-}"
CUBE_OPS_BIN_OVERRIDE="${ONE_CLICK_CUBE_OPS_BIN:-}"
CUBE_OPS_CLI_BIN_OVERRIDE="${ONE_CLICK_CUBE_OPS_CLI_BIN:-}"
CUBEVSMAPDUMP_BIN_OVERRIDE="${ONE_CLICK_CUBEVSMAPDUMP_BIN:-}"

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

  # Prefer independent cube-agent/version; fall back to legacy cube-image/agent-version.
  local guest_agent_version="${cube_version}"
  local guest_agent_ver_file="${RUNTIME_LAYOUT_DIR}/cube-agent/version"
  if [[ -f "${guest_agent_ver_file}" ]]; then
    guest_agent_version="$(head -n1 "${guest_agent_ver_file}" | tr -d '[:space:]')"
  elif [[ -f "${RUNTIME_LAYOUT_DIR}/cube-image/agent-version" ]]; then
    guest_agent_version="$(head -n1 "${RUNTIME_LAYOUT_DIR}/cube-image/agent-version" | tr -d '[:space:]')"
  fi

  # Guest-image path
  local guest_image_path="${RUNTIME_LAYOUT_DIR}/cube-image/cube-guest-image-cpu.img"
  local agent_ext4_path="${RUNTIME_LAYOUT_DIR}/cube-agent/cube-agent.ext4"

  # Kernel paths
  local kernel_vmlinux="${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-bm"
  local kernel_pvm_vmlinux="${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-pvm"

  # Prefer version.json variant digests for kernel inventory keys.
  local kernel_version=""
  local kernel_pvm_version=""
  local kernel_json="${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/version.json"
  if [[ -f "${kernel_json}" ]]; then
    kernel_version="$(python3 - "${kernel_json}" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print(((d.get("variants") or {}).get("bm") or {}).get("version") or "", end="")
PY
)"
    kernel_pvm_version="$(python3 - "${kernel_json}" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print(((d.get("variants") or {}).get("pvm") or {}).get("version") or "", end="")
PY
)"
  fi
  if [[ -z "${kernel_version}" || "${kernel_version}" == "unknown" ]]; then
    kernel_version="$(file_content_version "${kernel_vmlinux}")" || kernel_version="unknown"
  fi
  if [[ "${kernel_version}" == "unknown" ]]; then
    log "WARNING: cannot resolve kernel.version (no vmlinux-bm); manifest will record unknown"
  else
    log "release manifest kernel.version=${kernel_version} (content short hash)"
  fi

  if [[ -z "${kernel_pvm_version}" || "${kernel_pvm_version}" == "unknown" ]]; then
    if [[ -f "${kernel_pvm_vmlinux}" ]]; then
      kernel_pvm_version="$(file_content_version "${kernel_pvm_vmlinux}")" \
        || kernel_pvm_version="unknown"
      log "release manifest kernel.pvm_version=${kernel_pvm_version} (content short hash)"
    else
      kernel_pvm_version="unknown"
    fi
  else
    log "release manifest kernel.pvm_version=${kernel_pvm_version} (content short hash)"
  fi

  # Agent digest: prefer cube-agent.ext4; fall back to ELF binary for older layouts.
  local agent_artifact="${agent_ext4_path}"
  if [[ ! -f "${agent_artifact}" ]]; then
    agent_artifact="${ONE_CLICK_CUBE_AGENT_BIN:-}"
    if [[ -z "${agent_artifact}" ]]; then
      for candidate in \
        "${ROOT_DIR}/agent/target/x86_64-unknown-linux-musl/release/cube-agent" \
        "${ROOT_DIR}/agent/target/release/cube-agent"; do
        if [[ -f "${candidate}" ]]; then
          agent_artifact="${candidate}"
          break
        fi
      done
    fi
  fi

  # Shim + runtime binaries: already copied to RUNTIME_LAYOUT_DIR by build-vm-assets.sh.
  local shim_bin="${RUNTIME_LAYOUT_DIR}/cube-shim/bin/containerd-shim-cube-rs"
  local runtime_bin="${RUNTIME_LAYOUT_DIR}/cube-shim/bin/cube-runtime"

  python3 - "${output}" "${release_version}" "${cube_version}" "${cube_commit}" "${cube_build_time}" \
      "${guest_image_version}" "${guest_agent_version}" "${kernel_version}" "${kernel_pvm_version}" \
      "${CORE_BIN_DIR}" \
      "${agent_artifact}" "${shim_bin}" "${runtime_bin}" \
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
for name in ["cubemaster", "cubemastercli", "cubelet", "cubecli"]:
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

# ── cubeops from CORE_BIN_DIR ──
components["cubeops"] = {
    "version": cube_version,
    "commit": cube_commit,
    "build_time": cube_build_time,
    "digest_sha256": required_sha256(os.path.join(core_bin_dir, "cubeops")),
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
components["cube-lifecycle-manager"] = {
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
#
# The gRPC server block (port __CUBE_PROXY_GRPC_PORT__) is a plaintext HTTP/2
# listener for sandbox gRPC ingress.
EOF
)

  {
    printf '%s\n\n' "${header}"
    sed \
      -e 's|^worker_processes [0-9]\+;|worker_processes auto;|' \
      -e 's|^\(\s*listen \)8081\( reuseport;\)|\1__CUBE_PROXY_HTTP_PORT__\2|' \
      -e 's|^\(\s*listen \)8080\( ssl reuseport;\)|\1__CUBE_PROXY_HTTPS_PORT__\2|' \
      -e 's|^\(\s*listen \)9090\( http2 reuseport;\)|\1__CUBE_PROXY_GRPC_PORT__\2|' \
      -e 's|^\(\s*set \$host_proxy_port \)8081;|\1__CUBE_PROXY_HTTP_PORT__;|' \
      -e 's|^\(\s*set \$host_proxy_port \)8080;|\1__CUBE_PROXY_HTTPS_PORT__;|' \
      -e 's|^\(\s*listen \)127\.0\.0\.1:8082;|\1__CUBE_PROXY_ADMIN_LISTEN__:__CUBE_PROXY_ADMIN_PORT__;|' \
      -e 's|/usr/local/openresty/nginx/certs/cube\.app+3\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_CERT__|' \
      -e 's|/usr/local/openresty/nginx/certs/cube\.app+3-key\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_KEY__|' \
      "${src}"
  } > "${dst}"

  for token in __CUBE_PROXY_HTTP_PORT__ __CUBE_PROXY_HTTPS_PORT__ __CUBE_PROXY_GRPC_PORT__ __CUBE_PROXY_ADMIN_LISTEN__ __CUBE_PROXY_ADMIN_PORT__ __CUBE_PROXY_SSL_CERT__ __CUBE_PROXY_SSL_KEY__; do
    if ! grep -q -F "${token}" "${dst}"; then
      die "generated nginx.conf.template is missing placeholder ${token}; upstream CubeProxy/nginx.conf may have changed"
    fi
  done
}

# WebUI (npm) build state. The npm build only reads WEB_SOURCE_DIR and writes
# WEB_SOURCE_DIR/dist; it shares no inputs or outputs with the guest-image /
# runtime-layout build, so it can run concurrently with build-vm-assets.sh and
# then be collected later. WEB_BUILD_ASYNC_PID is the backgrounded builder (0 =
# none started; the synchronous path in build_web_dist runs instead).
WEB_BUILD_ASYNC_PID=0
# Set to 1 once a successful async build has been collected and its dist is on
# disk, so build_web_dist copies the result instead of re-`wait`ing a
# already-reaped pid (which would return 127 and be misread as a failure) or
# rebuilding synchronously.
WEB_BUILD_ASYNC_DONE=0
WEB_BUILD_ASYNC_LOG="${WORK_ROOT}/web-build.log"

# Replay the captured async web-build log to stderr. Called on both success and
# failure so that tsc/vite diagnostics that exit 0 (warnings) stay visible --
# mirroring the builder-track policy where every track's log is always replayed
# on completion, not only on failure.
_replay_web_build_log() {
  echo "[one-click] ----- begin web build log -----" >&2
  cat "${WEB_BUILD_ASYNC_LOG}" >&2 || true
  echo "[one-click] ----- end web build log -----" >&2
}

# Wait for the in-flight async build, replay its log, and die on failure. Clears
# WEB_BUILD_ASYNC_PID (so the EXIT trap and any later call become no-ops) and
# marks the dist ready on success. Caller must have confirmed WEB_BUILD_ASYNC_PID
# is nonzero.
_collect_web_build_async() {
  local web_pid="${WEB_BUILD_ASYNC_PID}"
  WEB_BUILD_ASYNC_PID=0
  if ! wait "${web_pid}"; then
    _replay_web_build_log
    die "web dashboard build failed"
  fi
  _replay_web_build_log
  WEB_BUILD_ASYNC_DONE=1
}

# Non-blocking check: if the async web build has ALREADY exited, collect it now.
# On failure this aborts immediately -- so a fast failure (bad lockfile, TS
# error) surfaces before the remaining serial work (component binary builds +
# packaging) runs, instead of being masked until build_web_dist near the very
# end. If the build is still running, return at once and let it keep overlapping.
# No-op when no async build is in flight or it was already collected.
poll_web_build_async() {
  [[ "${WEB_BUILD_ASYNC_PID}" -ne 0 ]] || return 0
  kill -0 "${WEB_BUILD_ASYNC_PID}" 2>/dev/null && return 0
  log "async web dashboard build finished early; collecting"
  _collect_web_build_async
}

# Reap a still-running async web build on exit/signal so npm is not orphaned.
# WEB_BUILD_ASYNC_PID is a wrapper; group-kill its children, then the wrapper.
cleanup_web_build_async() {
  local child
  [[ "${WEB_BUILD_ASYNC_PID}" -ne 0 ]] || return 0
  if kill -0 "${WEB_BUILD_ASYNC_PID}" 2>/dev/null; then
    while read -r child; do
      [[ -n "${child}" ]] || continue
      kill -- -"${child}" 2>/dev/null || kill "${child}" 2>/dev/null || true
    done < <(pgrep -P "${WEB_BUILD_ASYNC_PID}" 2>/dev/null || true)
    kill "${WEB_BUILD_ASYNC_PID}" 2>/dev/null || true
    wait "${WEB_BUILD_ASYNC_PID}" 2>/dev/null || true
  fi
}
trap cleanup_web_build_async EXIT
trap 'cleanup_web_build_async; trap - INT; kill -INT $$' INT
trap 'cleanup_web_build_async; trap - TERM; kill -TERM $$' TERM

# Overlap npm with guest-image build. No-op unless npm + source are available
# and ONE_CLICK_SEQUENTIAL_WEB_BUILD is unset. Keep the job off the caller's
# tty (setsid -w, stdin=/dev/null) so it cannot race sudo password echo-off.
start_web_build_async() {
  [[ "${ONE_CLICK_SEQUENTIAL_WEB_BUILD:-}" != "1" ]] || return 0
  [[ -z "${WEB_DIST_OVERRIDE}" ]] || return 0
  command -v npm >/dev/null 2>&1 || return 0
  [[ -d "${WEB_SOURCE_DIR}" ]] || return 0

  mkdir -p "$(dirname "${WEB_BUILD_ASYNC_LOG}")"
  log "building web dashboard (async, overlapping runtime layout build)"
  # Wrapper keeps wait/$! correct; avoid bare `setsid cmd &` (can exit early).
  (
    if command -v setsid >/dev/null 2>&1; then
      setsid -w bash -c 'cd "$1" && npm ci && npm run build' bash "${WEB_SOURCE_DIR}" \
        </dev/null >"${WEB_BUILD_ASYNC_LOG}" 2>&1
      exit $?
    fi
    set -m
    ( cd "${WEB_SOURCE_DIR}" && npm ci && npm run build ) \
      </dev/null >"${WEB_BUILD_ASYNC_LOG}" 2>&1 &
    local child=$!
    set +m
    wait "${child}"
    exit $?
  ) &
  WEB_BUILD_ASYNC_PID=$!
}

ensure_sudo_authenticated_for_packaging() {
  [[ "${EUID}" -eq 0 ]] && return 0
  if sudo -n true >/dev/null 2>&1; then
    return 0
  fi
  require_cmd sudo
  log "sudo authentication required for guest rootfs / runtime layout packaging"
  sudo -v || die "sudo authentication failed (needed to package guest rootfs / runtime layout)"
}

build_web_dist() {
  local output_dir="$1"
  rm -rf "${output_dir}"
  mkdir -p "${output_dir}"

  if [[ -n "${WEB_DIST_OVERRIDE}" ]]; then
    log "using prebuilt web dist: ${WEB_DIST_OVERRIDE}"
    ensure_dir "${WEB_DIST_OVERRIDE}"
    copy_dir_contents "${WEB_DIST_OVERRIDE}" "${output_dir}"
  elif [[ "${WEB_BUILD_ASYNC_DONE}" -eq 1 ]]; then
    # Already collected by an earlier poll_web_build_async (finished-early path).
    log "using async web dashboard build collected earlier"
    copy_dir_contents "${WEB_SOURCE_DIR}/dist" "${output_dir}"
  elif [[ "${WEB_BUILD_ASYNC_PID}" -ne 0 ]]; then
    log "collecting async web dashboard build"
    _collect_web_build_async
    copy_dir_contents "${WEB_SOURCE_DIR}/dist" "${output_dir}"
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

# sudo first (quiet tty), then async web build overlapping guest-image below.
ensure_sudo_authenticated_for_packaging
start_web_build_async

log "building runtime layout"
"${SCRIPT_DIR}/build-vm-assets.sh"

# The guest-image build above is the long pole the async web build overlaps. By
# the time it returns, a fast web-build failure (bad lockfile, TS error) has
# usually already happened -- surface it now rather than masking it behind the
# remaining component-binary builds and packaging that still precede
# build_web_dist. Non-blocking: if the web build is still running it keeps
# overlapping and is collected later.
poll_web_build_async

log "packaging fixed kernel artifact zip"
package_kernel_artifact_zip \
  "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-bm" \
  "${KERNEL_ARTIFACT_ZIP}" \
  "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-pvm"

rm -rf "${CORE_BIN_DIR}" "${PACKAGE_ROOT}" "${PACKAGE_TAR}" "${DIST_ROOT}" "${DIST_TAR}"
mkdir -p "${CORE_BIN_DIR}"

CUBEMASTER_VERSION_PKG="github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
CUBELET_VERSION_PKG="github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
CUBEOPS_VERSION_PKG="github.com/tencentcloud/CubeSandbox/CubeOps/internal/version"

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
  "cubeops" "${CUBE_OPS_BIN_OVERRIDE}" \
  "${ROOT_DIR}/CubeOps" "${CUBE_OPS_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubeops" ./cmd/cubeops "${CUBEOPS_VERSION_PKG}"
build_or_copy_go_binary \
  "cubeopscli" "${CUBE_OPS_CLI_BIN_OVERRIDE}" \
  "${ROOT_DIR}/CubeOps" "${CUBE_OPS_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubeopscli" ./cmd/cubeopscli
build_or_copy_go_binary \
  "cubevsmapdump" "${CUBEVSMAPDUMP_BIN_OVERRIDE}" \
  "${ROOT_DIR}/CubeNet/cubevs" "${CUBEVSMAPDUMP_BUILD_MODE}" \
  "${CORE_BIN_DIR}/cubevsmapdump" ./cmd/cubevsmapdump

mkdir -p \
  "${PACKAGE_ROOT}/CubeAPI/bin" \
  "${PACKAGE_ROOT}/CubeOps/bin" \
  "${PACKAGE_ROOT}/CubeMaster/bin" \
  "${PACKAGE_ROOT}/CubeMaster/plugin" \
  "${PACKAGE_ROOT}/Cubelet/bin" \
  "${PACKAGE_ROOT}/Cubelet/plugin" \
  "${PACKAGE_ROOT}/Cubelet/config" \
  "${PACKAGE_ROOT}/Cubelet/dynamicconf" \
  "${PACKAGE_ROOT}/cubeproxy" \
  "${PACKAGE_ROOT}/cube-lifecycle-manager" \
  "${PACKAGE_ROOT}/coredns" \
  "${PACKAGE_ROOT}/webui" \
  "${PACKAGE_ROOT}/webui/dist" \
  "${PACKAGE_ROOT}/support" \
  "${PACKAGE_ROOT}/support/bin" \
  "${PACKAGE_ROOT}/systemd" \
  "${PACKAGE_ROOT}/cube-vs/network/bin" \
  "${PACKAGE_ROOT}/cube-snapshot" \
  "${PACKAGE_ROOT}/scripts/one-click" \
  "${PACKAGE_ROOT}/scripts/systemd" \
  "${PACKAGE_ROOT}/scripts/cube-egress" \
  "${PACKAGE_ROOT}/cube-egress" \
  "${PACKAGE_ROOT}/terraform/tencentcloud"

copy_file "${CORE_BIN_DIR}/cubevsmapdump" "${PACKAGE_ROOT}/cube-vs/network/bin/cubevsmapdump"

# Lay down the one-click CubeAPI assets (Dockerfile, etc.) first, then copy the
# binary on top: copy_dir_contents rm -rf's the destination, so copying the
# binary afterwards keeps both coexisting in the package.
copy_dir_contents "${SCRIPT_DIR}/CubeAPI" "${PACKAGE_ROOT}/CubeAPI"
copy_file "${CORE_BIN_DIR}/cube-api" "${PACKAGE_ROOT}/CubeAPI/bin/cube-api"

# Lay down the one-click CubeOps package Dockerfile first, then copy the binary
# on top so terraform/tencentcloud/build_images.sh can build cube-ops from the
# extracted sandbox-package without the full source tree.
copy_dir_contents "${SCRIPT_DIR}/CubeOps" "${PACKAGE_ROOT}/CubeOps"
copy_file "${CORE_BIN_DIR}/cubeops" "${PACKAGE_ROOT}/CubeOps/bin/cubeops"
copy_file "${CORE_BIN_DIR}/cubeopscli" "${PACKAGE_ROOT}/CubeOps/bin/cubeopscli"

# Same ordering for CubeMaster so cubemaster/cubemastercli binaries survive the
# copy_dir_contents wipe and coexist with the one-click CubeMaster assets.
copy_dir_contents "${SCRIPT_DIR}/CubeMaster" "${PACKAGE_ROOT}/CubeMaster"
copy_file "${CORE_BIN_DIR}/cubemaster" "${PACKAGE_ROOT}/CubeMaster/bin/cubemaster"
copy_file "${CORE_BIN_DIR}/cubemastercli" "${PACKAGE_ROOT}/CubeMaster/bin/cubemastercli"
copy_file "${ROOT_DIR}/configs/single-node/cubemaster.yaml" "${PACKAGE_ROOT}/CubeMaster/conf.yaml"
# CubeMaster/Dockerfile COPY's this from the package CubeMaster/ context.
copy_file "${ROOT_DIR}/deploy/scripts/docker-install-volume-deps.sh" \
  "${PACKAGE_ROOT}/CubeMaster/docker-install-volume-deps.sh"
chmod +x "${PACKAGE_ROOT}/CubeMaster/docker-install-volume-deps.sh"

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

VOLUME_COS_PLUGIN_SRC="${ROOT_DIR}/examples/volume/cos/binary/cube-volume-cos.sh"
VOLUME_COS_CONF_EXAMPLE="${ROOT_DIR}/examples/volume/cos/volume-cos.conf.example"
VOLUME_COS_INSTALL_DEPS="${ROOT_DIR}/examples/volume/cos/install-deps.sh"
ensure_file "${VOLUME_COS_PLUGIN_SRC}"
ensure_file "${VOLUME_COS_CONF_EXAMPLE}"
ensure_file "${VOLUME_COS_INSTALL_DEPS}"
copy_file "${VOLUME_COS_PLUGIN_SRC}" "${PACKAGE_ROOT}/CubeMaster/plugin/cube-volume-cos"
copy_file "${VOLUME_COS_PLUGIN_SRC}" "${PACKAGE_ROOT}/Cubelet/plugin/cube-volume-cos"
chmod +x "${PACKAGE_ROOT}/CubeMaster/plugin/cube-volume-cos" "${PACKAGE_ROOT}/Cubelet/plugin/cube-volume-cos"
copy_file "${VOLUME_COS_CONF_EXAMPLE}" "${PACKAGE_ROOT}/CubeMaster/plugin/volume-cos.conf.example"
copy_file "${VOLUME_COS_CONF_EXAMPLE}" "${PACKAGE_ROOT}/Cubelet/plugin/volume-cos.conf.example"
# Host-side COS deps installer (cosfs/coscmd/jq); docs run it from plugin/.
copy_file "${VOLUME_COS_INSTALL_DEPS}" "${PACKAGE_ROOT}/CubeMaster/plugin/install-deps.sh"
copy_file "${VOLUME_COS_INSTALL_DEPS}" "${PACKAGE_ROOT}/Cubelet/plugin/install-deps.sh"
chmod +x "${PACKAGE_ROOT}/CubeMaster/plugin/install-deps.sh" "${PACKAGE_ROOT}/Cubelet/plugin/install-deps.sh"

VOLUME_S3_PLUGIN_SRC="${ROOT_DIR}/examples/volume/s3/binary/cube-volume-s3.sh"
VOLUME_S3_CONF_EXAMPLE="${ROOT_DIR}/examples/volume/s3/volume-s3.conf.example"
VOLUME_S3_INSTALL_DEPS="${ROOT_DIR}/examples/volume/s3/install-deps.sh"
ensure_file "${VOLUME_S3_PLUGIN_SRC}"
ensure_file "${VOLUME_S3_CONF_EXAMPLE}"
ensure_file "${VOLUME_S3_INSTALL_DEPS}"
copy_file "${VOLUME_S3_PLUGIN_SRC}" "${PACKAGE_ROOT}/CubeMaster/plugin/cube-volume-s3"
copy_file "${VOLUME_S3_PLUGIN_SRC}" "${PACKAGE_ROOT}/Cubelet/plugin/cube-volume-s3"
chmod +x "${PACKAGE_ROOT}/CubeMaster/plugin/cube-volume-s3" "${PACKAGE_ROOT}/Cubelet/plugin/cube-volume-s3"
copy_file "${VOLUME_S3_CONF_EXAMPLE}" "${PACKAGE_ROOT}/CubeMaster/plugin/volume-s3.conf.example"
copy_file "${VOLUME_S3_CONF_EXAMPLE}" "${PACKAGE_ROOT}/Cubelet/plugin/volume-s3.conf.example"
copy_file "${VOLUME_S3_INSTALL_DEPS}" "${PACKAGE_ROOT}/CubeMaster/plugin/install-s3-deps.sh"
copy_file "${VOLUME_S3_INSTALL_DEPS}" "${PACKAGE_ROOT}/Cubelet/plugin/install-s3-deps.sh"
chmod +x "${PACKAGE_ROOT}/CubeMaster/plugin/install-s3-deps.sh" "${PACKAGE_ROOT}/Cubelet/plugin/install-s3-deps.sh"

copy_dir_contents "${CUBE_PROXY_TEMPLATE_DIR}" "${PACKAGE_ROOT}/cubeproxy"
copy_dir_contents "${CUBE_COREDNS_TEMPLATE_DIR}" "${PACKAGE_ROOT}/coredns"
copy_dir_contents "${CUBE_WEBUI_TEMPLATE_DIR}" "${PACKAGE_ROOT}/webui"
copy_dir_contents "${CUBE_SYSTEMD_TEMPLATE_DIR}" "${PACKAGE_ROOT}/systemd"
# cube-proxy runtime pulls a pre-published image (CubeProxy/Makefile `make
# push` → cube-sandbox-{int,cn}.tencentcloudcr.com). build-context is still
# shipped so terraform/tencentcloud/build_images.sh can rebuild into a private
# TCR when TENCENTCLOUD_USE_TCR=true.
copy_dir_contents "${CUBE_PROXY_SOURCE_DIR}" "${PACKAGE_ROOT}/cubeproxy/build-context"
rm -f "${PACKAGE_ROOT}/cubeproxy/build-context/Makefile"
generate_cube_proxy_nginx_template \
  "${CUBE_PROXY_SOURCE_DIR}/nginx.conf" \
  "${PACKAGE_ROOT}/cubeproxy/nginx.conf.template"

# cube-lifecycle-manager: docker-compose template only. Like cube-egress,
# the runtime image is pre-published to cube-sandbox-{int,cn}.tencentcloudcr.com
# by `make push` from the cube-lifecycle-manager/ source tree; up-cube-
# lifecycle-manager.sh `docker pull`s it at deploy time. No source ships
# inside the bundle.
copy_dir_contents "${CUBE_LCM_TEMPLATE_DIR}" "${PACKAGE_ROOT}/cube-lifecycle-manager"
build_web_dist "${PACKAGE_ROOT}/webui/dist"
copy_dir_contents "${CUBE_SUPPORT_TEMPLATE_DIR}" "${PACKAGE_ROOT}/support"
copy_file "${MKCERT_BIN_ASSET}" "${PACKAGE_ROOT}/support/bin/mkcert"

copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-shim" "${PACKAGE_ROOT}/cube-shim"
copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf" "${PACKAGE_ROOT}/cube-kernel-scf"
copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-image" "${PACKAGE_ROOT}/cube-image"
if [[ -d "${RUNTIME_LAYOUT_DIR}/cube-agent" ]]; then
  copy_dir_contents "${RUNTIME_LAYOUT_DIR}/cube-agent" "${PACKAGE_ROOT}/cube-agent"
fi

# Ship the entire scripts/one-click directory: the systemd unit scripts
# delegate container lifecycle to the compose-based up-/down- helpers
# (e.g. up-cube-proxy.sh, up-support.sh, up-webui.sh and their compose-lib
# siblings), so the runtime needs every script in this directory.
copy_dir_contents "${SCRIPT_DIR}/scripts/one-click" "${PACKAGE_ROOT}/scripts/one-click"
copy_dir_contents "${SCRIPT_DIR}/scripts/systemd" "${PACKAGE_ROOT}/scripts/systemd"
# scripts/{one-click,systemd}/common.sh both `source ../common/validation.sh`,
# so the shared scripts/common helpers must ship alongside them in the package.
copy_dir_contents "${SCRIPT_DIR}/scripts/common" "${PACKAGE_ROOT}/scripts/common"
# cube-diag is the documented diagnostic entry point (see docs/guide/service-management.md);
# it must ship in the release bundle so the install layout exposes
# ${INSTALL_PREFIX}/scripts/cube-diag/collect-logs.sh.
copy_dir_contents "${SCRIPT_DIR}/scripts/cube-diag" "${PACKAGE_ROOT}/scripts/cube-diag"
# CubeEgress's host-side iptables/route init script. Lives in the
# CubeEgress repo subtree (CubeEgress/scripts/) — copy a single file
# rather than the whole dir so we don't pull in the legacy
# cube-proxy-net.service unit that conflicts with our deploy/one-click
# integration.
copy_file "${CUBE_EGRESS_SOURCE_DIR}/scripts/cube-proxy-iptables-init.sh" \
          "${PACKAGE_ROOT}/scripts/cube-egress/cube-proxy-iptables-init.sh"

# Host-side version marker for cube-egress: cubelet's versioninfo.Collector
# detects the component by the presence of cube-egress/version and reports
# the content as the installed version. Must match cube_version so the
# declared-vs-actual comparison in CubeMaster's version matrix works.
printf '%s\n' "${DIST_VERSION}" > "${PACKAGE_ROOT}/cube-egress/version"

copy_dir_contents "${SCRIPT_DIR}/terraform/tencentcloud" "${PACKAGE_ROOT}/terraform/tencentcloud"
# Strip any developer-local terraform state / kubeconfig / SSH keys / TLS
# material / saved credentials so they never ship inside sandbox-package either.
rm -rf \
  "${PACKAGE_ROOT}/terraform/tencentcloud/.terraform" \
  "${PACKAGE_ROOT}/terraform/tencentcloud/.bin" \
  "${PACKAGE_ROOT}/terraform/tencentcloud/.kube" \
  "${PACKAGE_ROOT}/terraform/tencentcloud/.ssh" \
  "${PACKAGE_ROOT}/terraform/tencentcloud/cubeproxy-certs"
# .env holds the operator's saved selections INCLUDING passwords (mode 600); it
# must never end up in a published tarball. Match every secret-bearing pattern
# the deployer's .gitignore anticipates (.env / .env.* / *.pem / *.key /
# credentials* / *.tfvars[.json] / state).
find "${PACKAGE_ROOT}/terraform/tencentcloud" -maxdepth 1 -type f \
  \( -name "*.tfstate" -o -name "*.tfstate.*" -o -name ".terraform.lock.hcl" \
     -o -name ".env" -o -name ".env.*" -o -name "*.pem" -o -name "*.key" \
     -o -name "credentials*" -o -name "*.tfvars" -o -name "*.tfvars.json" \) -delete 2>/dev/null || true
# tke-addons.tf renders cube-webui's nginx config from webui-nginx.conf; it is
# not maintained separately but derived from the canonical webui/nginx.conf so
# the two never drift.
copy_file "${CUBE_WEBUI_TEMPLATE_DIR}/nginx.conf" "${PACKAGE_ROOT}/terraform/tencentcloud/webui-nginx.conf"
# tke-addons.tf also mounts cube-proxy's nginx.conf; render the Terraform copy
# with placeholders so the Kubernetes deployment can bind the admin listener on
# the Pod IP while preserving CubeProxy/nginx.conf as the canonical source.
generate_cube_proxy_nginx_template \
  "${CUBE_PROXY_SOURCE_DIR}/nginx.conf" \
  "${PACKAGE_ROOT}/terraform/tencentcloud/cubeproxy-nginx.conf"
# Verify the terraform deployer actually shipped (mirrors the guarding done for
# the other components), so a renamed/emptied source dir fails the build loudly
# instead of producing a bundle with an absent/broken deployer.
for _tf in create.sh destroy.sh build_images.sh lib-state-sync.sh lib-phases.sh \
  main.tf variables.tf tke-addons.tf query_outputs.tf webui-nginx.conf cubeproxy-nginx.conf; do
  ensure_file "${PACKAGE_ROOT}/terraform/tencentcloud/${_tf}"
done

find "${PACKAGE_ROOT}" -type f -path "*/bin/*" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/one-click" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/systemd" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/common" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/cube-diag" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/scripts/cube-egress" -type f -name "*.sh" -exec chmod +x {} \;
find "${PACKAGE_ROOT}/terraform" -type f -name "*.sh" -exec chmod +x {} \;

mkdir -p "$(dirname "${PACKAGE_TAR}")"
tar_czf "${PACKAGE_TAR}" "${WORK_ROOT}" "sandbox-package"

mkdir -p "${DIST_ROOT}/assets/package" "${DIST_ROOT}/assets/kernel-artifacts" "${DIST_ROOT}/lib" "${DIST_ROOT}/scripts/common"
copy_file "${SCRIPT_DIR}/README.md" "${DIST_ROOT}/README.md"
copy_file "${SCRIPT_DIR}/install.sh" "${DIST_ROOT}/install.sh"
copy_file "${SCRIPT_DIR}/install-compute.sh" "${DIST_ROOT}/install-compute.sh"
copy_file "${SCRIPT_DIR}/down.sh" "${DIST_ROOT}/down.sh"
copy_file "${SCRIPT_DIR}/smoke.sh" "${DIST_ROOT}/smoke.sh"
copy_file "${SCRIPT_DIR}/online-install.sh" "${DIST_ROOT}/online-install.sh"
copy_file "${SCRIPT_DIR}/env.example" "${DIST_ROOT}/env.example"
copy_file "${SCRIPT_DIR}/lib/common.sh" "${DIST_ROOT}/lib/common.sh"
# lib/common.sh `source`s ${ONE_CLICK_DIR}/scripts/common/validation.sh, so the
# shared helpers must ship at the bundle top level too (install.sh /
# install-compute.sh source lib/common.sh from here).
copy_dir_contents "${SCRIPT_DIR}/scripts/common" "${DIST_ROOT}/scripts/common"
copy_file "${PACKAGE_TAR}" "${DIST_ROOT}/assets/package/sandbox-package.tar.gz"
copy_file "${KERNEL_ARTIFACT_ZIP}" "${DIST_ROOT}/assets/kernel-artifacts/cube-kernel-scf.zip"

# Ship the Tencent Cloud terraform cluster deployer at the bundle top level so
# that, right after `tar xzf cube-sandbox-one-click-<version>.tar.gz`, the user
# can run:
#     cd cube-sandbox-one-click-<version>
#     ./terraform/tencentcloud/create.sh
# to spin up a clustered CubeSandbox (TKE control plane + CVM compute nodes).
# The same files are also embedded inside sandbox-package (consumed by the
# jumpserver-side build_images.sh), but those stay buried in the inner package
# until it is extracted; surfacing them here makes the deployer reachable.
copy_dir_contents "${SCRIPT_DIR}/terraform/tencentcloud" "${DIST_ROOT}/terraform/tencentcloud"
# Never leak a developer's local terraform state, kubeconfig, SSH keys,
# TLS material or saved credentials (.env) into the release; create.sh
# regenerates them.
rm -rf \
  "${DIST_ROOT}/terraform/tencentcloud/.terraform" \
  "${DIST_ROOT}/terraform/tencentcloud/.bin" \
  "${DIST_ROOT}/terraform/tencentcloud/.kube" \
  "${DIST_ROOT}/terraform/tencentcloud/.ssh" \
  "${DIST_ROOT}/terraform/tencentcloud/cubeproxy-certs"
find "${DIST_ROOT}/terraform/tencentcloud" -maxdepth 1 -type f \
  \( -name "*.tfstate" -o -name "*.tfstate.*" -o -name ".terraform.lock.hcl" \
     -o -name ".env" -o -name ".env.*" -o -name "*.pem" -o -name "*.key" \
     -o -name "credentials*" -o -name "*.tfvars" -o -name "*.tfvars.json" \) -delete 2>/dev/null || true
# Derive cube-webui's nginx config from the canonical webui/nginx.conf (see the
# sandbox-package copy above) so create.sh can apply tke-addons.tf straight from
# the extracted bundle without the source tree present.
copy_file "${CUBE_WEBUI_TEMPLATE_DIR}/nginx.conf" "${DIST_ROOT}/terraform/tencentcloud/webui-nginx.conf"
# Ship the placeholder-rendered cube-proxy nginx config for Terraform too.
generate_cube_proxy_nginx_template \
  "${CUBE_PROXY_SOURCE_DIR}/nginx.conf" \
  "${DIST_ROOT}/terraform/tencentcloud/cubeproxy-nginx.conf"
# Verify the top-level terraform deployer copy shipped intact too.
for _tf in create.sh destroy.sh build_images.sh lib-state-sync.sh lib-phases.sh \
  main.tf variables.tf tke-addons.tf query_outputs.tf webui-nginx.conf cubeproxy-nginx.conf; do
  ensure_file "${DIST_ROOT}/terraform/tencentcloud/${_tf}"
done
find "${DIST_ROOT}/terraform" -type f -name "*.sh" -exec chmod +x {} \;

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

tar_czf "${DIST_TAR}" "${SCRIPT_DIR}/dist" "cube-sandbox-one-click-${DIST_VERSION}"
log "release bundle ready: ${DIST_TAR}"
