#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
# Verify upgrade intent, persistence, and the environment handed to services.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# Use the real runtime-env loader and startup scripts; stub only host setup
# and service binaries. No root, systemd service, network, or database is needed.
mkdir -p "${TMP_DIR}/systemd" "${TMP_DIR}/bin" \
  "${TMP_DIR}/toolbox/"{CubeOps,CubeMaster,CubeTemplateCenter}/bin
cp "${ONE_CLICK_DIR}/scripts/systemd/"{cubeops,cubemaster,cubetemplatecenter}-start.sh \
  "${TMP_DIR}/systemd/"
: > "${TMP_DIR}/empty-runtime.env"
cat > "${TMP_DIR}/systemd/common.sh" <<'SH'
# shellcheck disable=SC1090
source "${TEST_ONE_CLICK_DIR}/scripts/systemd/common.sh"
TOOLBOX_ROOT="${TEST_TOOLBOX_ROOT}"
require_root() { :; }
ensure_systemd_runtime_dirs() { :; }
SH
cat > "${TMP_DIR}/toolbox/CubeOps/bin/cubeops" <<'SH'
#!/usr/bin/env bash
if [[ -n "${TEST_PRINT_COMPANION:-}" ]]; then
  printf '%s\n' "${!TEST_PRINT_COMPANION-unset}"
  exit 0
fi
if [[ "${TEST_PRINT_NODE_ENDPOINT:-}" == 1 ]]; then
  printf '%s|%s|%s\n' "${CUBE_OPS_S3_ADDRESSING_STYLE-unset}" \
    "${CUBE_OPS_S3_NODE_ENDPOINT-unset}" "${CUBE_OPS_S3_BUCKET}"
  exit 0
fi
printf '%s|%s|%s\n' "${CUBE_OPS_S3_ADDRESSING_STYLE-unset}" \
  "${CUBE_OPS_S3_PATH_STYLE-unset}" "${CUBE_OPS_S3_BUCKET}"
SH
chmod +x "${TMP_DIR}/toolbox/CubeOps/bin/cubeops"
for binary in CubeMaster/bin/cubemaster CubeTemplateCenter/bin/templatecenter; do
  cat > "${TMP_DIR}/toolbox/${binary}" <<'SH'
#!/usr/bin/env bash
if [[ -n "${TEST_PRINT_COMPANION:-}" ]]; then
  printf '%s\n' "${!TEST_PRINT_COMPANION-unset}"
  exit 0
fi
printf '%s\n' "${CUBE_S3_ADDRESSING_STYLE-unset}"
SH
  chmod +x "${TMP_DIR}/toolbox/${binary}"
done
: > "${TMP_DIR}/toolbox/CubeMaster/conf.yaml"
: > "${TMP_DIR}/toolbox/CubeTemplateCenter/conf.yaml"
printf '#!/usr/bin/env bash\nexit 0\n' > "${TMP_DIR}/bin/mkfs.ext4"
chmod +x "${TMP_DIR}/bin/mkfs.ext4"

check_start() {
  local label="$1" expected="$2" got
  shift 2
  got="$(env -i PATH="${PATH}" \
    TEST_ONE_CLICK_DIR="${ONE_CLICK_DIR}" \
    TEST_TOOLBOX_ROOT="${TMP_DIR}/toolbox" \
    ONE_CLICK_RUNTIME_ENV_FILE="${TMP_DIR}/empty-runtime.env" \
    CUBE_OPS_LOG_DIR="${TMP_DIR}/log" \
    CUBE_OPS_WAREHOUSE_WORK_DIR="${TMP_DIR}/warehouse" \
    "$@" bash "${TMP_DIR}/systemd/cubeops-start.sh")"
  [[ "${got}" == "${expected}" ]] \
    || fail "${label}: expected ${expected}, got ${got}"
}

check_start legacy-default 'unset|unset|cube-ops'
for style in auto path virtual; do
  check_start "inherit-${style}" "${style}|true|cube-ops" \
    "CUBE_S3_ADDRESSING_STYLE=${style}" CUBE_OPS_S3_PATH_STYLE=true
  check_start "override-${style}" "${style}|unset|cube-ops" \
    CUBE_S3_ADDRESSING_STYLE=virtual "CUBE_OPS_S3_ADDRESSING_STYLE=${style}"
done
check_start empty-override-inherits 'virtual|unset|cube-ops' \
  CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ADDRESSING_STYLE=
check_start ops-only 'path|unset|cube-ops' CUBE_OPS_S3_ADDRESSING_STYLE=path
check_start legacy-volume-option 'virtual|true|cube-ops' \
  CUBE_S3_ADDRESSING_STYLE=virtual CUBE_S3_S3FS_EXTRA_OPTS=-ouse_path_request_style
check_start separate-bucket 'virtual|unset|cube-ops' \
  CUBE_S3_ADDRESSING_STYLE=virtual CUBE_S3_BUCKET=cube-volumes
check_start explicit-bucket 'virtual|unset|custom-ops' \
  CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_BUCKET=custom-ops

# Run the installer's actual S3 persistence block against a temporary file,
# rather than running the host installer or duplicating its implementation.
sed -n '/^# Persist MinIO deploy settings/,/^# CubeS3lvol /p' \
  "${ONE_CLICK_DIR}/install.sh" > "${TMP_DIR}/persist.sh"
[[ -s "${TMP_DIR}/persist.sh" ]] || fail "installer S3 persistence block missing"

read_styles() {
  env -i PATH="${PATH}" bash -euo pipefail -c '
    grep -q "^CUBE_S3_ADDRESSING_STYLE=" "$1"
    grep -q "^CUBE_OPS_S3_ADDRESSING_STYLE=" "$1"
    source "$1"
    printf "%s|%s\n" "${CUBE_S3_ADDRESSING_STYLE}" "${CUBE_OPS_S3_ADDRESSING_STYLE}"
  ' _ "$1"
}

check_persist() {
  local label="$1" expected="$2" got runtime_env="${TMP_DIR}/runtime.env"
  shift 2
  printf '%s\n' CUBE_S3_ADDRESSING_STYLE=path CUBE_OPS_S3_ADDRESSING_STYLE=path \
    > "${runtime_env}"
  env -i PATH="${PATH}" CUBE_SANDBOX_MINIO_ENABLED=0 \
    CUBE_S3_ENDPOINT=https://storage.example.test \
    CUBE_S3_ACCESS_KEY_ID=test-key CUBE_S3_SECRET_ACCESS_KEY=test-secret \
    CUBE_S3_BUCKET=cube-volumes CUBE_S3_REGION=us-east-1 CUBE_S3_S3FS_EXTRA_OPTS= \
    "$@" bash -c 'source "$1"; RUNTIME_ENV_FILE="$2"; source "$3"' \
    _ "${ONE_CLICK_DIR}/lib/common.sh" "${runtime_env}" "${TMP_DIR}/persist.sh"
  got="$(read_styles "${runtime_env}")"
  [[ "${got}" == "${expected}" ]] \
    || fail "${label}: expected ${expected}, got ${got}"
}

check_persist empty-clears-stale '|'
for style in auto path virtual; do
  check_persist "persist-${style}" "${style}|" "CUBE_S3_ADDRESSING_STYLE=${style}"
done
check_persist persist-override 'virtual|path' \
  CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ADDRESSING_STYLE=path
check_persist persist-ops-only '|auto' CUBE_OPS_S3_ADDRESSING_STYLE=auto

# Missing keys must fail even if matching variables exist in the parent shell.
: > "${TMP_DIR}/missing.env"
if (export CUBE_S3_ADDRESSING_STYLE='' CUBE_OPS_S3_ADDRESSING_STYLE='';
    read_styles "${TMP_DIR}/missing.env") >/dev/null 2>&1; then
  fail "missing keys were mistaken for persisted empty values"
fi

# Execute the installer's real snapshot/capture and replay call sites around
# the real three-way merge. This also catches missing installer wiring.
sed -n '/^snapshot_one_click_toggles /,/^fi$/p' \
  "${ONE_CLICK_DIR}/install.sh" > "${TMP_DIR}/snapshot.sh"
sed -n '/^apply_one_click_toggles$/,/^apply_one_click_database_intent$/p' \
  "${ONE_CLICK_DIR}/install.sh" > "${TMP_DIR}/replay.sh"
[[ -s "${TMP_DIR}/snapshot.sh" && -s "${TMP_DIR}/replay.sh" ]] \
  || fail "installer intent call sites missing"

# The real runtime loader takes precedence over launcher environment variables.
# Override template paths in the runtime file itself so fixtures work without
# root privileges and cannot create directories under /data on the host.
write_runtime_fixture_paths() {
  local runtime_env="$1"
  printf '%s=%q\n' CUBE_OPS_LOG_DIR "${TMP_DIR}/log" \
    CUBE_OPS_WAREHOUSE_WORK_DIR "${TMP_DIR}/warehouse" \
    CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR "${TMP_DIR}/artifacts" \
    CUBE_TEMPLATE_CENTER_ARTIFACT_STORE_DIR "${TMP_DIR}/artifacts" >> "${runtime_env}"
}

check_runtime_consumers() {
  local runtime_env="$1" shared="$2" ops="$3" script got expected
  write_runtime_fixture_paths "${runtime_env}"
  for script in cubeops cubemaster cubetemplatecenter; do
    expected="${shared}"
    if [[ "${script}" == cubeops ]]; then
      expected="${ops:-${shared}}|unset|cube-ops"
    fi
    got="$(env -i PATH="${TMP_DIR}/bin:${PATH}" \
      TEST_ONE_CLICK_DIR="${ONE_CLICK_DIR}" TEST_TOOLBOX_ROOT="${TMP_DIR}/toolbox" \
      ONE_CLICK_RUNTIME_ENV_FILE="${runtime_env}" \
      CUBE_S3_ADDRESSING_STYLE=ambient CUBE_OPS_S3_ADDRESSING_STYLE=ambient \
      bash "${TMP_DIR}/systemd/${script}-start.sh")"
    [[ "${got}" == "${expected}" ]] \
      || fail "${script} runtime load: expected ${expected}, got ${got}"
  done
}

check_upgrade() {
  local label="$1" mode="$2" old_shared="$3" old_ops="$4" dotenv="$5" expected="$6"
  local case_dir="${TMP_DIR}/${label}" got
  shift 6
  mkdir -p "${case_dir}"
  printf '%s=%q\n' CUBE_S3_ADDRESSING_STYLE "${old_shared}" \
    CUBE_OPS_S3_ADDRESSING_STYLE "${old_ops}" > "${case_dir}/old.env"
  if [[ "${dotenv}" == TEMPLATE ]]; then
    cp "${ONE_CLICK_DIR}/env.example" "${case_dir}/new.env"
  elif [[ "${dotenv}" != MISSING ]]; then
    printf '%s\n' "${dotenv}" > "${case_dir}/new.env"
  fi
  env -i PATH="${PATH}" CUBE_SANDBOX_MINIO_ENABLED=0 \
    CUBE_S3_ENDPOINT=https://storage.example.test \
    CUBE_S3_ACCESS_KEY_ID=test-key CUBE_S3_SECRET_ACCESS_KEY=test-secret \
    CUBE_S3_BUCKET=cube-volumes CUBE_S3_REGION=us-east-1 CUBE_S3_S3FS_EXTRA_OPTS= \
    "$@" bash -euo pipefail -c '
      source "$1/lib/common.sh"
      ENV_FILE="$2/new.env"
      ONE_CLICK_MODE="$3"
      apply_cli_overrides() { :; }
      source "$4/snapshot.sh"
      new_dotenv=""
      [[ ! -f "${ENV_FILE}" ]] || new_dotenv="${ENV_FILE}"
      merge_env_three_way "$1/env.example" "$2/old.env" "$1/env.example" \
        "${new_dotenv}" "$2/merged.env" "$2/diff"
      load_env_file "$2/merged.env"
      apply_cli_overrides
      source "$4/replay.sh"
      cp "$2/merged.env" "$2/runtime.env"
      RUNTIME_ENV_FILE="$2/runtime.env"
      source "$4/persist.sh"
    ' _ "${ONE_CLICK_DIR}" "${case_dir}" "${mode}" "${TMP_DIR}"
  got="$(read_styles "${case_dir}/runtime.env")"
  [[ "${got}" == "${expected}" ]] \
    || fail "${label}: expected ${expected}, got ${got}"
  check_runtime_consumers "${case_dir}/runtime.env" "${expected%%|*}" "${expected#*|}"
}

check_upgrade dotenv-set-and-clear upgrade '' path \
  $'CUBE_S3_ADDRESSING_STYLE=virtual\nCUBE_OPS_S3_ADDRESSING_STYLE=' 'virtual|'
check_upgrade dotenv-change auto path virtual \
  $'CUBE_S3_ADDRESSING_STYLE=auto\nCUBE_OPS_S3_ADDRESSING_STYLE=path' 'auto|path'
check_upgrade dotenv-clear-default-mode '' virtual path \
  $'CUBE_S3_ADDRESSING_STYLE=\nCUBE_OPS_S3_ADDRESSING_STYLE=' '|'
check_upgrade environment-set upgrade '' path MISSING 'virtual|auto' \
  CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ADDRESSING_STYLE=auto
check_upgrade environment-clear auto virtual path MISSING '|' \
  CUBE_S3_ADDRESSING_STYLE= CUBE_OPS_S3_ADDRESSING_STYLE=
check_upgrade dotenv-wins upgrade path virtual \
  $'CUBE_S3_ADDRESSING_STYLE="virtual"\nCUBE_OPS_S3_ADDRESSING_STYLE=""' 'virtual|' \
  CUBE_S3_ADDRESSING_STYLE=auto CUBE_OPS_S3_ADDRESSING_STYLE=path
check_upgrade preserve-unspecified upgrade virtual path MISSING 'virtual|path'
check_upgrade commented-is-not-intent auto path auto \
  $'# CUBE_S3_ADDRESSING_STYLE=virtual\n# CUBE_OPS_S3_ADDRESSING_STYLE=' 'path|auto'
check_upgrade copied-template auto virtual path TEMPLATE 'virtual|path'
check_upgrade exported-dotenv upgrade path path \
  $'export CUBE_S3_ADDRESSING_STYLE="virtual"\n  CUBE_OPS_S3_ADDRESSING_STYLE=""' 'virtual|'
check_upgrade preserve-other-key upgrade virtual path \
  'CUBE_OPS_S3_ADDRESSING_STYLE=' 'virtual|'
check_upgrade same-export-clear upgrade path path \
  'export CUBE_S3_ADDRESSING_STYLE="virtual" CUBE_OPS_S3_ADDRESSING_STYLE=""' 'virtual|'
check_upgrade same-export-env-conflict upgrade path path \
  'export CUBE_S3_ADDRESSING_STYLE="virtual" CUBE_OPS_S3_ADDRESSING_STYLE=""' 'virtual|' \
  CUBE_S3_ADDRESSING_STYLE=auto CUBE_OPS_S3_ADDRESSING_STYLE=auto
check_upgrade same-export-reversed upgrade path path \
  'export CUBE_OPS_S3_ADDRESSING_STYLE="" CUBE_S3_ADDRESSING_STYLE="virtual"' 'virtual|'
check_upgrade same-line-assignment upgrade path path \
  'CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ADDRESSING_STYLE=' 'virtual|'
check_upgrade export-env-reference upgrade path path \
  'export CUBE_S3_ADDRESSING_STYLE="${CUBE_S3_ADDRESSING_STYLE}" CUBE_OPS_S3_ADDRESSING_STYLE=""' 'virtual|' \
  CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_ADDRESSING_STYLE=auto
check_upgrade quoted-name-is-not-assignment upgrade path path \
  'export NOTE="CUBE_OPS_S3_ADDRESSING_STYLE= CUBE_S3_ADDRESSING_STYLE=virtual"' 'path|path'

# Fresh installs copy .env directly. Run that real copy branch and the entire
# persistence tail, including later upserts, before starting the real wrapper.
sed -n '/^RUNTIME_ENV_FILE=/,/^persist_one_click_compound_env_values /p' \
  "${ONE_CLICK_DIR}/install.sh" > "${TMP_DIR}/copy-runtime.sh"
sed -n '/^# Persist MinIO deploy settings/,/^chmod +x /p' \
  "${ONE_CLICK_DIR}/install.sh" | sed '$d' > "${TMP_DIR}/persist-all.sh"
[[ -s "${TMP_DIR}/copy-runtime.sh" && -s "${TMP_DIR}/persist-all.sh" ]] \
  || fail "installer fresh-install persistence call sites missing"
# Unrelated storage defaults normally come from earlier installer setup.
python3 - "${TMP_DIR}/persist-all.sh" "${TMP_DIR}/fixture-defaults.sh" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
keys = sorted(set(re.findall(r'\$\{([A-Z][A-Z0-9_]*)\}', text)) - {'RUNTIME_ENV_FILE'})
pathlib.Path(sys.argv[2]).write_text('\n'.join(': "${' + key + ':=}"' for key in keys) + '\n')
PY

check_runtime_setting() {
  local runtime_env="$1" label="$2" service="$3" key="$4" expected="$5" got
  write_runtime_fixture_paths "${runtime_env}"
  got="$(env -i PATH="${TMP_DIR}/bin:${PATH}" TEST_ONE_CLICK_DIR="${ONE_CLICK_DIR}" \
    TEST_TOOLBOX_ROOT="${TMP_DIR}/toolbox" TEST_PRINT_COMPANION="${key}" \
    ONE_CLICK_RUNTIME_ENV_FILE="${runtime_env}" \
    CUBE_OPS_LOG_DIR="${TMP_DIR}/log" CUBE_OPS_WAREHOUSE_WORK_DIR="${TMP_DIR}/warehouse" \
    bash "${TMP_DIR}/systemd/${service}-start.sh")"
  [[ "${got}" == "${expected}" ]] \
    || fail "${label} ${service} ${key}: expected ${expected}, got ${got}"
}

check_node_endpoint() {
  local runtime_env="$1" label="$2" got
  write_runtime_fixture_paths "${runtime_env}"
  got="$(env -i PATH="${PATH}" TEST_ONE_CLICK_DIR="${ONE_CLICK_DIR}" \
    TEST_TOOLBOX_ROOT="${TMP_DIR}/toolbox" TEST_PRINT_NODE_ENDPOINT=1 \
    ONE_CLICK_RUNTIME_ENV_FILE="${runtime_env}" \
    CUBE_OPS_LOG_DIR="${TMP_DIR}/log" CUBE_OPS_WAREHOUSE_WORK_DIR="${TMP_DIR}/warehouse" \
    bash "${TMP_DIR}/systemd/cubeops-start.sh")"
  [[ "${got}" == 'virtual|https://downloads.example.test|cube-ops' ]] \
    || fail "${label}: expected virtual|https://downloads.example.test|cube-ops, got ${got}"
  if [[ "${label}" == all-ops-fields* ]]; then
    env -i PATH="${PATH}" bash -euo pipefail -c '
      source "$1"
      [[ "${CUBE_OPS_S3_ACCESS_KEY_ID}" == fixture-key ]]
      [[ "${CUBE_OPS_S3_SECRET_ACCESS_KEY}" == "fixture secret with spaces" ]]
      [[ "${CUBE_OPS_S3_REGION}" == us-west-2 ]]
      [[ "${CUBE_OPS_S3_PATH_STYLE}" == false ]]
      [[ "${CUBE_OPS_S3_CREATE_BUCKET}" == false ]]
    ' _ "${runtime_env}"
  fi
  case "${label}" in
    artifact-prefix*)
      check_runtime_setting "${runtime_env}" "${label}" cubemaster CUBE_S3_ARTIFACT_PREFIX custom-templates
      check_runtime_setting "${runtime_env}" "${label}" cubetemplatecenter CUBE_S3_ARTIFACT_PREFIX custom-templates
      ;;
    upload-limit*)
      check_runtime_setting "${runtime_env}" "${label}" cubeops CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES 1048576
      ;;
  esac
}

check_fresh_install() {
  local label="$1" assignment="$2" mode="$3" case_dir="${TMP_DIR}/fresh-$1" upgrade
  mkdir -p "${case_dir}/install"
  printf '%s\n' 'CUBE_OPS_S3_ENDPOINT=https://storage.example.test' \
    "${assignment}" > "${case_dir}/new.env"
  env -i PATH="${PATH}" bash -euo pipefail -c '
    source "$1/lib/common.sh"
    # Load template defaults solely for unrelated persistence fixture settings.
    load_env_file "$1/env.example"
    ENV_FILE="$2/new.env"
    ONE_CLICK_MODE="$4"
    apply_cli_overrides() { :; }
    source "$3/snapshot.sh"
    source "$3/replay.sh"
    source "$3/fixture-defaults.sh"
    INSTALL_PREFIX="$2/install"
    INSTALL_MODE=install
    MERGED_ENV=""
    source "$3/copy-runtime.sh"
    source "$3/persist-all.sh"
    [[ "$(stat -c %a "${RUNTIME_ENV_FILE}")" == 600 ]]
  ' _ "${ONE_CLICK_DIR}" "${case_dir}" "${TMP_DIR}" "${mode}"
  check_node_endpoint "${case_dir}/install/.one-click.env" "${label} fresh install"

  # Consume the actual generated file on the next two upgrades. No new .env
  # or process S3 settings may rescue values dropped by serialization/merge.
  for upgrade in 1 2; do
    env -i PATH="${PATH}" bash -euo pipefail -c '
      source "$1/lib/common.sh"
      load_env_file "$1/env.example"
      ENV_FILE="$2/absent.env"
      ONE_CLICK_MODE=upgrade
      apply_cli_overrides() { :; }
      source "$3/snapshot.sh"
      merge_env_three_way "$1/env.example" "$2/install/.one-click.env" \
        "$1/env.example" "" "$2/merged.env" "$2/upgrade.diff"
      load_env_file "$2/merged.env"
      source "$3/replay.sh"
      source "$3/fixture-defaults.sh"
      INSTALL_PREFIX="$2/install"
      INSTALL_MODE=upgrade
      MERGED_ENV="$2/merged.env"
      source "$3/copy-runtime.sh"
      source "$3/persist-all.sh"
      [[ "$(stat -c %a "${RUNTIME_ENV_FILE}")" == 600 ]]
    ' _ "${ONE_CLICK_DIR}" "${case_dir}" "${TMP_DIR}"
    check_node_endpoint "${case_dir}/install/.one-click.env" "${label} upgrade ${upgrade}"
  done
}

check_fresh_install ops-leading \
  'CUBE_OPS_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test' install
check_fresh_install shared-leading \
  'CUBE_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test' ''
check_fresh_install export-control \
  'export CUBE_OPS_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test' install
check_fresh_install endpoint-leading \
  'CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test CUBE_OPS_S3_ADDRESSING_STYLE=virtual' install
check_fresh_install all-ops-fields \
  'CUBE_OPS_S3_ADDRESSING_STYLE=virtual CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test CUBE_OPS_S3_ACCESS_KEY_ID=fixture-key CUBE_OPS_S3_SECRET_ACCESS_KEY="fixture secret with spaces" CUBE_OPS_S3_REGION=us-west-2 CUBE_OPS_S3_PATH_STYLE=false CUBE_OPS_S3_CREATE_BUCKET=false' install
check_fresh_install artifact-prefix \
  'CUBE_S3_ADDRESSING_STYLE=virtual CUBE_S3_ARTIFACT_PREFIX=custom-templates CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test' install
check_fresh_install upload-limit \
  'CUBE_OPS_S3_ADDRESSING_STYLE=virtual CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES=1048576 CUBE_OPS_S3_NODE_ENDPOINT=https://downloads.example.test' install

echo 'PASS: S3 addressing upgrades, fresh installs, persistence, and runtime loading'
