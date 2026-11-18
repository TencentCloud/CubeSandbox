#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

src="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
cd "${tmp}"
mkdir -p install wrappers bin
for component in CubeOps CubeMaster CubeTemplateCenter; do
  mkdir -p "install/${component}/bin"
  touch "install/${component}/conf.yaml"
done

# Stub host setup and binaries. Each launcher still sources the real runtime
# environment and passes its result to the service in a clean process.
cp "${src}/scripts/systemd/"{cubeops,cubemaster,cubetemplatecenter}-start.sh wrappers/
cat > wrappers/common.sh <<'SH'
source "${TEST_SRC}/scripts/systemd/common.sh"
TOOLBOX_ROOT="${PWD}/install"
require_root() { :; }
ensure_systemd_runtime_dirs() { :; }
SH
cat > bin/service <<'SH'
#!/usr/bin/env bash
set -eu
[[ "${CUBE_S3_ARTIFACT_PREFIX}" == custom-templates ]]
[[ "${CUBE_SANDBOX_MYSQL_PASSWORD}" == "${MYSQL_SECRET}" && -n "${MYSQL_SECRET}" ]]
[[ "${CUBE_SANDBOX_MYSQL_DB}" == "${EXPECT_DB}" ]]
[[ "${CUBE_S3_ADDRESSING_STYLE}" == "${EXPECT_SHARED}" ]]
if [[ "$0" == */cubeops ]]; then
  [[ "${CUBE_OPS_S3_ADDRESSING_STYLE}" == "${EXPECT_OPS}" ]]
  [[ "${CUBE_OPS_S3_NODE_ENDPOINT}" == "${EXPECT_NODE}" ]]
  [[ "${CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES}" == "${EXPECT_LIMIT}" ]]
fi
SH
chmod +x bin/service
for binary in CubeOps/bin/cubeops CubeMaster/bin/cubemaster CubeTemplateCenter/bin/templatecenter; do
  ln -s "${PWD}/bin/service" "install/${binary}"
done
ln -s /bin/true bin/mkfs.ext4

cat > new.env <<'ENV'
CUBE_S3_ADDRESSING_STYLE=virtual CUBE_S3_ARTIFACT_PREFIX=custom-templates
export CUBE_OPS_S3_ADDRESSING_STYLE=path CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES=1048576
MYSQL_SECRET=fixture-value CUBE_SANDBOX_MYSQL_PASSWORD="${MYSQL_SECRET}"
export CUBE_S3_ADDRESSING_STYLE=virtual CUBE_SANDBOX_MYSQL_DB=staging
CUBE_SANDBOX_MYSQL_DB=production
ENV
# Use the actual installer copy and merge calls. Replacing either with an
# approximation would miss a missing override argument or a stale runtime.
sed -n '/^RUNTIME_ENV_FILE=/,/^# Install version files/{ /^# Install version files/!p; }' \
  "${src}/install.sh" > copy.sh
sed -n '/^S3_OVERRIDES=/,/^MERGED_ENV=/p' "${src}/install.sh" > capture.sh
sed -n '/^  python3 .*s3_addressing_env.py.* stored /,/^  apply_cli_overrides$/p' \
  "${src}/install.sh" > merge.sh
grep -q 'merge_env_three_way' merge.sh
grep -q 'chmod 600' copy.sh

install_env() {
  local mode="$1"
  shift
  env -i PATH="${PATH}" "$@" bash -euo pipefail -c '
    SCRIPT_DIR="$1" INSTALL_PREFIX="${PWD}/install" INSTALL_MODE="$2"
    source "${SCRIPT_DIR}/lib/common.sh"
    ENV_FILE="${PWD}/new.env"
    [[ ! -f "${ENV_FILE}" ]] || load_env_file "${ENV_FILE}"
    WORK_DIR="${PWD}"
    source ./capture.sh
    if [[ "${INSTALL_MODE}" == upgrade ]]; then
      RUNTIME_ENV_OLD="${INSTALL_PREFIX}/.one-click.env"
      MERGE_OLD_BASELINE="${SCRIPT_DIR}/env.example"
      MERGE_NEW_DOTENV=""
      [[ ! -f "${ENV_FILE}" ]] || MERGE_NEW_DOTENV="${ENV_FILE}"
      MERGED_ENV="${PWD}/merged.env" ENV_DIFF_FILE="${PWD}/diff"
      apply_cli_overrides() { :; }
      log() { :; }
      source ./merge.sh
    fi
    source ./copy.sh
    persist_one_click_database_runtime_env "${RUNTIME_ENV_FILE}"
    [[ "$(stat -c %a "${RUNTIME_ENV_FILE}")" == 600 ]]
    for key in CUBE_OPS_LOG_DIR CUBE_OPS_WAREHOUSE_WORK_DIR CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR CUBE_TEMPLATE_CENTER_ARTIFACT_STORE_DIR; do
      upsert_env_kv "${RUNTIME_ENV_FILE}" "${key}" "${PWD}/scratch"
    done
  ' _ "${src}" "${mode}"
}

check_services() {
  local service
  for service in cubeops cubemaster cubetemplatecenter; do
    env -i PATH="${PWD}/bin:${PATH}" TEST_SRC="${src}" EXPECT_SHARED="$1" EXPECT_OPS="$2" \
      EXPECT_NODE="${node}" EXPECT_LIMIT="${limit}" EXPECT_DB="${db}" \
      ONE_CLICK_RUNTIME_ENV_FILE="${PWD}/install/.one-click.env" bash "wrappers/${service}-start.sh"
  done
}

node=https://downloads.example.test limit=1048576 db=production
install_env install
check_services virtual path
rm new.env
install_env upgrade
check_services virtual path
printf '%s\n' 'export CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ADDRESSING_STYLE=""' > new.env
install_env upgrade CUBE_S3_ADDRESSING_STYLE=path CUBE_OPS_S3_ADDRESSING_STYLE=path
check_services virtual virtual
rm new.env
install_env upgrade CUBE_S3_ADDRESSING_STYLE=auto
check_services auto auto
install_env upgrade
check_services auto auto
printf '\nCUBE_S3_ADDRESSING_STYLE=path\n' >> install/.one-click.env
install_env upgrade
check_services path path
node=https://new-downloads.example.test limit=2048 db=nextdb
printf 'export CUBE_OPS_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_NODE_ENDPOINT=%s CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES=%s\nCUBE_SANDBOX_MYSQL_DB=%s\n' \
  "${node}" "${limit}" "${db}" > new.env
install_env upgrade
check_services path virtual
rm new.env
install_env upgrade
check_services path virtual
# An operator may edit the installed file with a shell reference. The
# subsequent merge must load that value and keep its literal companions.
cat >> install/.one-click.env <<'ENV'
LEGACY_STYLE=auto
export CUBE_OPS_S3_ADDRESSING_STYLE="$LEGACY_STYLE" CUBE_OPS_S3_NODE_ENDPOINT=https://legacy-downloads.example.test
ENV
node=https://legacy-downloads.example.test
install_env upgrade
check_services path auto
cat >> install/.one-click.env <<'ENV'
DOWNLOAD_HOST=expanded-downloads.example.test
CUBE_OPS_S3_ADDRESSING_STYLE=auto CUBE_OPS_S3_NODE_ENDPOINT="https://$DOWNLOAD_HOST"
ENV
printf 'CUBE_OPS_S3_ADDRESSING_STYLE=virtual\n' > new.env
node=https://expanded-downloads.example.test
install_env upgrade
check_services path virtual
cat >> install/.one-click.env <<'ENV'
MY_DATABASE=expanded_db
CUBE_S3_ADDRESSING_STYLE=auto CUBE_SANDBOX_MYSQL_DB="$MY_DATABASE"
ENV
printf 'CUBE_S3_ADDRESSING_STYLE=virtual\n' > new.env
db=expanded_db
install_env upgrade
check_services virtual virtual
rm new.env
install_env upgrade
check_services virtual virtual
[[ "$(grep -c '^CUBE_S3_ADDRESSING_STYLE=' install/.one-click.env)" == 1 ]]
echo "S3 addressing survives install, upgrades and service startup"
