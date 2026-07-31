#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Build and sync the Web Terminal components, then run the real Firefox
# create -> terminal -> multi-tab -> close -> pause -> resume -> pause -> kill
# acceptance flow against the local CubeSandbox dev VM.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"
PLAYWRIGHT_FLOW="${SCRIPT_DIR}/internal/web_terminal_lifecycle.playwright.js"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"
VM_COMMAND_TIMEOUT_SECS="${VM_COMMAND_TIMEOUT_SECS:-120}"
WEB_UI_URL="${WEB_UI_URL:-http://127.0.0.1:12088}"

BUILD_AND_SYNC="${BUILD_AND_SYNC:-1}"
INSTALL_WEB_DEPS="${INSTALL_WEB_DEPS:-auto}"
E2E_USERNAME="${E2E_USERNAME:-admin}"
E2E_PASSWORD="${E2E_PASSWORD:-admin}"
E2E_TEMPLATE_ID="${E2E_TEMPLATE_ID:-}"
E2E_ACTION_TIMEOUT_MS="${E2E_ACTION_TIMEOUT_MS:-30000}"
E2E_LIFECYCLE_TIMEOUT_MS="${E2E_LIFECYCLE_TIMEOUT_MS:-120000}"
PLAYWRIGHT_CLI_PACKAGE="${PLAYWRIGHT_CLI_PACKAGE:-@playwright/cli@0.1.17}"
HEADED="${HEADED:-0}"
KEEP_ARTIFACTS="${KEEP_ARTIFACTS:-0}"
KEEP_ARTIFACTS_ON_FAILURE="${KEEP_ARTIFACTS_ON_FAILURE:-1}"

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$"
ARTIFACT_DIR="${REPO_ROOT}/output/playwright/web-terminal-lifecycle-${RUN_ID}"
PLAYWRIGHT_SESSION="cube-web-terminal-lifecycle-${RUN_ID}"
ASKPASS_SCRIPT=""
E2E_RESULT=1

log() {
  printf '[web-terminal-e2e] %s\n' "$*"
}

fail() {
  printf '[web-terminal-e2e] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<EOF
Usage: $(basename "$0")

Build/sync CubeOps and WebUI, then run this real Firefox acceptance flow:
  create -> terminal command -> two isolated tabs -> close terminal
  -> pause -> resume -> pause -> delete

The dev VM must already be running and reachable over SSH.

Common overrides:
  BUILD_AND_SYNC=0          Reuse the currently deployed CubeOps/WebUI.
  E2E_TEMPLATE_ID=ID        Select a specific READY, envd-enabled template.
  E2E_USERNAME/PASSWORD     WebUI credentials (default: admin/admin).
  WEB_UI_URL=URL            WebUI URL (default: ${WEB_UI_URL}).
  HEADED=1                  Show Firefox while the flow runs.
  KEEP_ARTIFACTS=1          Keep screenshots after a passing run.
  KEEP_ARTIFACTS_ON_FAILURE=0
                             Remove screenshots after a failed run.
  E2E_ACTION_TIMEOUT_MS     UI/terminal timeout (default: ${E2E_ACTION_TIMEOUT_MS}).
  E2E_LIFECYCLE_TIMEOUT_MS  Create/lifecycle timeout (default: ${E2E_LIFECYCLE_TIMEOUT_MS}).
  VM_COMMAND_TIMEOUT_SECS   SSH command timeout (default: ${VM_COMMAND_TIMEOUT_SECS}).
EOF
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "Missing required command: $1"
}

validate_bool() {
  case "$2" in
    0|1) ;;
    *) fail "$1 must be 0 or 1 (got $2)" ;;
  esac
}

validate_positive_integer() {
  case "$2" in
    ''|*[!0-9]*|0) fail "$1 must be a positive integer (got $2)" ;;
  esac
}

close_browser() {
  if [[ -d "${ARTIFACT_DIR}" ]]; then
    (
      cd "${ARTIFACT_DIR}"
      npx --yes --package "${PLAYWRIGHT_CLI_PACKAGE}" \
        playwright-cli -s="${PLAYWRIGHT_SESSION}" close >/dev/null 2>&1 || true
    )
  fi
}

cleanup() {
  close_browser
  if [[ -n "${ASKPASS_SCRIPT}" ]]; then
    rm -f "${ASKPASS_SCRIPT}"
  fi
}
trap cleanup EXIT

prepare_ssh() {
  mkdir -p "${WORK_DIR}"
  ASKPASS_SCRIPT="$(mktemp "${WORK_DIR}/web-terminal-e2e-askpass.XXXXXX")"
  cat >"${ASKPASS_SCRIPT}" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${CUBE_E2E_VM_PASSWORD:?}"
EOF
  chmod 700 "${ASKPASS_SCRIPT}"
}

remote_exec() {
  CUBE_E2E_VM_PASSWORD="${VM_PASSWORD}" \
    DISPLAY="${DISPLAY:-cubesandbox-dev-env}" \
    SSH_ASKPASS="${ASKPASS_SCRIPT}" \
    SSH_ASKPASS_REQUIRE=force \
    timeout --foreground "${VM_COMMAND_TIMEOUT_SECS}s" setsid -w ssh \
      -o ConnectTimeout=10 \
      -o ServerAliveInterval=5 \
      -o ServerAliveCountMax=3 \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o PreferredAuthentications=password \
      -o PubkeyAuthentication=no \
      -p "${SSH_PORT}" \
      "${VM_USER}@${SSH_HOST}" "$@"
}

playwright() {
  (
    cd "${ARTIFACT_DIR}"
    npx --yes --package "${PLAYWRIGHT_CLI_PACKAGE}" \
      playwright-cli -s="${PLAYWRIGHT_SESSION}" "$@"
  )
}

playwright_checked() {
  local output
  output="$(
    cd "${ARTIFACT_DIR}"
    npx --yes --package "${PLAYWRIGHT_CLI_PACKAGE}" \
      playwright-cli --json -s="${PLAYWRIGHT_SESSION}" "$@"
  )"
  printf '%s\n' "${output}"
  if ! printf '%s' "${output}" | node -e '
    let input = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => {
      input += chunk;
    });
    process.stdin.on("end", () => {
      const result = JSON.parse(input);
      if (result.isError) {
        console.error(`[web-terminal-e2e] Browser assertion failed: ${result.error}`);
        process.exit(1);
      }
    });
  '; then
    return 1
  fi
}

build_and_sync() {
  log "Running CubeOps tests"
  (
    cd "${REPO_ROOT}/CubeOps"
    go test ./...
  )

  log "Running Go SDK tests"
  (
    cd "${REPO_ROOT}/sdk/go"
    go test ./...
  )

  if [[ "${INSTALL_WEB_DEPS}" == "1" ]] || {
    [[ "${INSTALL_WEB_DEPS}" == "auto" ]] && [[ ! -d "${REPO_ROOT}/web/node_modules" ]]
  }; then
    log "Installing WebUI dependencies with npm ci"
    (
      cd "${REPO_ROOT}/web"
      npm ci
    )
  fi

  log "Linting, testing, and building WebUI"
  (
    cd "${REPO_ROOT}/web"
    npm run lint
    npm test
    npm run build
  )

  log "Building CubeOps"
  mkdir -p "${REPO_ROOT}/_output/bin"
  (
    cd "${REPO_ROOT}/CubeOps"
    CGO_ENABLED=0 go build -o "${REPO_ROOT}/_output/bin/cubeops" ./cmd/cubeops
  )

  log "Syncing and restarting CubeOps"
  timeout --foreground "${VM_COMMAND_TIMEOUT_SECS}s" env \
    REPO_ROOT="${REPO_ROOT}" \
    VM_USER="${VM_USER}" VM_PASSWORD="${VM_PASSWORD}" \
    SSH_HOST="${SSH_HOST}" SSH_PORT="${SSH_PORT}" \
    "${SCRIPT_DIR}/sync_to_vm.sh" bin cubeops
  remote_exec "
    set -e
    sudo systemctl restart cube-sandbox-cubeops.service
    sudo systemctl is-active --quiet cube-sandbox-cubeops.service
  "

  log "Syncing and restarting WebUI"
  timeout --foreground "${VM_COMMAND_TIMEOUT_SECS}s" env \
    REPO_ROOT="${REPO_ROOT}" WEB_SYNC_BUILD=0 \
    VM_USER="${VM_USER}" VM_PASSWORD="${VM_PASSWORD}" \
    SSH_HOST="${SSH_HOST}" SSH_PORT="${SSH_PORT}" \
    "${SCRIPT_DIR}/internal/sync_web_to_vm.sh"
}

verify_dev_vm() {
  log "Verifying CubeSandbox services"
  local attempt
  for attempt in $(seq 1 12); do
    if remote_exec "
      set -e
      for unit in \
        cube-sandbox-cubemaster.service \
        cube-sandbox-cube-api.service \
        cube-sandbox-cubelet.service \
        cube-sandbox-cubeops.service \
        cube-sandbox-webui.service
      do
        sudo systemctl is-active --quiet \"\${unit}\"
      done
      test \"\$(sudo docker inspect --format '{{.State.Health.Status}}' cube-webui)\" = healthy
    " && curl --max-time 10 -fsS "${WEB_UI_URL}/health" >/dev/null; then
      return
    fi
    sleep 5
  done
  fail "CubeSandbox services did not become active and healthy after 60 seconds"
}

run_browser_flow() {
  mkdir -p "${ARTIFACT_DIR}"

  local -a open_options=(--browser firefox)
  if [[ "${HEADED}" == "1" ]]; then
    open_options+=(--headed)
  fi

  log "Opening Firefox at ${WEB_UI_URL}"
  playwright_checked open "${WEB_UI_URL}" "${open_options[@]}"

  local config_json
  local config_literal
  config_json="$(
    node -e '
      const [baseURL, username, password, templateID, actionTimeoutMs, lifecycleTimeoutMs, artifactDir] =
        process.argv.slice(1);
      process.stdout.write(JSON.stringify({
        baseURL,
        username,
        password,
        templateID,
        actionTimeoutMs: Number(actionTimeoutMs),
        lifecycleTimeoutMs: Number(lifecycleTimeoutMs),
        artifactDir,
      }));
    ' \
      "${WEB_UI_URL}" \
      "${E2E_USERNAME}" \
      "${E2E_PASSWORD}" \
      "${E2E_TEMPLATE_ID}" \
      "${E2E_ACTION_TIMEOUT_MS}" \
      "${E2E_LIFECYCLE_TIMEOUT_MS}" \
      "${ARTIFACT_DIR}"
  )"
  config_literal="$(
    node -e 'process.stdout.write(JSON.stringify(process.argv[1]))' "${config_json}"
  )"

  playwright_checked run-code \
    "async (page) => page.evaluate((value) => sessionStorage.setItem('cube.e2e.webTerminalLifecycle', value), ${config_literal})"
  playwright_checked run-code --filename "${PLAYWRIGHT_FLOW}"
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" || "${1:-}" == "help" ]]; then
  usage
  exit 0
fi
[[ "$#" -eq 0 ]] || fail "Unexpected argument: $1"

for command in go node npm npx ssh setsid curl mktemp timeout; do
  need_cmd "${command}"
done
[[ -f "${PLAYWRIGHT_FLOW}" ]] || fail "Playwright flow not found: ${PLAYWRIGHT_FLOW}"

validate_bool BUILD_AND_SYNC "${BUILD_AND_SYNC}"
validate_bool HEADED "${HEADED}"
validate_bool KEEP_ARTIFACTS "${KEEP_ARTIFACTS}"
validate_bool KEEP_ARTIFACTS_ON_FAILURE "${KEEP_ARTIFACTS_ON_FAILURE}"
case "${INSTALL_WEB_DEPS}" in
  0|1|auto) ;;
  *) fail "INSTALL_WEB_DEPS must be 0, 1, or auto (got ${INSTALL_WEB_DEPS})" ;;
esac
validate_positive_integer E2E_ACTION_TIMEOUT_MS "${E2E_ACTION_TIMEOUT_MS}"
validate_positive_integer E2E_LIFECYCLE_TIMEOUT_MS "${E2E_LIFECYCLE_TIMEOUT_MS}"
validate_positive_integer VM_COMMAND_TIMEOUT_SECS "${VM_COMMAND_TIMEOUT_SECS}"

prepare_ssh
remote_exec "true" >/dev/null

if [[ "${BUILD_AND_SYNC}" == "1" ]]; then
  build_and_sync
else
  log "Skipping build and sync because BUILD_AND_SYNC=0"
fi
verify_dev_vm

set +e
run_browser_flow
E2E_RESULT=$?
set -e

if [[ "${E2E_RESULT}" -eq 0 ]]; then
  log "PASS: build/create/terminal/multi-tab/close/pause/resume/pause/delete"
  if [[ "${KEEP_ARTIFACTS}" == "0" ]]; then
    close_browser
    rm -rf "${ARTIFACT_DIR}"
  else
    log "Artifacts: ${ARTIFACT_DIR}"
  fi
else
  if [[ "${KEEP_ARTIFACTS_ON_FAILURE}" == "1" ]]; then
    log "FAIL: browser artifacts kept at ${ARTIFACT_DIR}"
  else
    rm -rf "${ARTIFACT_DIR}"
  fi
  exit "${E2E_RESULT}"
fi
