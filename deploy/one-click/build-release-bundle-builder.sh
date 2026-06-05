#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
ENV_FILE="${ONE_CLICK_ENV_FILE:-${SCRIPT_DIR}/.env}"
if [[ -f "${ENV_FILE}" ]]; then
  load_env_file "${ENV_FILE}"
fi

PREBUILT_DIR="${SCRIPT_DIR}/.work/prebuilt"
HELPER_SCRIPT="${SCRIPT_DIR}/.work/build-prebuilt-in-builder.sh"
BUILDER_IMAGE_REF="${BUILDER_IMAGE:-cube-sandbox-builder:ubuntu2004}"

CUBE_VERSION_FROM_ENV="${CUBE_VERSION:-}"
LATEST_RELEASE_TAG="$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
: "${CUBE_VERSION:=${LATEST_RELEASE_TAG:-0.0.0-dev}}"
: "${CUBE_COMMIT:=$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')}"
: "${CUBE_BUILD_TIME:=$(date -u +'%Y-%m-%dT%H:%M:%SZ')}"
: "${ONE_CLICK_DIST_VERSION:=${CUBE_VERSION_FROM_ENV:-${LATEST_RELEASE_TAG:-$(latest_git_revision "${ROOT_DIR}")}}}"
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME ONE_CLICK_DIST_VERSION

require_cmd docker
require_cmd make

rm -rf "${PREBUILT_DIR}"
mkdir -p "${PREBUILT_DIR}" "$(dirname "${HELPER_SCRIPT}")"

cat > "${HELPER_SCRIPT}" <<'SCRIPT_EOF'
#!/usr/bin/env bash
set -euo pipefail

# Version values are resolved by the host script and passed into this helper.

go_version_ldflags() {
  local version_pkg="$1"
  printf -- "-s -w -X '%s.Version=%s' -X '%s.Commit=%s' -X '%s.BuildTime=%s'" \
    "${version_pkg}" "${CUBE_VERSION}" \
    "${version_pkg}" "${CUBE_COMMIT}" \
    "${version_pkg}" "${CUBE_BUILD_TIME}"
}

CUBEMASTER_VERSION_PKG="github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
CUBELET_VERSION_PKG="github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
NETAGENT_VERSION_PKG="github.com/tencentcloud/CubeSandbox/network-agent/pkg/version"

CUBEMASTER_LDFLAGS="$(go_version_ldflags "${CUBEMASTER_VERSION_PKG}")"
CUBELET_LDFLAGS="$(go_version_ldflags "${CUBELET_VERSION_PKG}")"
NETAGENT_LDFLAGS="$(go_version_ldflags "${NETAGENT_VERSION_PKG}")"

PREBUILT_DIR="/workspace/deploy/one-click/.work/prebuilt"
mkdir -p "${PREBUILT_DIR}"
rm -f \
  "${PREBUILT_DIR}/cubemaster" \
  "${PREBUILT_DIR}/cubemastercli" \
  "${PREBUILT_DIR}/cubelet" \
  "${PREBUILT_DIR}/cubecli" \
  "${PREBUILT_DIR}/cube-api" \
  "${PREBUILT_DIR}/network-agent" \
  "${PREBUILT_DIR}/cubevsmapdump" \
  "${PREBUILT_DIR}/cube-agent" \
  "${PREBUILT_DIR}/containerd-shim-cube-rs" \
  "${PREBUILT_DIR}/cube-runtime"

echo "[one-click] building cubemaster in builder" >&2
(cd /workspace/CubeMaster && go mod download && go build -ldflags "${CUBEMASTER_LDFLAGS}" -o "${PREBUILT_DIR}/cubemaster" ./cmd/cubemaster)

echo "[one-click] building cubemastercli in builder" >&2
(cd /workspace/CubeMaster && go build -ldflags "${CUBEMASTER_LDFLAGS}" -o "${PREBUILT_DIR}/cubemastercli" ./cmd/cubemastercli)

echo "[one-click] building cubelet and cubecli in builder" >&2
mkdir -p /workspace/_output/bin
(cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk)
(cd /workspace/Cubelet && go mod download && make proto && go build -ldflags "${CUBELET_LDFLAGS}" -o /workspace/_output/bin/cubelet ./cmd/cubelet && go build -ldflags "${CUBELET_LDFLAGS}" -o /workspace/_output/bin/cubecli ./cmd/cubecli)
install -m 0755 /workspace/_output/bin/cubelet "${PREBUILT_DIR}/cubelet"
install -m 0755 /workspace/_output/bin/cubecli "${PREBUILT_DIR}/cubecli"

echo "[one-click] building cube-api in builder" >&2
(cd /workspace/CubeAPI && cargo build --release --locked)
install -m 0755 /workspace/CubeAPI/target/release/cube-api "${PREBUILT_DIR}/cube-api"

echo "[one-click] building network-agent in builder" >&2
(cd /workspace/CubeNet && make -C cubevs gen && cd /workspace/network-agent && go build -ldflags "${NETAGENT_LDFLAGS}" -o "${PREBUILT_DIR}/network-agent" ./cmd/network-agent)

echo "[one-click] building cubevsmapdump in builder" >&2
(cd /workspace/CubeNet/cubevs && make gen && go build -o "${PREBUILT_DIR}/cubevsmapdump" ./cmd/cubevsmapdump)

echo "[one-click] building cube-agent in builder" >&2
# Agent Makefile reads CUBE_VERSION/CUBE_COMMIT/CUBE_BUILD_TIME directly.
(cd /workspace/agent && make -j1)
make -C /workspace/agent BINDIR=${PREBUILT_DIR} install

echo "[one-click] building shim workspace in builder" >&2
# CUBE_VERSION/COMMIT/BUILD_TIME picked up by shim/build.rs and cube-runtime/build.rs
(cd /workspace/CubeShim && cargo build --release --locked)
install -m 0755 /workspace/CubeShim/target/release/containerd-shim-cube-rs "${PREBUILT_DIR}/containerd-shim-cube-rs"
install -m 0755 /workspace/CubeShim/target/release/cube-runtime "${PREBUILT_DIR}/cube-runtime"
SCRIPT_EOF

chmod 0755 "${HELPER_SCRIPT}"

if ! docker image inspect "${BUILDER_IMAGE_REF}" >/dev/null 2>&1; then
  log "builder image ${BUILDER_IMAGE_REF} missing, building it first"
  make -C "${ROOT_DIR}" builder-image BUILDER_IMAGE="${BUILDER_IMAGE_REF}" >&2
fi

log "building one-click component binaries in builder"
make -C "${ROOT_DIR}" builder-run \
  BUILDER_IMAGE="${BUILDER_IMAGE_REF}" \
  BUILDER_CMD="bash /workspace/deploy/one-click/.work/build-prebuilt-in-builder.sh" >&2

for artifact in \
  cubemaster \
  cubemastercli \
  cubelet \
  cubecli \
  cube-api \
  network-agent \
  cubevsmapdump \
  cube-agent \
  containerd-shim-cube-rs \
  cube-runtime
do
  ensure_file "${PREBUILT_DIR}/${artifact}"
done

log "packaging one-click release bundle on host with prebuilt artifacts"
ONE_CLICK_CUBEMASTER_BIN="${PREBUILT_DIR}/cubemaster" \
ONE_CLICK_CUBEMASTERCLI_BIN="${PREBUILT_DIR}/cubemastercli" \
ONE_CLICK_CUBELET_BIN="${PREBUILT_DIR}/cubelet" \
ONE_CLICK_CUBECLI_BIN="${PREBUILT_DIR}/cubecli" \
ONE_CLICK_CUBE_API_BIN="${PREBUILT_DIR}/cube-api" \
ONE_CLICK_NETWORK_AGENT_BIN="${PREBUILT_DIR}/network-agent" \
ONE_CLICK_CUBEVSMAPDUMP_BIN="${PREBUILT_DIR}/cubevsmapdump" \
ONE_CLICK_CUBE_AGENT_BIN="${PREBUILT_DIR}/cube-agent" \
ONE_CLICK_CUBESHIM_BIN="${PREBUILT_DIR}/containerd-shim-cube-rs" \
ONE_CLICK_CUBE_RUNTIME_BIN="${PREBUILT_DIR}/cube-runtime" \
  "${SCRIPT_DIR}/build-release-bundle.sh" "$@"
