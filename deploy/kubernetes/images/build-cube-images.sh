#!/usr/bin/env bash
# Build and optionally push CubeSandbox images for the Kubernetes/TKE chart.
#
# This script builds role-specific images directly from the CubeSandbox release
# package (sandbox-package). cube-node intentionally uses
# deploy/kubernetes/images/cube-node/Dockerfile because it is a Kubernetes delivery image
# that bundles node-side runtime components.
#
# Development shortcuts:
#   LOCAL_BIN=1 ./build-cube-images.sh cubelet
#   ./build-cube-images.sh --local cube-shim
# Overlay binaries from _output/bin (make cubelet / make shim / ...) into the
# image build context. Pass one or more image names to build only those images.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
WORKTREE_ROOT="${REPO_ROOT}"

VERSION="${VERSION:-v0.5.1}"
IMAGE_TAG="${IMAGE_TAG:-${VERSION}}"
REGISTRY="${REGISTRY:-ccr.ccs.tencentyun.com/cubesandbox-chart}"
# SOURCE_REF pins the CubeMaster / CubeAPI / CubeProxy / CubeEgress /
# cube-lifecycle-manager / web / deploy/one-click/webui source tree used when
# building cube-api / cube-proxy / cube-egress / cube-lifecycle-manager /
# cube-webui from repository source, ensuring the delivered images match the
# release tag rather than the current worktree. Set SOURCE_REF="" to build from
# the current worktree.
SOURCE_REF="${SOURCE_REF-${VERSION}}"
PUSH="${PUSH:-0}"
NO_CACHE="${NO_CACHE:-0}"
LOCAL_BIN="${LOCAL_BIN:-0}"
LOCAL_BIN_DIR="${LOCAL_BIN_DIR:-${WORKTREE_ROOT}/_output/bin}"
BUILD_ROOT="${BUILD_ROOT:-/tmp/cube-kubernetes-images-${VERSION}}"
CUBE_NODE_BASE_IMAGE="${CUBE_NODE_BASE_IMAGE:-}"
CUBE_EGRESS_OPENRESTY_BASE_IMAGE="${CUBE_EGRESS_OPENRESTY_BASE_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/openresty-tproxy}"
# CubeProxy / webui OpenResty bases. Private TCR defaults need auth; override to
# a reachable image (same knobs CI passes as OPENRESTY_BASE_IMAGE).
CUBE_PROXY_BASE_IMAGE="${CUBE_PROXY_BASE_IMAGE:-openresty/openresty:1.21.4.1-6-alpine-fat}"
OPENRESTY_BASE_IMAGE="${OPENRESTY_BASE_IMAGE:-${CUBE_PROXY_BASE_IMAGE}}"
# Match CI release-docker-images.yml metadata build-args for webui / LCM.
CUBE_COMMIT="${CUBE_COMMIT:-$(git -C "${WORKTREE_ROOT}" rev-parse HEAD 2>/dev/null || echo unknown)}"
CUBE_BUILD_TIME="${CUBE_BUILD_TIME:-$(date -u +'%Y-%m-%dT%H:%M:%SZ')}"
# webui Dockerfile.dockerignore requires BuildKit (per-Dockerfile ignore files).
export DOCKER_BUILDKIT="${DOCKER_BUILDKIT:-1}"

ONE_CLICK_ARCH="${ONE_CLICK_ARCH:-amd64}"
# MIRROR selects the release download origin for one-click + PVM host packages:
#   cn  -> https://cnb.cool/CubeSandbox/CubeSandbox/-/releases (China)
#   ""  -> https://github.com/TencentCloud/CubeSandbox/releases (default / overseas)
# Explicit ONE_CLICK_URL / PVM_KERNEL_*_URL overrides still win.
MIRROR="${MIRROR:-}"
ONE_CLICK_ARTIFACT="${ONE_CLICK_ARTIFACT:-cube-sandbox-one-click-${VERSION}-${ONE_CLICK_ARCH}.tar.gz}"
PVM_KERNEL_RPM_ARTIFACT="${PVM_KERNEL_RPM_ARTIFACT:-kernel-6.6.69_opencloudos9.cubesandbox.pvm.host_gb85200d80fa2-1.x86_64.rpm}"
PVM_KERNEL_DEB_ARTIFACT="${PVM_KERNEL_DEB_ARTIFACT:-linux-image-6.6.69-opencloudos9.cubesandbox.pvm.host-gb85200d80fa2_6.6.69-gb85200d80fa2-1_amd64.deb}"
case "${MIRROR}" in
  cn|CN|cnb|CNB)
    RELEASE_DOWNLOAD_BASE="https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/${VERSION}"
    ;;
  ""|github|GITHUB|gh|GH)
    RELEASE_DOWNLOAD_BASE="https://github.com/TencentCloud/CubeSandbox/releases/download/${VERSION}"
    ;;
  *)
    printf '[build-cube-images] ERROR: unsupported MIRROR=%s (use cn or github)\n' "${MIRROR}" >&2
    exit 1
    ;;
esac
ONE_CLICK_URL="${ONE_CLICK_URL:-${RELEASE_DOWNLOAD_BASE}/${ONE_CLICK_ARTIFACT}}"
PVM_KERNEL_RPM_URL="${PVM_KERNEL_RPM_URL:-${RELEASE_DOWNLOAD_BASE}/${PVM_KERNEL_RPM_ARTIFACT}}"
PVM_KERNEL_DEB_URL="${PVM_KERNEL_DEB_URL:-${RELEASE_DOWNLOAD_BASE}/${PVM_KERNEL_DEB_ARTIFACT}}"

# Optional SHA256 checksums for the downloaded artifacts. When set, the
# download function refuses to accept a mismatching file. Chart operators
# should always set these when publishing images to protect the build against
# release-mirror tampering. Leave empty for interactive development builds.
ONE_CLICK_SHA256="${ONE_CLICK_SHA256:-}"
PVM_KERNEL_RPM_SHA256="${PVM_KERNEL_RPM_SHA256:-}"
PVM_KERNEL_DEB_SHA256="${PVM_KERNEL_DEB_SHA256:-}"

# Bake kernel packages into cube-pvm-host-bootstrap by default so delivery only
# depends on normal image pulls from the target registry.
INCLUDE_PVM_KERNEL_RPM="${INCLUDE_PVM_KERNEL_RPM:-1}"
INCLUDE_PVM_KERNEL_DEB="${INCLUDE_PVM_KERNEL_DEB:-1}"
DOWNLOAD_RETRIES="${DOWNLOAD_RETRIES:-5}"
DOWNLOAD_CONNECT_TIMEOUT="${DOWNLOAD_CONNECT_TIMEOUT:-20}"

ALL_IMAGES=(
  cube-master
  cube-api
  cubemastercli
  cube-proxy
  cube-lifecycle-manager
  cube-egress
  cube-egress-net
  cube-webui
  cubelet
  network-agent
  cube-shim
  cube-kernel
  cube-guest
  cube-node-init
  cube-wait-node-prep
  cube-pvm-host-bootstrap
  pause
)

# Images that need sandbox-package layout/binaries.
PACKAGE_IMAGES=(
  cube-master
  cubemastercli
  cubelet
  network-agent
  cube-shim
  cube-kernel
  cube-guest
)

# Images that read source trees under REPO_ROOT (worktree or SOURCE_REF export).
SOURCE_IMAGES=(
  cube-master
  cube-api
  cube-proxy
  cube-lifecycle-manager
  cube-egress
  cube-egress-net
  cube-webui
)

SELECTED_IMAGES=()
BUILT_IMAGES=()
PACKAGE_READY=0
SOURCE_READY=0
PAUSE_BUILT_TAG=""

log() { printf '[build-cube-images] %s\n' "$*"; }
fail() { printf '[build-cube-images] ERROR: %s\n' "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

usage() {
  cat <<EOF
Usage: $(basename "$0") [options] [image...]

Build CubeSandbox Kubernetes delivery images.

Options:
  -h, --help     Show this help
  --local        Overlay binaries from LOCAL_BIN_DIR (default: <repo>/_output/bin)
                 Equivalent to LOCAL_BIN=1

Environment:
  VERSION / IMAGE_TAG / REGISTRY / SOURCE_REF / PUSH / NO_CACHE / BUILD_ROOT
  LOCAL_BIN / LOCAL_BIN_DIR / PACKAGE_DIR_OVERRIDE / MIRROR
  CUBE_PROXY_BASE_IMAGE / OPENRESTY_BASE_IMAGE / CUBE_EGRESS_OPENRESTY_BASE_IMAGE

When no image names are given, all images are built. With --local / LOCAL_BIN=1,
package-based images overlay matching binaries from make output:

  cubelet         <- cubelet, cubecli                 (make cubelet)
  cube-shim       <- containerd-shim-cube-rs, cube-runtime  (make shim)
  network-agent   <- network-agent                    (make network-agent)
  cube-master     <- cubemaster                       (make cubemaster)
  cubemastercli   <- cubemastercli                    (make cubemaster)

Examples:
  make cubelet
  LOCAL_BIN=1 IMAGE_TAG=dev $0 cubelet

  $0 --local cube-shim
  SOURCE_REF="" IMAGE_TAG=dev $0 cube-api

Available images:
$(printf '  %s\n' "${ALL_IMAGES[@]}")
EOF
}

is_known_image() {
  local candidate="$1"
  local name
  for name in "${ALL_IMAGES[@]}"; do
    if [[ "${name}" == "${candidate}" ]]; then
      return 0
    fi
  done
  return 1
}

parse_args() {
  local arg
  while [[ $# -gt 0 ]]; do
    arg="$1"
    shift
    case "${arg}" in
      -h|--help)
        usage
        exit 0
        ;;
      --local)
        LOCAL_BIN=1
        ;;
      --)
        while [[ $# -gt 0 ]]; do
          SELECTED_IMAGES+=("$1")
          shift
        done
        ;;
      -*)
        fail "unknown option: ${arg} (try --help)"
        ;;
      *)
        SELECTED_IMAGES+=("${arg}")
        ;;
    esac
  done

  if [[ ${#SELECTED_IMAGES[@]} -eq 0 ]]; then
    SELECTED_IMAGES=("${ALL_IMAGES[@]}")
  else
    local name
    for name in "${SELECTED_IMAGES[@]}"; do
      is_known_image "${name}" || fail "unknown image '${name}'. Valid: ${ALL_IMAGES[*]}"
    done
  fi
}

should_build() {
  local candidate="$1"
  local name
  for name in "${SELECTED_IMAGES[@]}"; do
    if [[ "${name}" == "${candidate}" ]]; then
      return 0
    fi
  done
  return 1
}

selection_needs_package() {
  local name
  for name in "${PACKAGE_IMAGES[@]}"; do
    should_build "${name}" && return 0
  done
  return 1
}

selection_needs_source() {
  local name
  for name in "${SOURCE_IMAGES[@]}"; do
    should_build "${name}" && return 0
  done
  return 1
}

record_built() {
  BUILT_IMAGES+=("$1")
}

validate_download() {
  local out="$1"
  local validator="${2:-file}"
  local expected_sha="${3:-}"
  case "${validator}" in
    tar.gz)
      tar -tzf "${out}" >/dev/null || return 1
      ;;
    file)
      [[ -s "${out}" ]] || return 1
      ;;
    *)
      fail "unknown download validator: ${validator}"
      ;;
  esac
  if [[ -n "${expected_sha}" ]]; then
    local actual_sha
    actual_sha="$(sha256sum "${out}" | awk '{print $1}')"
    if [[ "${actual_sha}" != "${expected_sha}" ]]; then
      log "sha256 mismatch on ${out}: expected ${expected_sha} got ${actual_sha}"
      return 1
    fi
  fi
  return 0
}

download_file() {
  local url="$1"
  local out="$2"
  local validator="${3:-file}"
  local expected_sha="${4:-}"
  local attempt

  if [[ -f "${out}" ]]; then
    if validate_download "${out}" "${validator}" "${expected_sha}"; then
      log "reusing existing download: ${out}"
      return 0
    fi
    log "existing download is invalid, redownloading: ${out}"
    rm -f "${out}"
  fi

  mkdir -p "$(dirname "${out}")"
  for attempt in $(seq 1 "${DOWNLOAD_RETRIES}"); do
    log "downloading $(basename "${out}") attempt ${attempt}/${DOWNLOAD_RETRIES}: ${url}"
    local curl_extra_args=()
    # --retry-all-errors was introduced in curl 7.71. Enable only when
    # available AND we're not concerned about false-positive retries on 4xx.
    # In practice CubeSandbox artifacts are static: a 4xx from SourceForge
    # means the URL is wrong (typo, moved), not a transient issue, so keep
    # it opt-in via CURL_RETRY_ALL_ERRORS=1.
    if [[ "${CURL_RETRY_ALL_ERRORS:-0}" == "1" ]] \
       && curl --help all 2>/dev/null | grep -q -- '--retry-all-errors'; then
      curl_extra_args+=(--retry-all-errors)
    fi
    if curl \
      --fail \
      --location \
      --continue-at - \
      --retry 3 \
      "${curl_extra_args[@]}" \
      --retry-delay 5 \
      --connect-timeout "${DOWNLOAD_CONNECT_TIMEOUT}" \
      --show-error \
      --progress-bar \
      -o "${out}" \
      "${url}"; then
      if validate_download "${out}" "${validator}" "${expected_sha}"; then
        return 0
      fi
      log "downloaded file failed ${validator} validation: ${out}"
    fi
    if [[ "${attempt}" != "${DOWNLOAD_RETRIES}" ]]; then
      log "retrying download after 5 seconds"
      sleep 5
    fi
  done
  fail "failed to download valid file after ${DOWNLOAD_RETRIES} attempts: ${url}"
}

ensure_source_tree() {
  [[ "${SOURCE_READY}" == "1" ]] && return 0
  SOURCE_READY=1

  # When SOURCE_REF is set (default: ${VERSION}), export the CubeMaster / CubeAPI /
  # CubeProxy / CubeEgress / cube-lifecycle-manager / web / deploy/one-click/webui
  # trees at that ref into ${SOURCE_TREE_DIR} and point REPO_ROOT there. This
  # ensures cube-api, cube-proxy, cube-egress, cube-lifecycle-manager,
  # cube-webui and related contexts are compiled from the release-tag source, not
  # from whatever happens to be in the current worktree (which may be ahead of
  # the tag).
  if [[ -z "${SOURCE_REF}" ]]; then
    REPO_ROOT="${WORKTREE_ROOT}"
    log "using current worktree source (SOURCE_REF empty)"
    return 0
  fi

  need git
  git -C "${WORKTREE_ROOT}" rev-parse --verify "${SOURCE_REF}^{commit}" >/dev/null 2>&1 \
    || fail "SOURCE_REF=${SOURCE_REF} is not a valid git ref in ${WORKTREE_ROOT}"
  SOURCE_REF_SHA="$(git -C "${WORKTREE_ROOT}" rev-parse "${SOURCE_REF}^{commit}")"
  SOURCE_TREE_STAMP="${SOURCE_TREE_DIR}/.exported-sha"
  SOURCE_EXPORT_SET="CubeMaster CubeAPI CubeProxy CubeEgress cube-lifecycle-manager web deploy/one-click/webui"
  if [[ ! -f "${SOURCE_TREE_STAMP}" ]] \
    || [[ "$(cat "${SOURCE_TREE_STAMP}")" != "${SOURCE_REF_SHA}"$'\n'"${SOURCE_EXPORT_SET}" ]]; then
    log "exporting source tree at ${SOURCE_REF} (${SOURCE_REF_SHA:0:12}) into ${SOURCE_TREE_DIR}"
    rm -rf "${SOURCE_TREE_DIR}"
    mkdir -p "${SOURCE_TREE_DIR}"
    for module in ${SOURCE_EXPORT_SET}; do
      git -C "${WORKTREE_ROOT}" archive --format=tar "${SOURCE_REF_SHA}" -- "${module}" \
        | tar -C "${SOURCE_TREE_DIR}" -x \
        || fail "failed to export ${module} at ${SOURCE_REF}"
    done
    printf '%s\n%s\n' "${SOURCE_REF_SHA}" "${SOURCE_EXPORT_SET}" > "${SOURCE_TREE_STAMP}"
  else
    log "reusing exported source tree at ${SOURCE_REF} (${SOURCE_REF_SHA:0:12})"
  fi
  REPO_ROOT="${SOURCE_TREE_DIR}"
}

ensure_package_dir() {
  [[ "${PACKAGE_READY}" == "1" ]] && return 0
  PACKAGE_READY=1

  if [[ -n "${PACKAGE_DIR_OVERRIDE:-}" ]]; then
    PACKAGE_DIR="${PACKAGE_DIR_OVERRIDE}"
    [[ -d "${PACKAGE_DIR}" ]] || fail "PACKAGE_DIR_OVERRIDE does not exist: ${PACKAGE_DIR}"
  else
    if [[ ! -f "${ONE_CLICK_TAR}" ]] || ! tar -tzf "${ONE_CLICK_TAR}" >/dev/null 2>&1; then
      log "downloading one-click release package: ${ONE_CLICK_URL}"
      download_file "${ONE_CLICK_URL}" "${ONE_CLICK_TAR}" tar.gz "${ONE_CLICK_SHA256}"
    fi
    if [[ ! -f "${SANDBOX_PACKAGE_TAR}" ]]; then
      log "extracting one-click release package"
      rm -rf "${EXTRACT_DIR}/${ONE_CLICK_DIRNAME}"
      tar -C "${EXTRACT_DIR}" -xzf "${ONE_CLICK_TAR}"
    fi
    if [[ ! -d "${PACKAGE_DIR}" ]]; then
      log "extracting sandbox-package"
      rm -rf "${PACKAGE_DIR}"
      mkdir -p "${BUILD_ROOT}"
      tar -C "${BUILD_ROOT}" -xzf "${SANDBOX_PACKAGE_TAR}"
    fi
  fi

  [[ -d "${PACKAGE_DIR}/CubeMaster" ]] || fail "invalid package dir: missing CubeMaster"
  [[ -d "${PACKAGE_DIR}/Cubelet" ]] || fail "invalid package dir: missing Cubelet"
  [[ -d "${PACKAGE_DIR}/CubeAPI" ]] || fail "invalid package dir: missing CubeAPI"
}

make_hint_for_bin() {
  case "$1" in
    cubelet|cubecli) printf 'make cubelet' ;;
    containerd-shim-cube-rs|cube-runtime) printf 'make shim' ;;
    network-agent|cubevsmapdump) printf 'make network-agent' ;;
    cubemaster|cubemastercli) printf 'make cubemaster' ;;
    *) printf 'make <component>' ;;
  esac
}

# Copy a locally built binary into the docker context. No-op unless LOCAL_BIN=1.
# Does not mutate PACKAGE_DIR / PACKAGE_DIR_OVERRIDE.
overlay_local_bin() {
  local src_name="$1"
  local dest_path="$2"
  local required="${3:-1}"
  local src="${LOCAL_BIN_DIR}/${src_name}"

  [[ "${LOCAL_BIN}" == "1" ]] || return 0

  if [[ ! -x "${src}" ]]; then
    if [[ "${required}" == "1" ]]; then
      fail "missing local binary ${src}; run: $(make_hint_for_bin "${src_name}")"
    fi
    return 0
  fi
  mkdir -p "$(dirname "${dest_path}")"
  cp "${src}" "${dest_path}"
  chmod +x "${dest_path}"
  log "overlay local binary ${src} -> ${dest_path}"
}

overlay_local_bins_for_component() {
  local name="$1"
  local ctx="$2"
  local pkg_basename="$3"

  case "${name}" in
    cubelet)
      overlay_local_bin cubelet "${ctx}/package/${pkg_basename}/bin/cubelet"
      overlay_local_bin cubecli "${ctx}/package/${pkg_basename}/bin/cubecli"
      ;;
    cube-shim)
      overlay_local_bin containerd-shim-cube-rs \
        "${ctx}/package/${pkg_basename}/bin/containerd-shim-cube-rs"
      overlay_local_bin cube-runtime \
        "${ctx}/package/${pkg_basename}/bin/cube-runtime"
      ;;
    network-agent)
      overlay_local_bin network-agent "${ctx}/package/${pkg_basename}/bin/network-agent"
      overlay_local_bin cubevsmapdump "${ctx}/package/${pkg_basename}/bin/cubevsmapdump" 0
      ;;
  esac
}

copy_scripts() {
  local ctx="$1"
  shift
  mkdir -p "${ctx}/scripts"
  for script in "$@"; do
    cp "${SCRIPT_DIR}/scripts/${script}" "${ctx}/scripts/${script}"
    chmod +x "${ctx}/scripts/${script}"
  done
}

prepare_context() {
  local name="$1"
  local ctx="${CONTEXT_DIR}/${name}"
  rm -rf "${ctx}"
  mkdir -p "${ctx}/package" "${ctx}/scripts" "${ctx}/artifacts"
  printf '%s\n' "${ctx}"
}

copy_cube_master_component_context() {
  local ctx="$1"
  local src="${PACKAGE_DIR}/CubeMaster"
  local bin="${src}/bin/cubemaster"

  [[ -x "${bin}" ]] || fail "invalid CubeMaster package: missing executable ${bin}"
  [[ -f "${REPO_ROOT}/CubeMaster/docker/tools/gracestop.sh" ]] || fail "missing CubeMaster docker tools/gracestop.sh"

  cp "${bin}" "${ctx}/cubemaster"
  chmod +x "${ctx}/cubemaster"
  overlay_local_bin cubemaster "${ctx}/cubemaster"
  cp -a "${REPO_ROOT}/CubeMaster/docker/tools" "${ctx}/tools"
}

copy_cubemastercli_context() {
  local ctx="$1"
  local src="${PACKAGE_DIR}/CubeMaster"
  local bin="${src}/bin/cubemastercli"

  [[ -x "${bin}" ]] || fail "invalid CubeMaster package: missing executable ${bin}"

  cp "${bin}" "${ctx}/cubemastercli"
  chmod +x "${ctx}/cubemastercli"
  overlay_local_bin cubemastercli "${ctx}/cubemastercli"
}

copy_cube_proxy_component_context() {
  local ctx="$1"
  local src="${REPO_ROOT}/CubeProxy"

  [[ -f "${src}/nginx.conf" ]] || fail "missing CubeProxy nginx.conf"
  [[ -d "${src}/conf/includes" ]] || fail "missing CubeProxy conf/includes"

  # Auto-pause/resume moved out of cube-proxy-sidecar into the standalone
  # cube-lifecycle-manager image. cube-proxy is nginx/OpenResty only.
  cp -a "${src}/lua" "${ctx}/lua"
  mkdir -p "${ctx}/conf"
  cp -a "${src}/conf/includes" "${ctx}/conf/includes"
  cp "${src}/nginx.conf" "${ctx}/nginx.conf"
  cp "${src}/rotate_nginx_log.sh" "${ctx}/rotate_nginx_log.sh"
  cp "${src}/root" "${ctx}/root"
  cp "${src}/start.sh" "${ctx}/start.sh"
}

build_cube_lifecycle_manager_image() {
  local src="${REPO_ROOT}/cube-lifecycle-manager"
  [[ -f "${src}/Dockerfile" ]] || fail "missing cube-lifecycle-manager Dockerfile"
  [[ -f "${src}/go.mod" ]] || fail "missing cube-lifecycle-manager go.mod"
  build_image cube-lifecycle-manager "${src}" "${src}/Dockerfile" \
    --build-arg "CUBE_VERSION=${IMAGE_TAG}" \
    --build-arg "CUBE_COMMIT=${CUBE_COMMIT}" \
    --build-arg "CUBE_BUILD_TIME=${CUBE_BUILD_TIME}"
  record_built cube-lifecycle-manager
}

# Same as .github/workflows/release-docker-images.yml for component "webui":
# context=., file=deploy/one-click/webui/Dockerfile, OPENRESTY_BASE_IMAGE + CUBE_*.
build_cube_webui_image() {
  local dockerfile="${REPO_ROOT}/deploy/one-click/webui/Dockerfile"
  local image="${REGISTRY}/cube-webui:${IMAGE_TAG}"
  local docker_args=(
    -f "${dockerfile}"
    -t "${image}"
    --build-arg "CUBE_VERSION=${IMAGE_TAG}"
    --build-arg "CUBE_COMMIT=${CUBE_COMMIT}"
    --build-arg "CUBE_BUILD_TIME=${CUBE_BUILD_TIME}"
    --build-arg "OPENRESTY_BASE_IMAGE=${OPENRESTY_BASE_IMAGE}"
    --build-arg "CUBE_PROXY_BASE_IMAGE=${CUBE_PROXY_BASE_IMAGE}"
  )
  [[ -f "${dockerfile}" ]] || fail "missing webui Dockerfile: ${dockerfile}"
  [[ -f "${REPO_ROOT}/web/package.json" ]] || fail "missing web/ frontend sources in ${REPO_ROOT}"
  if [[ "${NO_CACHE}" == "1" ]]; then
    docker_args=(--no-cache --pull "${docker_args[@]}")
  fi
  log "building ${image} from ${dockerfile} (context=${REPO_ROOT}, base=${OPENRESTY_BASE_IMAGE})"
  docker build "${docker_args[@]}" "${REPO_ROOT}"
  if [[ "${PUSH}" == "1" ]]; then
    log "pushing ${image}"
    docker push "${image}"
  fi
  record_built cube-webui
}

build_cube_proxy_image() {
  local ctx="$1"
  local dockerfile="${REPO_ROOT}/CubeProxy/Dockerfile"
  local image="${REGISTRY}/cube-proxy:${IMAGE_TAG}"
  local docker_args=(
    -f "${dockerfile}"
    -t "${image}"
    --build-arg "CUBE_PROXY_BASE_IMAGE=${CUBE_PROXY_BASE_IMAGE}"
  )
  if [[ "${NO_CACHE}" == "1" ]]; then
    docker_args=(--no-cache --pull "${docker_args[@]}")
  fi
  log "building ${image} from ${dockerfile} (base=${CUBE_PROXY_BASE_IMAGE})"
  docker build "${docker_args[@]}" "${ctx}"
  if [[ "${PUSH}" == "1" ]]; then
    log "pushing ${image}"
    docker push "${image}"
  fi
  record_built cube-proxy
}

build_image() {
  local name="$1"
  local ctx="$2"
  shift 2
  local dockerfile="${SCRIPT_DIR}/${name}/Dockerfile"
  if [[ $# -gt 0 && "$1" != --* ]]; then
    dockerfile="$1"
    shift
  fi
  local image="${REGISTRY}/${name}:${IMAGE_TAG}"
  local docker_args=(-f "${dockerfile}" -t "${image}" "$@")
  if [[ "${NO_CACHE}" == "1" ]]; then
    docker_args=(--no-cache --pull "${docker_args[@]}")
  fi
  log "building ${image} from ${dockerfile}"
  docker build "${docker_args[@]}" "${ctx}"
  if [[ "${PUSH}" == "1" ]]; then
    log "pushing ${image}"
    docker push "${image}"
  fi
}

build_cube_api_image() {
  local dockerfile="${CONTEXT_DIR}/cube-api.Dockerfile"

  # CubeAPI/Dockerfile first compiles a dummy main to cache dependencies. Docker
  # preserves source mtimes on COPY, so Cargo can incorrectly keep that dummy
  # binary if the real src/main.rs is older than the cached artifact. Keep the
  # upstream Dockerfile unchanged and inject one cache-busting cleanup layer for
  # Kubernetes image builds.
  awk '
    {
      print
      if ($0 == "COPY src/ src/") {
        print "RUN rust_target=\"$(cat /etc/rust-target)\" \\"
        print "    && rm -f \"target/${rust_target}/release/cube-api\" target/${rust_target}/release/deps/cube_api-*"
      }
    }
  ' "${REPO_ROOT}/CubeAPI/Dockerfile" > "${dockerfile}"

  build_image cube-api "${REPO_ROOT}/CubeAPI" "${dockerfile}"
  record_built cube-api
}

build_cube_egress_openresty_base_image() {
  local image="cube-egress/openresty:1.29.2.5-tproxy"
  local docker_args=(
    -f "${REPO_ROOT}/CubeEgress/openresty/Dockerfile"
    -t "${image}"
    -t "${CUBE_EGRESS_OPENRESTY_BASE_IMAGE}"
  )
  if [[ "${NO_CACHE}" == "1" ]]; then
    docker_args=(--no-cache --pull "${docker_args[@]}")
  fi
  log "building ${image} from ${REPO_ROOT}/CubeEgress/openresty/Dockerfile"
  log "tagging ${image} as ${CUBE_EGRESS_OPENRESTY_BASE_IMAGE} for CubeEgress/Dockerfile"
  docker build "${docker_args[@]}" "${REPO_ROOT}/CubeEgress/openresty"
}

build_cube_egress_image() {
  local image="${REGISTRY}/cube-egress:${IMAGE_TAG}"
  local docker_args=(
    -f "${REPO_ROOT}/CubeEgress/Dockerfile"
    -t "${image}"
    --build-arg "CUBE_EGRESS_VERSION=${VERSION}"
    --build-arg "CUBE_EGRESS_COMMIT=$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
    --build-arg "CUBE_EGRESS_BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  )
  if [[ "${NO_CACHE}" == "1" ]]; then
    # Do not add --pull here: CubeEgress/Dockerfile uses a fixed FROM name.
    # build_cube_egress_openresty_base_image tags the locally built base with
    # that exact name so the egress image remains reproducible.
    docker_args=(--no-cache "${docker_args[@]}")
  fi
  log "building ${image} from ${REPO_ROOT}/CubeEgress/Dockerfile"
  docker build "${docker_args[@]}" "${REPO_ROOT}/CubeEgress"
  if [[ "${PUSH}" == "1" ]]; then
    log "pushing ${image}"
    docker push "${image}"
  fi
  record_built cube-egress
}

build_cube_node_from_base_image() {
  local ctx
  local dockerfile

  [[ -n "${CUBE_NODE_BASE_IMAGE}" ]] || fail "CUBE_NODE_BASE_IMAGE is required"
  ctx="$(prepare_context cube-node)"
  copy_scripts "${ctx}" cube-node-entrypoint.sh stage-toolbox.sh
  dockerfile="${ctx}/Dockerfile.rebase"

  cat > "${dockerfile}" <<EOF
FROM ${CUBE_NODE_BASE_IMAGE}

COPY scripts/cube-node-entrypoint.sh /usr/local/bin/cube-node-entrypoint.sh
COPY scripts/stage-toolbox.sh /usr/local/bin/stage-toolbox.sh

RUN chmod +x /usr/local/bin/cube-node-entrypoint.sh /usr/local/bin/stage-toolbox.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/cube-node-entrypoint.sh"]
EOF

  build_image cube-node "${ctx}" "${dockerfile}"
}

copy_cube_egress_net_context() {
  local ctx="$1"
  local init_script="${REPO_ROOT}/CubeEgress/scripts/cube-proxy-iptables-init.sh"

  [[ -f "${init_script}" ]] || fail "missing CubeEgress network init script: ${init_script}"
  cp "${init_script}" "${ctx}/scripts/cube-proxy-iptables-init.sh"
  cp "${SCRIPT_DIR}/scripts/cube-egress-net-entrypoint.sh" "${ctx}/scripts/cube-egress-net-entrypoint.sh"
  chmod +x \
    "${ctx}/scripts/cube-proxy-iptables-init.sh" \
    "${ctx}/scripts/cube-egress-net-entrypoint.sh"
}

# Big Pod REV3: per-component images (no monolithic cube-node).
build_component_image() {
  local name="$1"
  local pkg_dir="$2"
  local ctx
  local pkg_basename
  [[ -d "${PACKAGE_DIR}/${pkg_dir}" ]] || fail "invalid sandbox-package: missing ${pkg_dir} for ${name}"
  pkg_basename="$(basename "${pkg_dir}")"
  ctx="$(prepare_context "${name}")"
  copy_scripts "${ctx}" component-entrypoint.sh
  mkdir -p "${ctx}/package"
  cp -a "${PACKAGE_DIR}/${pkg_dir}" "${ctx}/package/${pkg_basename}"
  overlay_local_bins_for_component "${name}" "${ctx}" "${pkg_basename}"
  build_image "${name}" "${ctx}"
  record_built "${name}"
}

run_selected_builds() {
  local ctx

  if should_build cube-master; then
    ensure_package_dir
    ensure_source_tree
    ctx="$(prepare_context cube-master)"
    copy_cube_master_component_context "${ctx}"
    build_image cube-master "${ctx}" "${REPO_ROOT}/CubeMaster/docker/Dockerfile"
    record_built cube-master
  fi

  if should_build cube-api; then
    ensure_source_tree
    build_cube_api_image
  fi

  if should_build cubemastercli; then
    ensure_package_dir
    ctx="$(prepare_context cubemastercli)"
    copy_cubemastercli_context "${ctx}"
    build_image cubemastercli "${ctx}"
    record_built cubemastercli
  fi

  if should_build cube-proxy; then
    ensure_source_tree
    ctx="$(prepare_context cube-proxy)"
    copy_cube_proxy_component_context "${ctx}"
    build_cube_proxy_image "${ctx}"
  fi

  if should_build cube-lifecycle-manager; then
    ensure_source_tree
    build_cube_lifecycle_manager_image
  fi

  if should_build cube-egress; then
    ensure_source_tree
    build_cube_egress_openresty_base_image
    build_cube_egress_image
  fi

  if should_build cube-egress-net; then
    ensure_source_tree
    ctx="$(prepare_context cube-egress-net)"
    copy_cube_egress_net_context "${ctx}"
    build_image cube-egress-net "${ctx}"
    record_built cube-egress-net
  fi

  if should_build cube-webui; then
    ensure_source_tree
    build_cube_webui_image
  fi

  if should_build cubelet; then
    ensure_package_dir
    build_component_image cubelet Cubelet
  fi
  if should_build network-agent; then
    ensure_package_dir
    build_component_image network-agent network-agent
  fi
  if should_build cube-shim; then
    ensure_package_dir
    build_component_image cube-shim cube-shim
  fi
  if should_build cube-kernel; then
    ensure_package_dir
    build_component_image cube-kernel cube-kernel-scf
  fi
  if should_build cube-guest; then
    ensure_package_dir
    build_component_image cube-guest cube-image
  fi

  if should_build cube-node-init; then
    ctx="$(prepare_context cube-node-init)"
    copy_scripts "${ctx}" cube-node-init.sh wait-pvm-host.sh node-prep-lib.sh
    build_image cube-node-init "${ctx}"
    record_built cube-node-init
  fi

  if should_build cube-wait-node-prep; then
    ctx="$(prepare_context cube-wait-node-prep)"
    copy_scripts "${ctx}" node-prep-lib.sh wait-node-prep.sh write-node-prep-ready.sh
    build_image cube-wait-node-prep "${ctx}"
    record_built cube-wait-node-prep
  fi

  if should_build cube-pvm-host-bootstrap; then
    ctx="$(prepare_context cube-pvm-host-bootstrap)"
    copy_scripts "${ctx}" pvm-host-bootstrap.sh node-prep-lib.sh
    if [[ "${INCLUDE_PVM_KERNEL_RPM}" == "1" ]]; then
      log "downloading PVM host kernel rpm for bootstrap image"
      download_file "${PVM_KERNEL_RPM_URL}" "${PVM_KERNEL_RPM}" file "${PVM_KERNEL_RPM_SHA256}"
      cp "${PVM_KERNEL_RPM}" "${ctx}/artifacts/kernel-pvm-host.rpm"
    fi
    if [[ "${INCLUDE_PVM_KERNEL_DEB}" == "1" ]]; then
      log "downloading PVM host kernel deb for bootstrap image"
      download_file "${PVM_KERNEL_DEB_URL}" "${PVM_KERNEL_DEB}" file "${PVM_KERNEL_DEB_SHA256}"
      cp "${PVM_KERNEL_DEB}" "${ctx}/artifacts/linux-image-pvm-host.deb"
    fi
    build_image cube-pvm-host-bootstrap "${ctx}"
    record_built cube-pvm-host-bootstrap
  fi

  if should_build pause; then
    # Chart values default images.pause.tag=3.9 (upstream pause version), not IMAGE_TAG.
    local pause_tag="${PAUSE_TAG:-3.9}"
    local pause_ctx="${SCRIPT_DIR}/pause"
    local pause_dockerfile="${pause_ctx}/Dockerfile"
    local pause_image="${REGISTRY}/pause:${pause_tag}"
    local docker_args=(-f "${pause_dockerfile}" -t "${pause_image}")
    [[ -f "${pause_dockerfile}" ]] || fail "missing pause Dockerfile: ${pause_dockerfile}"
    if [[ "${NO_CACHE}" == "1" ]]; then
      docker_args=(--no-cache --pull "${docker_args[@]}")
    fi
    log "building ${pause_image} from ${pause_dockerfile}"
    docker build "${docker_args[@]}" "${pause_ctx}"
    if [[ "${PUSH}" == "1" ]]; then
      log "pushing ${pause_image}"
      docker push "${pause_image}"
    fi
    # record_built uses IMAGE_TAG in summary; print pause tag explicitly via BUILT_IMAGES name only
    BUILT_IMAGES+=("pause")
    # Override summary line by storing custom - print_summary uses IMAGE_TAG; fix via side channel
    PAUSE_BUILT_TAG="${pause_tag}"
  fi
}

print_summary() {
  local name
  local tag
  cat <<EOF

Built CubeSandbox images:
EOF
  for name in "${BUILT_IMAGES[@]}"; do
    tag="${IMAGE_TAG}"
    if [[ "${name}" == "pause" ]]; then
      tag="${PAUSE_BUILT_TAG:-3.9}"
    fi
    printf '  %s/%s:%s\n' "${REGISTRY}" "${name}" "${tag}"
  done
  cat <<EOF

Use these values:
  images.*.repository: ${REGISTRY}/<image-name>
  images.*.tag: ${IMAGE_TAG}
  images.pause.tag: ${PAUSE_BUILT_TAG:-3.9} (when pause was built)

Template builder is not built by this script. The chart uses a dind image by
default and can be overridden through images.templateBuilder.* when needed.
EOF
}

# --- main ---
parse_args "$@"

need docker
need tar
need curl
need go
need sha256sum

DOWNLOAD_DIR="${BUILD_ROOT}/downloads"
EXTRACT_DIR="${BUILD_ROOT}/extract"
CONTEXT_DIR="${BUILD_ROOT}/contexts"
SOURCE_TREE_DIR="${BUILD_ROOT}/source-tree"
ONE_CLICK_DIRNAME="cube-sandbox-one-click-${VERSION}-${ONE_CLICK_ARCH}"
ONE_CLICK_TAR="${DOWNLOAD_DIR}/${ONE_CLICK_DIRNAME}.tar.gz"
PVM_KERNEL_RPM="${DOWNLOAD_DIR}/$(basename "${PVM_KERNEL_RPM_URL}")"
PVM_KERNEL_DEB="${DOWNLOAD_DIR}/$(basename "${PVM_KERNEL_DEB_URL}")"
SANDBOX_PACKAGE_TAR="${EXTRACT_DIR}/${ONE_CLICK_DIRNAME}/assets/package/sandbox-package.tar.gz"
PACKAGE_DIR="${BUILD_ROOT}/sandbox-package"

mkdir -p "${DOWNLOAD_DIR}" "${EXTRACT_DIR}" "${CONTEXT_DIR}"
log "release download base (${MIRROR:-github}): ${RELEASE_DOWNLOAD_BASE}"
log "selected images: ${SELECTED_IMAGES[*]}"
if [[ "${LOCAL_BIN}" == "1" ]]; then
  log "LOCAL_BIN=1; overlaying binaries from ${LOCAL_BIN_DIR}"
fi

# Eagerly prepare only what the selection needs so single-image builds stay fast.
if selection_needs_source; then
  ensure_source_tree
fi
if selection_needs_package; then
  ensure_package_dir
fi

run_selected_builds
print_summary
