#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

ENV_FILE="${ONE_CLICK_ENV_FILE:-${SCRIPT_DIR}/.env}"
if [[ -f "${ENV_FILE}" ]]; then
  load_env_file "${ENV_FILE}"
fi

require_root

INSTALL_PREFIX="${CUBE_SANDBOX_INSTALL_ROOT}"
ensure_dir "${INSTALL_PREFIX}"

ROLE_FILE="${INSTALL_PREFIX}/.one-click.env"
if [[ -f "${ROLE_FILE}" ]]; then
  load_env_file "${ROLE_FILE}"
fi
ROLE="$(one_click_deploy_role)"

require_cmd systemctl
log "stopping systemd deployment (role=${ROLE})"
if [[ "${ROLE}" == "compute" ]]; then
  systemctl stop cube-sandbox-compute.target
else
  systemctl stop cube-sandbox-control.target
fi

# CubeS3lvol (when enabled) is a Wants= member of the role target, so the
# stop above pulls cube-sandbox-s3lvol.service down through its ExecStop
# (cube-s3lvol-stop.sh: full teardown if the target is alive, target-side
# cleanup only if it has crashed -- never disconnecting the NVMf initiator).
# down.sh intentionally does NOT delete the s3lvol per-node state
# (/data/cubelet/rcow/wal_bdev.img, lvstore/bstore metadata): the next
# install/start attaches and replays it.
