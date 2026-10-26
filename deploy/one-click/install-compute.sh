#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export ONE_CLICK_DEPLOY_ROLE=compute

exec "${SCRIPT_DIR}/install.sh" "$@"
