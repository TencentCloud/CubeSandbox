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

ensure_file "${INSTALL_PREFIX}/scripts/one-click/quickcheck.sh"
"${INSTALL_PREFIX}/scripts/one-click/quickcheck.sh"
