#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

SYSTEMD_SCRIPT_DIR="${TOOLBOX_ROOT}/scripts/systemd"
CUBE_EGRESS_START_SCRIPT="${SYSTEMD_SCRIPT_DIR}/cube-egress-start.sh"

require_root
require_cmd bash
require_cmd docker
require_cmd curl

test -x "${SYSTEMD_SCRIPT_DIR}/cube-egress-prepare.sh" || die "cube-egress prepare script missing: ${SYSTEMD_SCRIPT_DIR}/cube-egress-prepare.sh"
test -x "${SYSTEMD_SCRIPT_DIR}/cube-egress-net-start.sh" || die "cube-egress network start script missing: ${SYSTEMD_SCRIPT_DIR}/cube-egress-net-start.sh"
test -x "${CUBE_EGRESS_START_SCRIPT}" || die "cube-egress start script missing: ${CUBE_EGRESS_START_SCRIPT}"
test -x "${SYSTEMD_SCRIPT_DIR}/cube-egress-postcheck.sh" || die "cube-egress postcheck script missing: ${SYSTEMD_SCRIPT_DIR}/cube-egress-postcheck.sh"

"${SYSTEMD_SCRIPT_DIR}/cube-egress-prepare.sh"
"${SYSTEMD_SCRIPT_DIR}/cube-egress-net-start.sh"

start_with_pidfile "cube-egress" "export ONE_CLICK_RUNTIME_ENV_FILE=\"${ENV_FILE}\"; \"${CUBE_EGRESS_START_SCRIPT}\""

"${SYSTEMD_SCRIPT_DIR}/cube-egress-postcheck.sh"
log "cube-egress ready"
