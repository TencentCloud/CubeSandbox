#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
API_TEMPLATE="${CHART_DIR}/templates/api.yaml"
OPS_TEMPLATE="${CHART_DIR}/templates/ops.yaml"

grep -Fq 'name: CUBE_OPS_URL' "${API_TEMPLATE}"
! grep -Fq 'CUBE_API_OPS_' "${API_TEMPLATE}"
! grep -Fq 'CUBE_API_WEBHOOK_CONFIG' "${API_TEMPLATE}"
! grep -Fq 'CUBE_WEBHOOK_SECRET_' "${API_TEMPLATE}"

grep -Fq 'name: CUBE_OPS_WEBHOOK_CONFIG' "${OPS_TEMPLATE}"
grep -Fq 'secretRef:' "${OPS_TEMPLATE}"
grep -Fq 'mountPath: /etc/cube/webhooks.toml' "${OPS_TEMPLATE}"
grep -Fq 'checksum/webhook-config:' "${OPS_TEMPLATE}"

echo "webhook ownership checks OK"
