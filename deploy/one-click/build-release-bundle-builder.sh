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

require_cmd docker
require_cmd make

rm -rf "${PREBUILT_DIR}"
mkdir -p "${PREBUILT_DIR}" "$(dirname "${HELPER_SCRIPT}")"

cat > "${HELPER_SCRIPT}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

PREBUILT_DIR="/workspace/deploy/one-click/.work/prebuilt"
mkdir -p "${PREBUILT_DIR}"
rm -f \
  "${PREBUILT_DIR}/cubemaster" \
  "${PREBUILT_DIR}/cubemastercli" \
  "${PREBUILT_DIR}/cubelet" \
  "${PREBUILT_DIR}/cubecli" \
  "${PREBUILT_DIR}/cube-api" \
  "${PREBUILT_DIR}/network-agent" \
  "${PREBUILT_DIR}/cube-agent" \
  "${PREBUILT_DIR}/containerd-shim-cube-rs" \
  "${PREBUILT_DIR}/cube-runtime"

echo "[one-click] building cubemaster in builder" >&2
(cd /workspace/CubeMaster && go mod download && go build -o "${PREBUILT_DIR}/cubemaster" ./cmd/cubemaster)

echo "[one-click] building cubemastercli in builder" >&2
(cd /workspace/CubeMaster && go build -o "${PREBUILT_DIR}/cubemastercli" ./cmd/cubemastercli)

echo "[one-click] building cubelet and cubecli in builder" >&2
mkdir -p /workspace/_output/bin
(cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk)
(cd /workspace/Cubelet && go mod download && make proto && go build -a -o /workspace/_output/bin/cubelet ./cmd/cubelet && go build -a -o /workspace/_output/bin/cubecli ./cmd/cubecli)
install -m 0755 /workspace/_output/bin/cubelet "${PREBUILT_DIR}/cubelet"
install -m 0755 /workspace/_output/bin/cubecli "${PREBUILT_DIR}/cubecli"

echo "[one-click] building cube-api in builder" >&2
(cd /workspace/CubeAPI && cargo build --release --locked)
install -m 0755 /workspace/CubeAPI/target/release/cube-api "${PREBUILT_DIR}/cube-api"

echo "[one-click] building network-agent in builder" >&2
(cd /workspace/network-agent && go build -o "${PREBUILT_DIR}/network-agent" ./cmd/network-agent)

echo "[one-click] building cube-agent in builder" >&2
(cd /workspace/agent && make -j1)
install -m 0755 /workspace/agent/target/x86_64-unknown-linux-musl/release/cube-agent "${PREBUILT_DIR}/cube-agent"

echo "[one-click] building shim workspace in builder" >&2
(cd /workspace/CubeShim && cargo build --release --locked)
install -m 0755 /workspace/CubeShim/target/release/containerd-shim-cube-rs "${PREBUILT_DIR}/containerd-shim-cube-rs"
install -m 0755 /workspace/CubeShim/target/release/cube-runtime "${PREBUILT_DIR}/cube-runtime"
EOF

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
ONE_CLICK_CUBE_AGENT_BIN="${PREBUILT_DIR}/cube-agent" \
ONE_CLICK_CUBESHIM_BIN="${PREBUILT_DIR}/containerd-shim-cube-rs" \
ONE_CLICK_CUBE_RUNTIME_BIN="${PREBUILT_DIR}/cube-runtime" \
  "${SCRIPT_DIR}/build-release-bundle.sh" "$@"
