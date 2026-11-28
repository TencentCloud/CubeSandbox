#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

src="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
cd "${tmp}"
mkdir install

# Use the installer's actual upgrade merge and persistence paths with a
# temporary install prefix. Host setup and service replacement are omitted.
sed -n '/^  normalize_s3_addressing_env "\${RUNTIME_ENV_OLD}" /,/^  apply_cli_overrides$/p' "${src}/install.sh" > merge.sh
sed -n '/^RUNTIME_ENV_FILE=/,/^persist_s3_addressing_styles /p' "${src}/install.sh" > persist.sh
grep -q '^  merge_env_three_way ' merge.sh
grep -q '^chmod 600 ' persist.sh
[[ "$(wc -l < merge.sh)" -lt 25 && "$(wc -l < persist.sh)" -lt 25 ]]

install_env() {
  local mode="$1"
  shift
  env -i PATH="${PATH}" "$@" bash -euo pipefail -c '
    SCRIPT_DIR="$1" INSTALL_MODE="$2" INSTALL_PREFIX="${PWD}/install"
    source "${SCRIPT_DIR}/lib/common.sh"
    ENV_FILE="${PWD}/new.env" WORK_DIR="${PWD}" MERGED_ENV=""
    snapshot_one_click_toggles "${ENV_FILE}"
    if [[ -f "${ENV_FILE}" ]]; then
      load_env_file "${ENV_FILE}"
      capture_one_click_toggle_dotenv_values
    fi
    if [[ "${INSTALL_MODE}" == upgrade ]]; then
      RUNTIME_ENV_OLD="${INSTALL_PREFIX}/.one-click.env"
      MERGED_ENV="${PWD}/merged.env" ENV_DIFF_FILE="${PWD}/diff"
      MERGE_OLD_BASELINE="${SCRIPT_DIR}/env.example"
      MERGE_NEW_DOTENV=""
      [[ ! -f "${ENV_FILE}" ]] || MERGE_NEW_DOTENV="${ENV_FILE}"
      apply_cli_overrides() { :; }
      source ./merge.sh
    fi
    apply_one_click_toggles
    validate_s3_addressing_styles
    source ./persist.sh
  ' _ "${src}" "${mode}"
}

runtime=install/.one-click.env
check_styles() {
  local actual
  actual="$(env -i PATH="${PATH}" bash -c '
    source "$1"
    printf "%s|%s" "${CUBE_S3_ADDRESSING_STYLE-unset}" "${CUBE_OPS_S3_ADDRESSING_STYLE-unset}"
  ' _ "${runtime}")"
  if [[ "${actual}" != "$1" ]]; then
    printf 'Expected %s, got %s\n' "$1" "${actual}" >&2
    return 1
  fi
}

# Fresh installation keeps the expression and does not append a shadow copy.
cat > new.env <<'ENV'
MY_STYLE=virtual
export CUBE_S3_ADDRESSING_STYLE="$MY_STYLE"
CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test
CUBE_S3_ARTIFACT_PREFIX=custom-templates
CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES=1048576
ENV
install_env install CUBE_S3_ADDRESSING_STYLE=path CUBE_OPS_S3_ADDRESSING_STYLE=auto
check_styles 'virtual|auto'
[[ "$(grep -c '^CUBE_S3_ADDRESSING_STYLE=' "${runtime}")" == 1 ]]
[[ "$(stat -c %a "${runtime}")" == 600 ]]
sed -i 's/^MY_STYLE=virtual$/MY_STYLE=path/' "${runtime}"
check_styles 'path|auto'

rm new.env
for _ in first second; do
  install_env upgrade
  check_styles 'path|auto'
  grep -Fxq 'CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test' "${runtime}"
  grep -Fxq 'CUBE_S3_ARTIFACT_PREFIX=custom-templates' "${runtime}"
  grep -Fxq 'CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES=1048576' "${runtime}"
done

printf '%s\n' 'CUBE_S3_ADDRESSING_STYLE=virtual' 'export CUBE_OPS_S3_ADDRESSING_STYLE=' > new.env
install_env upgrade CUBE_S3_ADDRESSING_STYLE=path CUBE_OPS_S3_ADDRESSING_STYLE=path
check_styles 'virtual|'
grep -Fxq 'CUBE_OPS_S3_ADDRESSING_STYLE=' "${runtime}"
rm new.env
install_env upgrade CUBE_S3_ADDRESSING_STYLE=auto
check_styles 'auto|'
install_env upgrade
check_styles 'auto|'

cp "${runtime}" before.env
if install_env upgrade CUBE_S3_ADDRESSING_STYLE=vitual > invalid.log 2>&1; then
  echo 'Expected invalid S3 addressing style to be rejected' >&2
  exit 1
fi
grep -Fq 'invalid CUBE_S3_ADDRESSING_STYLE.' invalid.log
cmp before.env "${runtime}"
install_env install CUBE_S3_ADDRESSING_STYLE=virtual
check_styles 'virtual|unset'

# Run the real CubeOps launcher using a stub binary and no root/systemd setup.
mkdir -p install/CubeOps/bin wrappers
touch install/CubeOps/conf.yaml
cp "${src}/scripts/systemd/cubeops-start.sh" wrappers/
cat > wrappers/common.sh <<'SH'
source "${TEST_SRC}/scripts/systemd/common.sh"
TOOLBOX_ROOT="${PWD}/install"
require_root() { :; }
ensure_systemd_runtime_dirs() { :; }
SH
cat > install/CubeOps/bin/cubeops <<'SH'
#!/usr/bin/env bash
[[ "${CUBE_OPS_S3_ADDRESSING_STYLE}" == virtual ]]
SH
chmod +x install/CubeOps/bin/cubeops
printf '%s\n' 'CUBE_OPS_S3_ADDRESSING_STYLE="   "' 'CUBE_OPS_WAREHOUSE_WORK_DIR=./scratch' >> "${runtime}"
env -i PATH="${PATH}" TEST_SRC="${src}" ONE_CLICK_RUNTIME_ENV_FILE="${PWD}/${runtime}" \
  CUBE_OPS_STORE_BACKEND='   ' CUBE_OPS_S3_PATH_STYLE=true CUBE_OPS_LOG_DIR="${PWD}/scratch" \
  bash wrappers/cubeops-start.sh 2> warning.log
grep -Fq 'virtual overrides CUBE_OPS_S3_PATH_STYLE' warning.log

# Check the same fill and validation calls used by the installer, in order.
sed -n '/^fill_s3_from_local_minio() {$/,/^}$/p' "${src}/install.sh" > minio.sh
awk '
  /^fill_s3_from_local_minio$/ { filled = NR }
  /^validate_bundled_minio_addressing$/ { checked = NR }
  /^stop_existing_systemd_deployment$/ { stopped = NR }
  END { exit !(filled && checked == filled + 1 && checked < stopped) }
' "${src}/install.sh"

run_minio_preflight() {
  env -i PATH="${PATH}" DEPLOY_ROLE=control \
    CUBE_SANDBOX_MINIO_ENABLED=1 CUBE_SANDBOX_NODE_IP=192.0.2.10 \
    CUBE_SANDBOX_MINIO_ROOT_USER=test CUBE_SANDBOX_MINIO_ROOT_PASSWORD=fixture \
    CUBE_ARTIFACT_STORE_BACKEND=s3 CUBE_OPS_STORE_BACKEND=s3 "$@" \
    bash -euo pipefail -c '
      source "$1/lib/common.sh"
      source ./minio.sh
      fill_s3_from_local_minio
      validate_bundled_minio_addressing
    ' _ "${src}"
}

reject_minio_virtual() {
  if run_minio_preflight "$@" > minio.log 2>&1; then
    echo 'Expected bundled MinIO virtual addressing to fail preflight' >&2
    exit 1
  fi
  grep -Fq 'bundled MinIO uses path style' minio.log
}
reject_minio_virtual CUBE_S3_ADDRESSING_STYLE=' Virtual '
reject_minio_virtual CUBE_OPS_S3_ADDRESSING_STYLE=virtual
reject_minio_virtual CUBE_OPS_STORE_BACKEND='   ' CUBE_OPS_S3_ADDRESSING_STYLE=virtual
reject_minio_virtual CUBE_ARTIFACT_STORE_BACKEND=fs CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ADDRESSING_STYLE='   '
reject_minio_virtual CUBE_OPS_S3_ADDRESSING_STYLE=virtual \
  CUBE_OPS_S3_ENDPOINT=https://storage.example.test CUBE_OPS_S3_NODE_ENDPOINT=http://192.0.2.10:9000/
run_minio_preflight CUBE_S3_ADDRESSING_STYLE=auto CUBE_OPS_S3_ADDRESSING_STYLE=path
run_minio_preflight CUBE_SANDBOX_MINIO_ENABLED=0 CUBE_S3_ADDRESSING_STYLE=virtual
run_minio_preflight DEPLOY_ROLE=compute CUBE_S3_ADDRESSING_STYLE=virtual
run_minio_preflight CUBE_ARTIFACT_STORE_BACKEND=fs CUBE_OPS_STORE_BACKEND=fs CUBE_S3_ADDRESSING_STYLE=virtual
run_minio_preflight CUBE_OPS_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ENDPOINT=https://storage.example.test

echo 'S3 addressing survives install and upgrades'
