#!/usr/bin/env bash
# Build and optionally push CubeSandbox images for the Kubernetes/TKE chart.
#
# This script builds role-specific images directly from the CubeSandbox release
# package (sandbox-package). cube-node intentionally uses
# deploy/kubernetes/images/cube-node/Dockerfile because it is a Kubernetes delivery image
# that bundles node-side runtime components.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

VERSION="${VERSION:-v0.5.0}"
IMAGE_TAG="${IMAGE_TAG:-${VERSION}}"
REGISTRY="${REGISTRY:-ccr.ccs.tencentyun.com/cubesandbox-chart}"
PUSH="${PUSH:-0}"
NO_CACHE="${NO_CACHE:-0}"
BUILD_ROOT="${BUILD_ROOT:-/tmp/cube-kubernetes-images-${VERSION}}"
CUBE_NODE_BASE_IMAGE="${CUBE_NODE_BASE_IMAGE:-}"
CUBE_EGRESS_OPENRESTY_BASE_IMAGE="${CUBE_EGRESS_OPENRESTY_BASE_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/openresty-tproxy}"

ONE_CLICK_ARCH="${ONE_CLICK_ARCH:-amd64}"
ONE_CLICK_URL="${ONE_CLICK_URL:-https://downloads.sourceforge.net/project/cubesandbox.mirror/${VERSION}/cube-sandbox-one-click-${VERSION}-${ONE_CLICK_ARCH}.tar.gz}"
PVM_KERNEL_RPM_URL="${PVM_KERNEL_RPM_URL:-https://downloads.sourceforge.net/project/cubesandbox.mirror/${VERSION}/kernel-6.6.69_opencloudos9.cubesandbox.pvm.host_gb85200d80fa2-1.x86_64.rpm}"
PVM_KERNEL_DEB_URL="${PVM_KERNEL_DEB_URL:-https://downloads.sourceforge.net/project/cubesandbox.mirror/${VERSION}/linux-image-6.6.69-opencloudos9.cubesandbox.pvm.host-gb85200d80fa2_6.6.69-gb85200d80fa2-1_amd64.deb}"

# Bake kernel packages into cube-pvm-host-bootstrap by default so delivery only
# depends on normal image pulls from the target registry.
INCLUDE_PVM_KERNEL_RPM="${INCLUDE_PVM_KERNEL_RPM:-1}"
INCLUDE_PVM_KERNEL_DEB="${INCLUDE_PVM_KERNEL_DEB:-1}"
DOWNLOAD_RETRIES="${DOWNLOAD_RETRIES:-5}"
DOWNLOAD_CONNECT_TIMEOUT="${DOWNLOAD_CONNECT_TIMEOUT:-20}"

log() { printf '[build-cube-images] %s\n' "$*"; }
fail() { printf '[build-cube-images] ERROR: %s\n' "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

validate_download() {
  local out="$1"
  local validator="${2:-file}"
  case "${validator}" in
    tar.gz)
      tar -tzf "${out}" >/dev/null
      ;;
    file)
      [[ -s "${out}" ]]
      ;;
    *)
      fail "unknown download validator: ${validator}"
      ;;
  esac
}

download_file() {
  local url="$1"
  local out="$2"
  local validator="${3:-file}"
  local attempt

  if [[ -f "${out}" ]]; then
    if validate_download "${out}" "${validator}"; then
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
    if curl --help all 2>/dev/null | grep -q -- '--retry-all-errors'; then
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
      if validate_download "${out}" "${validator}"; then
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

need docker
need tar
need curl
need go

DOWNLOAD_DIR="${BUILD_ROOT}/downloads"
EXTRACT_DIR="${BUILD_ROOT}/extract"
CONTEXT_DIR="${BUILD_ROOT}/contexts"
ONE_CLICK_DIRNAME="cube-sandbox-one-click-${VERSION}-${ONE_CLICK_ARCH}"
ONE_CLICK_TAR="${DOWNLOAD_DIR}/${ONE_CLICK_DIRNAME}.tar.gz"
PVM_KERNEL_RPM="${DOWNLOAD_DIR}/$(basename "${PVM_KERNEL_RPM_URL}")"
PVM_KERNEL_DEB="${DOWNLOAD_DIR}/$(basename "${PVM_KERNEL_DEB_URL}")"
SANDBOX_PACKAGE_TAR="${EXTRACT_DIR}/${ONE_CLICK_DIRNAME}/assets/package/sandbox-package.tar.gz"
PACKAGE_DIR="${BUILD_ROOT}/sandbox-package"

mkdir -p "${DOWNLOAD_DIR}" "${EXTRACT_DIR}" "${CONTEXT_DIR}"

if [[ -n "${PACKAGE_DIR_OVERRIDE:-}" ]]; then
  PACKAGE_DIR="${PACKAGE_DIR_OVERRIDE}"
  [[ -d "${PACKAGE_DIR}" ]] || fail "PACKAGE_DIR_OVERRIDE does not exist: ${PACKAGE_DIR}"
else
  if [[ ! -f "${ONE_CLICK_TAR}" ]] || ! tar -tzf "${ONE_CLICK_TAR}" >/dev/null 2>&1; then
    log "downloading one-click release package: ${ONE_CLICK_URL}"
    download_file "${ONE_CLICK_URL}" "${ONE_CLICK_TAR}" tar.gz
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
  cp -a "${REPO_ROOT}/CubeMaster/docker/tools" "${ctx}/tools"
}

copy_cubemastercli_context() {
  local ctx="$1"
  local src="${PACKAGE_DIR}/CubeMaster"
  local bin="${src}/bin/cubemastercli"

  [[ -x "${bin}" ]] || fail "invalid CubeMaster package: missing executable ${bin}"

  cp "${bin}" "${ctx}/cubemastercli"
  chmod +x "${ctx}/cubemastercli"
}

copy_cube_proxy_component_context() {
  local ctx="$1"
  local src="${REPO_ROOT}/CubeProxy"
  local sidecar_src="${src}/sidecar"
  local sidecar_out="${ctx}/bin/cube-proxy-sidecar"

  [[ -f "${src}/nginx.conf" ]] || fail "missing CubeProxy nginx.conf"
  [[ -d "${src}/conf/includes" ]] || fail "missing CubeProxy conf/includes"
  [[ -f "${sidecar_src}/go.mod" ]] || fail "missing CubeProxy sidecar source"

  cp -a "${src}/lua" "${ctx}/lua"
  mkdir -p "${ctx}/conf"
  cp -a "${src}/conf/includes" "${ctx}/conf/includes"
  cp "${src}/nginx.conf" "${ctx}/nginx.conf"
  cp "${src}/rotate_nginx_log.sh" "${ctx}/rotate_nginx_log.sh"
  cp "${src}/root" "${ctx}/root"
  cp "${src}/start.sh" "${ctx}/start.sh"

  mkdir -p "${ctx}/bin"
  log "building CubeProxy sidecar for cube-proxy-node image context"
  (
    cd "${sidecar_src}"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -tags 'netgo osusergo' -ldflags '-s -w' \
        -o "${sidecar_out}" ./cmd/sidecar
  )
  chmod +x "${sidecar_out}"
}

build_image() {
  local name="$1"
  local ctx="$2"
  local dockerfile="${3:-${SCRIPT_DIR}/${name}/Dockerfile}"
  local image="${REGISTRY}/${name}:${IMAGE_TAG}"
  local docker_args=(-f "${dockerfile}" -t "${image}")
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
}

build_cube_node_from_base_image() {
  local ctx
  local dockerfile

  [[ -n "${CUBE_NODE_BASE_IMAGE}" ]] || fail "CUBE_NODE_BASE_IMAGE is required"
  ctx="$(prepare_context cube-node)"
  copy_scripts "${ctx}" cube-node-entrypoint.sh
  dockerfile="${ctx}/Dockerfile.rebase"

  cat > "${dockerfile}" <<EOF
FROM ${CUBE_NODE_BASE_IMAGE}

COPY scripts/cube-node-entrypoint.sh /usr/local/bin/cube-node-entrypoint.sh

RUN chmod +x /usr/local/bin/cube-node-entrypoint.sh

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

ctx="$(prepare_context cube-master)"
copy_cube_master_component_context "${ctx}"
build_image cube-master "${ctx}" "${REPO_ROOT}/CubeMaster/docker/Dockerfile"

build_cube_api_image

ctx="$(prepare_context cubemastercli)"
copy_cubemastercli_context "${ctx}"
build_image cubemastercli "${ctx}"

ctx="$(prepare_context cube-proxy-node)"
copy_cube_proxy_component_context "${ctx}"
build_image cube-proxy-node "${ctx}" "${REPO_ROOT}/CubeProxy/Dockerfile"

build_cube_egress_openresty_base_image
build_cube_egress_image

ctx="$(prepare_context cube-egress-net)"
copy_cube_egress_net_context "${ctx}"
build_image cube-egress-net "${ctx}"

ctx="$(prepare_context cube-webui)"
[[ -d "${PACKAGE_DIR}/webui" ]] || fail "invalid sandbox-package: missing required cube-webui component webui"
cp -a "${PACKAGE_DIR}/webui" "${ctx}/package/webui"
[[ -f "${ctx}/package/webui/dist/index.html" ]] || fail "invalid webui package: missing dist/index.html"
[[ -f "${ctx}/package/webui/nginx.conf" ]] || fail "invalid webui package: missing nginx.conf"
build_image cube-webui "${ctx}"

if [[ -n "${CUBE_NODE_BASE_IMAGE}" ]]; then
  log "building cube-node by rebasing ${CUBE_NODE_BASE_IMAGE}"
  build_cube_node_from_base_image
else
  ctx="$(prepare_context cube-node)"
  copy_scripts "${ctx}" cube-node-entrypoint.sh
  for d in Cubelet network-agent cube-shim cube-kernel-scf cube-image cube-vs cube-snapshot; do
    [[ -d "${PACKAGE_DIR}/${d}" ]] || fail "invalid sandbox-package: missing required cube-node component ${d}"
    cp -a "${PACKAGE_DIR}/${d}" "${ctx}/package/${d}"
  done
  [[ -d "${PACKAGE_DIR}/scripts/common" ]] || fail "invalid sandbox-package: missing required cube-node component scripts/common"
  mkdir -p "${ctx}/package/scripts"
  cp -a "${PACKAGE_DIR}/scripts/common" "${ctx}/package/scripts/common"
  build_image cube-node "${ctx}"
fi

ctx="$(prepare_context cube-node-init)"
copy_scripts "${ctx}" cube-node-init.sh
build_image cube-node-init "${ctx}"

ctx="$(prepare_context cube-pvm-host-bootstrap)"
copy_scripts "${ctx}" pvm-host-bootstrap.sh
if [[ "${INCLUDE_PVM_KERNEL_RPM}" == "1" ]]; then
  log "downloading PVM host kernel rpm for bootstrap image"
  download_file "${PVM_KERNEL_RPM_URL}" "${PVM_KERNEL_RPM}" file
  cp "${PVM_KERNEL_RPM}" "${ctx}/artifacts/kernel-pvm-host.rpm"
fi
if [[ "${INCLUDE_PVM_KERNEL_DEB}" == "1" ]]; then
  log "downloading PVM host kernel deb for bootstrap image"
  download_file "${PVM_KERNEL_DEB_URL}" "${PVM_KERNEL_DEB}" file
  cp "${PVM_KERNEL_DEB}" "${ctx}/artifacts/linux-image-pvm-host.deb"
fi
build_image cube-pvm-host-bootstrap "${ctx}"

cat <<EOF

Built CubeSandbox images:
  ${REGISTRY}/cube-master:${IMAGE_TAG}
  ${REGISTRY}/cube-api:${IMAGE_TAG}
  ${REGISTRY}/cubemastercli:${IMAGE_TAG}
  ${REGISTRY}/cube-proxy-node:${IMAGE_TAG}
  ${REGISTRY}/cube-egress:${IMAGE_TAG}
  ${REGISTRY}/cube-egress-net:${IMAGE_TAG}
  ${REGISTRY}/cube-webui:${IMAGE_TAG}
  ${REGISTRY}/cube-node:${IMAGE_TAG}
  ${REGISTRY}/cube-node-init:${IMAGE_TAG}
  ${REGISTRY}/cube-pvm-host-bootstrap:${IMAGE_TAG}

Use these values:
  images.*.repository: ${REGISTRY}/<image-name>
  images.*.tag: ${IMAGE_TAG}

Template builder is not built by this script. The chart uses a dind image by
default and can be overridden through images.templateBuilder.* when needed.
EOF
