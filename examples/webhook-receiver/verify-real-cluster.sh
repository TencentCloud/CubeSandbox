#!/usr/bin/env bash
# Verifies signed CubeOps lifecycle Webhooks from a CubeSandbox control node.
# It expects the cluster services to be running before verification starts.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
RUN_DIR="${WEBHOOK_REAL_LOG_DIR:-$(mktemp -d /tmp/cubesandbox-webhook-real.XXXXXX)}"
DROPIN=/etc/systemd/system/cube-sandbox-cubeops.service.d/webhook-e2e.conf
CUBEMASTER_DROPIN=/etc/systemd/system/cube-sandbox-cubemaster.service.d/template-pull-proxy.conf
WEBHOOK_CONFIG_FILE="/run/cube-sandbox-systemd/webhook-e2e-$$.yaml"
WEBHOOK_GROUP="cubeops-webhook-e2e-$$"
ONE_CLICK_ENV=/usr/local/services/cubetoolbox/.one-click.env
CMCLI=/usr/local/services/cubetoolbox/CubeMaster/bin/cubemastercli
CUBE_OPS_BIN=/usr/local/services/cubetoolbox/CubeOps/bin/cubeops
CUBEMASTER_ADDRESS="${CUBEMASTER_ADDRESS:-127.0.0.1}"
CUBEMASTER_PORT="${CUBEMASTER_PORT:-8089}"
CUBEMASTER_SERVICE="${CUBEMASTER_SERVICE:-cube-sandbox-cubemaster.service}"
CUBE_API_URL="${CUBE_API_URL:-http://127.0.0.1:3000}"
CUBE_API_SERVICE="${CUBE_API_SERVICE:-cube-sandbox-cube-api.service}"
CUBE_OPS_URL="${CUBE_OPS_URL:-http://127.0.0.1:3010}"
CUBE_OPS_SERVICE="${CUBE_OPS_SERVICE:-cube-sandbox-cubeops.service}"
RECEIVER_HOST="${RECEIVER_HOST:-127.0.0.1}"
RECEIVER_PORT="${RECEIVER_PORT:-9000}"
WEBHOOK_ENDPOINT_URL="${WEBHOOK_ENDPOINT_URL:-http://${RECEIVER_HOST}:${RECEIVER_PORT}/webhook}"
INSTALL_CUBE_OPS="${INSTALL_CUBE_OPS:-1}"
TEMPLATE_IMAGE="${TEMPLATE_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest}"
# Set this only when the registry is reachable through a host proxy, for example
# CUBEMASTER_PROXY=http://host.docker.internal:7897 in a configured WSL setup.
CUBEMASTER_PROXY="${CUBEMASTER_PROXY:-}"
SECRET=real-cluster-webhook-secret
DROPIN_INSTALLED=0
CUBEMASTER_PROXY_DROPIN_INSTALLED=0
CUBE_OPS_BINARY_REPLACED=0
RECEIVER_PID=
SANDBOX_ID=
START_TIME=
WEBHOOK_REDIS_URL="${REDIS_URL:-}"

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "missing required command: $1" >&2
        exit 1
    }
}

wait_for_active() {
    local unit="$1" attempts="${2:-90}"

    for _ in $(seq 1 "${attempts}"); do
        if sudo systemctl is-active --quiet "${unit}"; then
            return 0
        fi
        if sudo systemctl is-failed --quiet "${unit}"; then
            break
        fi
        sleep 2
    done

    echo "service did not become active: ${unit}" >&2
    sudo systemctl status --no-pager "${unit}" >&2 || true
    sudo journalctl -u "${unit}" -n 80 --no-pager >&2 || true
    return 1
}

wait_for_http() {
    local url="$1" attempts="${2:-60}"

    for _ in $(seq 1 "${attempts}"); do
        if curl --noproxy '*' -fsS --max-time 3 "${url}" >/dev/null; then
            return 0
        fi
        sleep 2
    done
    echo "HTTP health check did not become ready: ${url}" >&2
    return 1
}

wait_for_healthy_node() {
    local output

    for _ in $(seq 1 45); do
        output="$("${CMCLI}" --address "${CUBEMASTER_ADDRESS}" --port "${CUBEMASTER_PORT}" node list 2>&1 || true)"
        # Zone and CPU type may be blank, so HEALTHY is the penultimate field.
        if printf '%s\n' "${output}" | awk '$(NF - 1) == "true" && $NF == "RUNNING" { found = 1 } END { exit found ? 0 : 1 }'; then
            return 0
        fi
        sleep 2
    done

    echo 'CubeMaster has no healthy running node' >&2
    printf '%s\n' "${output}" >&2
    return 1
}

configure_webhook() {
    if [[ -z "${WEBHOOK_REDIS_URL}" ]]; then
        # Match the Redis connection CubeOps receives from the one-click
        # systemd EnvironmentFile instead of assuming the local default password.
        WEBHOOK_REDIS_URL="$(sudo bash -c '
            set -a
            source "$1"
            if [[ -n "${REDIS_URL:-}" ]]; then
                printf "%s" "${REDIS_URL}"
                exit 0
            fi
            host="${CUBE_EXTERNAL_REDIS_HOST:-${CUBE_SANDBOX_REDIS_HOST:-127.0.0.1}}"
            port="${CUBE_EXTERNAL_REDIS_PORT:-${CUBE_SANDBOX_REDIS_PORT:-6379}}"
            password="${CUBE_EXTERNAL_REDIS_PASSWORD:-${CUBE_SANDBOX_REDIS_PASSWORD:-}}"
            auth=""
            if [[ -n "${password}" ]]; then auth=":${password}@"; fi
            printf "redis://%s%s:%s/0" "${auth}" "${host}" "${port}"
        ' bash "${ONE_CLICK_ENV}")"
        [[ -n "${WEBHOOK_REDIS_URL}" ]] || {
            echo "could not resolve Redis URL from ${ONE_CLICK_ENV}" >&2
            return 1
        }
    fi

    sudo install -Dm600 /dev/null "${WEBHOOK_CONFIG_FILE}"
    sudo tee "${WEBHOOK_CONFIG_FILE}" >/dev/null <<EOF
redis_url: "${WEBHOOK_REDIS_URL}"
webhook:
  enabled: true
  consumer_group: "${WEBHOOK_GROUP}"
  workers: 4
  default_timeout: "3s"
  default_max_retries: 3
  initial_backoff: "200ms"
  max_backoff: "2s"
  endpoints:
    - name: real-cluster-proof
      url: "${WEBHOOK_ENDPOINT_URL}"
      events:
        - sandbox.created
        - sandbox.paused
        - sandbox.resumed
        - sandbox.deleted
      secret: "${SECRET}"
EOF

    sudo mkdir -p "$(dirname "${DROPIN}")"
    sudo tee "${DROPIN}" >/dev/null <<EOF
[Service]
Environment=CUBE_OPS_CONFIG=${WEBHOOK_CONFIG_FILE}
EOF
    DROPIN_INSTALLED=1
    sudo systemctl daemon-reload
    sudo systemctl restart "${CUBE_OPS_SERVICE}"
    wait_for_http "${CUBE_OPS_URL}/health"
}

cleanup() {
    set +e

    # A failure after creation must not leave a sandbox consuming the test node.
    if [[ -n "${SANDBOX_ID}" ]]; then
        curl --noproxy '*' -fsS -X DELETE "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}" >/dev/null
    fi
    if [[ -n "${RECEIVER_PID}" ]]; then
        kill "${RECEIVER_PID}" 2>/dev/null
        wait "${RECEIVER_PID}" 2>/dev/null
    fi
    # Remove only the temporary overrides created by this verifier.
    if [[ "${DROPIN_INSTALLED}" -eq 1 ]]; then
        sudo rm -f "${DROPIN}"
    fi
    if [[ "${CUBE_OPS_BINARY_REPLACED}" -eq 1 ]]; then
        sudo install -m755 "${RUN_DIR}/cubeops.previous" "${CUBE_OPS_BIN}"
    fi
    if [[ "${DROPIN_INSTALLED}" -eq 1 || "${CUBE_OPS_BINARY_REPLACED}" -eq 1 ]]; then
        sudo systemctl daemon-reload
        sudo systemctl restart "${CUBE_OPS_SERVICE}"
    fi
    if [[ -n "${WEBHOOK_REDIS_URL}" ]]; then
        redis-cli -u "${WEBHOOK_REDIS_URL}" XGROUP DESTROY \
            cube:v1:shared:sandbox:lifecycle:events "${WEBHOOK_GROUP}" >/dev/null 2>&1 || true
    fi
    sudo rm -f "${WEBHOOK_CONFIG_FILE}"
    if [[ "${CUBEMASTER_PROXY_DROPIN_INSTALLED}" -eq 1 ]]; then
        sudo rm -f "${CUBEMASTER_DROPIN}"
        sudo systemctl daemon-reload
        sudo systemctl restart "${CUBEMASTER_SERVICE}"
    fi
}
trap cleanup EXIT

wait_for_event_count() {
    local expected_count="$1" attempts="${2:-90}"

    for _ in $(seq 1 "${attempts}"); do
        grep -oE '"event": "sandbox\.[^"]+"' "${RUN_DIR}/receiver.log" \
            > "${RUN_DIR}/events.txt" || true
        if [[ "$(wc -l < "${RUN_DIR}/events.txt")" -ge "${expected_count}" ]]; then
            return 0
        fi
        sleep 1
    done

    echo "timed out waiting for ${expected_count} webhook events" >&2
    cat "${RUN_DIR}/receiver.log" >&2 || true
    return 1
}

extract_json_field() {
    python3 - "$1" "$2" <<'PY'
import json
import sys

target = sys.argv[2].replace("_", "").lower()

def find(value):
    if isinstance(value, dict):
        for key, item in value.items():
            if key.replace("_", "").lower() == target and isinstance(item, (str, int)):
                return str(item)
        for item in value.values():
            result = find(item)
            if result:
                return result
    elif isinstance(value, list):
        for item in value:
            result = find(item)
            if result:
                return result
    return ""

with open(sys.argv[1], encoding="utf-8") as source:
    print(find(json.load(source)))
PY
}

require_command curl
require_command go
require_command python3
require_command redis-cli
require_command sudo
require_command systemctl
require_command timeout
[[ -x "${CMCLI}" ]] || {
    echo "cubemastercli was not found: ${CMCLI}" >&2
    exit 1
}
[[ -f "${ONE_CLICK_ENV}" ]] || {
    echo "CubeSandbox runtime environment is missing: ${ONE_CLICK_ENV}" >&2
    exit 1
}

mkdir -p "${RUN_DIR}"

if [[ "${INSTALL_CUBE_OPS}" == "1" ]]; then
    echo '[1/8] Build and temporarily install the current CubeOps binary'
    (cd "${ROOT_DIR}/CubeOps" && go build -o "${RUN_DIR}/cubeops" ./cmd/cubeops)
    sudo install -Dm755 "${CUBE_OPS_BIN}" "${RUN_DIR}/cubeops.previous"
    CUBE_OPS_BINARY_REPLACED=1
    sudo systemctl stop "${CUBE_OPS_SERVICE}"
    sudo install -Dm755 "${RUN_DIR}/cubeops" "${CUBE_OPS_BIN}"
else
    echo '[1/8] Use the currently installed CubeOps binary'
fi

echo '[2/8] Verify the control plane and at least one healthy compute node'
wait_for_active "${CUBEMASTER_SERVICE}"
wait_for_active "${CUBE_API_SERVICE}"
if [[ "${INSTALL_CUBE_OPS}" != "1" ]]; then
    wait_for_active "${CUBE_OPS_SERVICE}"
fi
wait_for_http "${CUBE_API_URL}/health"
wait_for_healthy_node

if [[ -n "${CUBEMASTER_PROXY}" ]]; then
    echo '[3/8] Configure the optional CubeMaster image-pull proxy'
    sudo mkdir -p "$(dirname "${CUBEMASTER_DROPIN}")"
    sudo tee "${CUBEMASTER_DROPIN}" >/dev/null <<EOF
[Service]
Environment=HTTP_PROXY=${CUBEMASTER_PROXY}
Environment=HTTPS_PROXY=${CUBEMASTER_PROXY}
Environment=ALL_PROXY=${CUBEMASTER_PROXY}
Environment=NO_PROXY=127.0.0.1,localhost
Environment=no_proxy=127.0.0.1,localhost
Environment=CUBEMASTER_NATIVE_ROOTFS_EXPORT_JOBS=1
EOF
    CUBEMASTER_PROXY_DROPIN_INSTALLED=1
    sudo systemctl daemon-reload
    sudo systemctl restart "${CUBEMASTER_SERVICE}"
    wait_for_active "${CUBEMASTER_SERVICE}"
else
    echo '[3/8] No CubeMaster image-pull proxy configured'
fi

echo '[4/8] Create a real template from the sandbox-code image'
"${CMCLI}" --address "${CUBEMASTER_ADDRESS}" --port "${CUBEMASTER_PORT}" tpl create-from-image \
    --detach --json --image "${TEMPLATE_IMAGE}" --writable-layer-size 1G \
    --expose-port 49999 --expose-port 49983 --probe 49999 --probe-path /health \
    --with-cube-ca=false > "${RUN_DIR}/template-create.json"
JOB_ID="$(extract_json_field "${RUN_DIR}/template-create.json" jobid)"
TEMPLATE_ID="$(extract_json_field "${RUN_DIR}/template-create.json" templateid)"
[[ -n "${JOB_ID}" && -n "${TEMPLATE_ID}" ]] || {
    echo 'could not extract job_id or template_id from CubeMaster response' >&2
    exit 1
}
printf 'job_id=%s\ntemplate_id=%s\nimage=%s\n' "${JOB_ID}" "${TEMPLATE_ID}" "${TEMPLATE_IMAGE}" | tee "${RUN_DIR}/template.txt"

echo '[5/8] Wait for the real template build'
timeout 45m "${CMCLI}" --address "${CUBEMASTER_ADDRESS}" --port "${CUBEMASTER_PORT}" tpl watch --job-id "${JOB_ID}" | tee "${RUN_DIR}/template-watch.txt"

echo '[6/8] Start the signed local webhook receiver'
python3 "${SCRIPT_DIR}/receiver.py" --host "${RECEIVER_HOST}" --port "${RECEIVER_PORT}" --secret "${SECRET}" > "${RUN_DIR}/receiver.log" 2>&1 &
RECEIVER_PID=$!
sleep 1
kill -0 "${RECEIVER_PID}"
configure_webhook

echo '[7/8] Run real lifecycle: create -> pause -> connect(resume) -> delete'
START_TIME="$(date --iso-8601=seconds)"
CREATE_RESPONSE="$(curl --noproxy '*' -fsS -H 'Content-Type: application/json' -d "{\"templateID\":\"${TEMPLATE_ID}\",\"timeout\":300}" "${CUBE_API_URL}/sandboxes")"
printf '%s\n' "${CREATE_RESPONSE}" | tee "${RUN_DIR}/create-response.json"
SANDBOX_ID="$(python3 -c 'import json, sys; print(json.load(sys.stdin)["sandboxID"])' <<< "${CREATE_RESPONSE}")"
printf 'sandbox_id=%s\n' "${SANDBOX_ID}" | tee "${RUN_DIR}/sandbox.txt"

wait_for_event_count 1
curl --noproxy '*' -fsS -X POST "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}/pause" >/dev/null
wait_for_event_count 2
curl --noproxy '*' -fsS -X POST -H 'Content-Type: application/json' -d '{"timeout":300}' "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}/connect" | tee "${RUN_DIR}/connect-response.json"
wait_for_event_count 3
curl --noproxy '*' -fsS -X DELETE "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}" >/dev/null
SANDBOX_ID=

echo '[8/8] Verify signed webhook deliveries and collect evidence'
wait_for_event_count 4
sudo journalctl -u "${CUBE_OPS_SERVICE}" -u "${CUBEMASTER_SERVICE}" --since "${START_TIME}" --no-pager > "${RUN_DIR}/cluster.log"

python3 - "${RUN_DIR}/events.txt" <<'PY'
import sys

expected = [
    '"event": "sandbox.created"',
    '"event": "sandbox.paused"',
    '"event": "sandbox.resumed"',
    '"event": "sandbox.deleted"',
]
actual = [line.strip() for line in open(sys.argv[1], encoding="utf-8") if line.strip()]
if actual != expected:
    raise SystemExit(f"Webhook events mismatch:\nexpected={expected}\nactual={actual}")
print("Webhook lifecycle verification: PASS")
PY

tar -C "$(dirname "${RUN_DIR}")" -czf /tmp/cube-webhook-real.tar.gz "$(basename "${RUN_DIR}")"
echo "PASS: real-cluster webhook verification completed"
echo "Evidence archive: /tmp/cube-webhook-real.tar.gz"
echo "Receiver payloads: ${RUN_DIR}/receiver.log"
cat "${RUN_DIR}/events.txt"
