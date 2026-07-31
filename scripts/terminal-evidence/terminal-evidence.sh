#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

usage() {
  cat <<'EOF'
Usage:
  terminal-evidence.sh --run-real --endpoint URL --task-id ID [options]
  terminal-evidence.sh --preflight-only --endpoint URL --task-id ID [options]
  terminal-evidence.sh --cleanup-only --endpoint URL --task-id ID --output-dir DIR [options]

Required:
  --endpoint URL          Deployed non-localhost WebUI/nginx origin.
  --task-id ID            Unique 6-64 character lowercase run ID.

Credential input:
  --credential-file FILE  Mode 0400/0600 JSON with username/password. The path is
                          passed to the browser driver through the environment.
  --secondary-credential-file FILE
                          Optional distinct mode-0400/0600 credential for the
                          second real user in the concurrency matrix.
  --direct-db-task-users  Test-only, explicit authorization required: create two
                          exact <task-id>-a/-b bcrypt users directly in the live
                          authentication table and remove only those rows during
                          cleanup. This cannot be combined with credential files.
  --allow-public-login-hint
                          Dev-only: extract the credentials already displayed by
                          the deployed login hint into a task-owned mode-0600 file.

Options:
  --output-dir DIR        Raw output root (default:
                          /data/cubelet/cubesandbox-terminal-evidence/<task-id>).
  --template-id ID        Explicit READY template; otherwise discover one.
  --playwright-module DIR Existing Playwright module root (never installed here).
  --chromium PATH         Existing Chromium executable.
  --require-multi-container
                          Fail unless the real WebUI exposes two live containers
                          and both task-owned role markers are verified.
  --resume-run            Continue the same exact run directory after a failure.
  --help                  Show this help.

The runner never creates cloud resources. Direct database user creation happens
only with --direct-db-task-users and uses exact task-owned usernames, bcrypt,
stdin-only SQL, and bounded cleanup. It otherwise creates only exact task-owned
sandboxes and browser/network state recorded in state.json. Raw logs,
credentials, profiles, and traces stay outside git under /data/cubelet. Every
step is PASS, FAIL, or an explicit SKIP; unsupported coverage is never promoted
to PASS.
EOF
}

mode=''
endpoint=''
task_id=''
output_dir=''
credential_file=''
secondary_credential_file=''
direct_db_task_users=false
allow_public_login_hint=false
template_id=''
playwright_module=${TE_PLAYWRIGHT_MODULE:-/opt/cubesandbox-dev-tools/node_modules}
chromium_path=${TE_CHROMIUM_PATH:-/home/hongcao3224/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome}
require_multi_container=false
resume_run=false

while (($# > 0)); do
  case "$1" in
    --run-real|--preflight-only|--cleanup-only)
      [[ -z "$mode" ]] || te_die 'choose exactly one execution mode'
      mode=${1#--}
      shift
      ;;
    --endpoint|--task-id|--output-dir|--credential-file|--secondary-credential-file|--template-id|--playwright-module|--chromium)
      (($# >= 2)) || te_die "$1 requires a value"
      case "$1" in
        --endpoint) endpoint=$2 ;;
        --task-id) task_id=$2 ;;
        --output-dir) output_dir=$2 ;;
        --credential-file) credential_file=$2 ;;
        --secondary-credential-file) secondary_credential_file=$2 ;;
        --template-id) template_id=$2 ;;
        --playwright-module) playwright_module=$2 ;;
        --chromium) chromium_path=$2 ;;
      esac
      shift 2
      ;;
    --allow-public-login-hint)
      allow_public_login_hint=true
      shift
      ;;
    --direct-db-task-users)
      direct_db_task_users=true
      shift
      ;;
    --require-multi-container)
      require_multi_container=true
      shift
      ;;
    --resume-run)
      resume_run=true
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      te_die "unknown argument: $1"
      ;;
  esac
done

[[ -n "$mode" ]] || te_die 'choose --run-real, --preflight-only, or --cleanup-only'
te_validate_task_id "$task_id"
te_validate_endpoint "$endpoint"
endpoint=${endpoint%/}
output_dir=${output_dir:-/data/cubelet/cubesandbox-terminal-evidence/$task_id}
te_validate_output_dir "$output_dir" "$task_id"

if [[ "$direct_db_task_users" == true && ( -n "$credential_file" || -n "$secondary_credential_file" || "$allow_public_login_hint" == true ) ]]; then
  te_die '--direct-db-task-users cannot be combined with credential files or the public login hint'
fi
if [[ "$allow_public_login_hint" == true && -n "$credential_file" ]]; then
  te_die 'choose --credential-file or --allow-public-login-hint, not both'
fi
if [[ -n "$secondary_credential_file" && -z "$credential_file" && "$allow_public_login_hint" != true ]]; then
  te_die '--secondary-credential-file requires --credential-file'
fi
if [[ "$mode" != preflight-only && -z "$credential_file" && "$allow_public_login_hint" != true && "$direct_db_task_users" != true ]]; then
  te_die 'credential input is required'
fi
if [[ "$mode" == run-real && "$direct_db_task_users" != true && -z "$secondary_credential_file" ]]; then
  te_die '--run-real requires --secondary-credential-file or --direct-db-task-users for the two-user hard gate'
fi

if [[ "$mode" == cleanup-only ]]; then
  [[ -d "$output_dir" ]] || te_die "cleanup run directory is missing: $output_dir"
  resume_run=true
fi
te_init_output "$output_dir" "$resume_run"

current_step='preflight'
cubelet_config_changed=false
cleanup_started=false
sandbox_cleanup_completed=false
task_username_a="${task_id}-a"
task_username_b="${task_id}-b"
task_credential_a="$output_dir/raw/login-credential-a.json"
task_credential_b="$output_dir/raw/login-credential-b.json"
task_users_marker="$output_dir/raw/task-users.json"
te_validate_task_username "$task_username_a"
te_validate_task_username "$task_username_b"
endpoint_host=${endpoint#*://}
endpoint_ip=${endpoint_host%%:*}
endpoint_port=${endpoint_host##*:}
if [[ "$endpoint_port" == "$endpoint_host" ]]; then
  [[ "$endpoint" == https://* ]] && endpoint_port=443 || endpoint_port=80
fi

restore_cubelet_config() {
  local live=/usr/local/services/cubetoolbox/Cubelet/config/config.toml
  local backup="$output_dir/rollback/cubelet-config.toml"
  local window="$output_dir/summaries/idle-runtime-window.tsv"
  if [[ "$cubelet_config_changed" != true ]]; then
    return 0
  fi
  sudo -n test -f "$backup"
  sudo -n cp -a "$backup" "${live}.commit7-restore"
  sudo -n mv -f "${live}.commit7-restore" "$live"
  sudo -n systemctl restart cube-sandbox-cubelet.service
  te_wait_cubelet_health
  cubelet_config_changed=false
  printf '%s\trestored\t%s\t%s\n' \
    "$(te_now)" \
    "$(sudo -n sha256sum "$live" | awk '{print $1}')" \
    "$(systemctl show cube-sandbox-cubelet.service -p MainPID --value)" >>"$window"
  te_record_step "$output_dir" restore-idle-config PASS 'exact Cubelet config prestate restored; Cubelet socket healthy'
}

create_direct_db_task_users() {
  local check_sql="$output_dir/raw/.task-users-check.sql"
  local create_sql="$output_dir/raw/.task-users-create.sql"
  local check_output create_output
  printf "SELECT CONCAT('task_users=',COUNT(*)) FROM t_system_user WHERE username IN ('%s','%s');\n" \
    "$task_username_a" "$task_username_b" >"$check_sql"
  chmod 0600 "$check_sql"
  if ! check_output=$(te_mysql_from_file "$check_sql"); then
    rm -f -- "$check_sql"
    return 1
  fi
  rm -f -- "$check_sql"

  if [[ -f "$task_users_marker" ]]; then
    te_validate_credential_pair "$task_credential_a" "$task_credential_b"
    [[ "$check_output" == 'task_users=2' ]] ||
      te_die 'recorded task users are not both present; refusing implicit recreation'
    credential_file=$task_credential_a
    secondary_credential_file=$task_credential_b
    te_record_step "$output_dir" provision-task-users PASS 'two existing exact task-owned DB users verified for resumed run'
    return 0
  fi

  [[ "$check_output" == 'task_users=0' ]] ||
    te_die 'one or more exact task usernames already exist without this run marker'
  python3 - "$task_username_a" "$task_username_b" "$task_credential_a" "$task_credential_b" "$create_sql" "$task_users_marker" <<'PY'
import bcrypt
import json
import os
import secrets
import sys
from datetime import datetime, timezone

username_a, username_b, credential_a, credential_b, sql_path, marker_path = sys.argv[1:]

def write_exclusive(path, value):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, 'w', encoding='utf-8') as stream:
        stream.write(value)
    os.chmod(path, 0o600)

password_a = secrets.token_urlsafe(32)
password_b = secrets.token_urlsafe(32)
hash_a = bcrypt.hashpw(password_a.encode(), bcrypt.gensalt(rounds=10)).decode()
hash_b = bcrypt.hashpw(password_b.encode(), bcrypt.gensalt(rounds=10)).decode()
write_exclusive(credential_a, json.dumps({'username': username_a, 'password': password_a}) + '\n')
write_exclusive(credential_b, json.dumps({'username': username_b, 'password': password_b}) + '\n')
sql = (
    'START TRANSACTION;\n'
    f"INSERT INTO t_system_user (username,password) VALUES ('{username_a}','{hash_a}');\n"
    f"INSERT INTO t_system_user (username,password) VALUES ('{username_b}','{hash_b}');\n"
    'COMMIT;\n'
    f"SELECT CONCAT('task_users=',COUNT(*)) FROM t_system_user WHERE username IN ('{username_a}','{username_b}');\n"
)
write_exclusive(sql_path, sql)
write_exclusive(marker_path, json.dumps({
    'schemaVersion': 1,
    'usernames': [username_a, username_b],
    'credentialFiles': [os.path.basename(credential_a), os.path.basename(credential_b)],
    'creationMode': 'authorized-direct-db',
    'preparedAt': datetime.now(timezone.utc).isoformat().replace('+00:00', 'Z'),
}) + '\n')
password_a = password_b = hash_a = hash_b = sql = ''
PY
  if ! create_output=$(te_mysql_from_file "$create_sql"); then
    rm -f -- "$create_sql"
    return 1
  fi
  rm -f -- "$create_sql"
  [[ "$create_output" == 'task_users=2' ]] || te_die 'direct DB task-user creation did not produce exactly two users'
  credential_file=$task_credential_a
  secondary_credential_file=$task_credential_b
  te_validate_credential_pair "$credential_file" "$secondary_credential_file"
  jq -n \
    --arg user_a "$task_username_a" \
    --arg user_b "$task_username_b" \
    --arg observed_at "$(te_now)" \
    '{creationMode:"authorized-direct-db",users:[$user_a,$user_b],distinctUsers:true,passwordsRecorded:false,bcryptHashesRecorded:false,rowCount:2,observedAt:$observed_at}' \
    >"$output_dir/summaries/task-users-provision.json"
  chmod 0600 "$output_dir/summaries/task-users-provision.json"
  te_record_step "$output_dir" provision-task-users PASS 'two exact task-owned bcrypt users created through stdin-only SQL'
}

cleanup_direct_db_task_users() {
  [[ "$direct_db_task_users" == true ]] || return 0
  local precheck_sql="$output_dir/raw/.task-users-cleanup-precheck.sql"
  local cleanup_sql="$output_dir/raw/.task-users-cleanup.sql"
  local precheck_output cleanup_output
  printf "SELECT CONCAT('open_sessions=',COUNT(*)) FROM terminal_sessions WHERE user_id IN ('%s','%s') AND closed_at IS NULL;\n" \
    "$task_username_a" "$task_username_b" >"$precheck_sql"
  chmod 0600 "$precheck_sql"
  if ! precheck_output=$(te_mysql_from_file "$precheck_sql"); then
    rm -f -- "$precheck_sql"
    return 1
  fi
  rm -f -- "$precheck_sql"
  [[ "$precheck_output" == 'open_sessions=0' ]] ||
    {
      te_die 'task-owned users still have open terminal sessions; credentials and DB rows were retained'
      return 1
    }

  printf '%s\n' \
    'START TRANSACTION;' \
    "UPDATE t_refresh_token SET revoked_at=UTC_TIMESTAMP() WHERE username IN ('$task_username_a','$task_username_b') AND revoked_at IS NULL;" \
    "DELETE FROM t_refresh_token WHERE username IN ('$task_username_a','$task_username_b');" \
    "DELETE FROM terminal_grants WHERE user_id IN ('$task_username_a','$task_username_b');" \
    "DELETE FROM t_system_user WHERE username IN ('$task_username_a','$task_username_b');" \
    'COMMIT;' \
    "SELECT CONCAT('open_sessions=',COUNT(*)) FROM terminal_sessions WHERE user_id IN ('$task_username_a','$task_username_b') AND closed_at IS NULL;" \
    "SELECT CONCAT('refresh_rows=',COUNT(*)) FROM t_refresh_token WHERE username IN ('$task_username_a','$task_username_b');" \
    "SELECT CONCAT('grant_rows=',COUNT(*)) FROM terminal_grants WHERE user_id IN ('$task_username_a','$task_username_b');" \
    "SELECT CONCAT('user_rows=',COUNT(*)) FROM t_system_user WHERE username IN ('$task_username_a','$task_username_b');" \
    >"$cleanup_sql"
  chmod 0600 "$cleanup_sql"
  if ! cleanup_output=$(te_mysql_from_file "$cleanup_sql"); then
    rm -f -- "$cleanup_sql"
    return 1
  fi
  rm -f -- "$cleanup_sql"
  for expected in open_sessions=0 refresh_rows=0 grant_rows=0 user_rows=0; do
    grep -Fxq -- "$expected" <<<"$cleanup_output" ||
      {
        te_die "task-user cleanup verification failed: $expected"
        return 1
      }
  done
  rm -f -- "$task_credential_a" "$task_credential_b"
  jq -n \
    --arg user_a "$task_username_a" \
    --arg user_b "$task_username_b" \
    --arg observed_at "$(te_now)" \
    '{users:[$user_a,$user_b],openSessions:0,refreshRows:0,grantRows:0,userRows:0,terminalSessionAuditRowsDeleted:false,credentialFilesRemoved:true,result:"PASS",observedAt:$observed_at}' \
    >"$output_dir/summaries/task-users-cleanup.json"
  chmod 0600 "$output_dir/summaries/task-users-cleanup.json"
  if [[ -f "$task_users_marker" ]]; then
    local marker_tmp="${task_users_marker}.tmp.$$"
    jq --arg cleaned_at "$(te_now)" '.status="cleaned" | .cleanedAt=$cleaned_at | .credentialFilesRemoved=true' \
      "$task_users_marker" >"$marker_tmp"
    chmod 0600 "$marker_tmp"
    mv -f "$marker_tmp" "$task_users_marker"
  fi
  te_record_step "$output_dir" cleanup-task-users PASS 'exact refresh/grant/user rows removed; terminal session audit rows retained'
}

cleanup_generated_login_hint() {
  if [[ "$allow_public_login_hint" == true && "$credential_file" == "$output_dir/raw/login-credential.json" ]]; then
    rm -f -- "$credential_file"
  fi
}

browser_phase() {
  local phase=$1
  local log_path="$output_dir/raw/browser-$phase.log"
  local tmp_tag
  tmp_tag=$(printf '%s' "$task_id" | sha256sum | cut -c1-12)
  local task_tmp="/data/cubelet/c7e/$tmp_tag"
  local phase_tmp="$task_tmp/$phase"
  local task_uid task_gid
  task_uid=$(id -u)
  task_gid=$(id -g)
  if [[ -e /data/cubelet/c7e && ( ! -d /data/cubelet/c7e || -L /data/cubelet/c7e ) ]]; then
    te_die '/data/cubelet/c7e exists but is not a real directory'
  fi
  sudo -n install -d -o root -g root -m 0755 /data/cubelet/c7e
  sudo -n install -d -o "$task_uid" -g "$task_gid" -m 0700 "$task_tmp"
  install -d -m 0700 "$phase_tmp"
  current_step="browser-$phase"
  local exit_code=0
  if TE_ENDPOINT="$endpoint" \
    TE_TASK_ID="$task_id" \
    TE_OUTPUT_DIR="$output_dir" \
    TE_CREDENTIAL_FILE="$credential_file" \
    TE_SECONDARY_CREDENTIAL_FILE="$secondary_credential_file" \
    TE_TEMPLATE_ID="$template_id" \
    TE_REQUIRE_MULTI_CONTAINER="$require_multi_container" \
    TE_CHROMIUM_PATH="$chromium_path" \
    NODE_PATH="$playwright_module" \
    TMPDIR="$phase_tmp" \
    XDG_CACHE_HOME="$output_dir/browser-cache" \
    XDG_CONFIG_HOME="$output_dir/browser-config" \
      te_run_step "$output_dir" "$current_step" "$log_path" \
        node "$SCRIPT_DIR/browser.cjs" "$phase"; then
    exit_code=0
  else
    exit_code=$?
  fi
  find "$phase_tmp" -depth -delete
  sudo -n rmdir "$task_tmp" 2>/dev/null || true
  sudo -n rmdir /data/cubelet/c7e 2>/dev/null || true
  return "$exit_code"
}

cleanup_resources() {
  local include_task_users=${1:-false}
  if [[ "$cleanup_started" == true ]]; then
    if [[ "$include_task_users" == true && "$sandbox_cleanup_completed" == true ]]; then
      local repeated_cleanup_exit=0
      if ! cleanup_direct_db_task_users; then
        repeated_cleanup_exit=1
      fi
      cleanup_generated_login_hint
      return "$repeated_cleanup_exit"
    fi
    return 0
  fi
  cleanup_started=true
  local cleanup_exit=0
  local sandbox_cleanup_ok=true
  if ! restore_cubelet_config; then
    te_record_step "$output_dir" cleanup-restore-runtime FAIL 'exact runtime environment restore failed'
    cleanup_exit=1
  fi
  if ! te_remove_exact_firewall_rule "$task_id" "$endpoint_ip" "$endpoint_port"; then
    te_record_step "$output_dir" cleanup-firewall FAIL 'exact task firewall rule removal failed'
    cleanup_exit=1
  fi
  if [[ -f "$output_dir/state.json" ]]; then
    sandbox_cleanup_ok=false
    if [[ -z "$credential_file" || ! -f "$credential_file" ]]; then
      if jq -e '.phases.cleanup.status == "PASS" and ([.cleanup[]?.absent] | all)' "$output_dir/state.json" >/dev/null 2>&1; then
        sandbox_cleanup_ok=true
      else
        te_record_step "$output_dir" cleanup-sandboxes FAIL 'recorded sandbox cleanup requires the retained primary credential file'
        cleanup_exit=1
      fi
    else
      if ! browser_phase cleanup; then
        te_record_step "$output_dir" cleanup-sandboxes FAIL 'exact recorded sandbox cleanup failed'
        cleanup_exit=1
      else
        sandbox_cleanup_ok=true
      fi
    fi
  fi
  sandbox_cleanup_completed=$sandbox_cleanup_ok
  if [[ "$sandbox_cleanup_ok" == true && "$include_task_users" == true ]]; then
    if ! cleanup_direct_db_task_users; then
      te_record_step "$output_dir" cleanup-task-users FAIL 'exact task-owned DB user cleanup failed; credentials retained when possible'
      cleanup_exit=1
    fi
    cleanup_generated_login_hint
  fi
  return "$cleanup_exit"
}

on_exit() {
  local exit_code=$?
  trap - EXIT INT TERM
  if ((exit_code != 0)); then
    te_capture_failure_once "$output_dir" "$current_step" "$exit_code" || true
    if ! cleanup_resources true; then
      te_record_step "$output_dir" cleanup-after-failure FAIL 'one or more exact cleanup steps failed'
    fi
  fi
  exit "$exit_code"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for command_name in awk bash ctr curl df docker find getent git install iptables jq journalctl mv node ps rg sha256sum stat sudo systemctl wc; do
  te_require_command "$command_name"
done
endpoint_ip=$(getent ahostsv4 "$endpoint_ip" | awk 'NR == 1 { print $1 }')
[[ -n "$endpoint_ip" ]] || te_die 'endpoint did not resolve to an IPv4 address for the scoped transport-loss probe'
[[ -d "$playwright_module/playwright" ]] || te_die "Playwright module is missing: $playwright_module/playwright"
[[ -x "$chromium_path" ]] || te_die "Chromium is missing or not executable: $chromium_path"
sudo -n true
curl -fsS --connect-timeout 3 --max-time 10 -o /dev/null "$endpoint/"
curl -fsS --connect-timeout 3 --max-time 10 -o /dev/null http://127.0.0.1:3010/health
systemctl is-active --quiet cube-sandbox-cubeops.service
systemctl is-active --quiet cube-sandbox-cubemaster.service
systemctl is-active --quiet cube-sandbox-cubelet.service
systemctl is-active --quiet cube-sandbox-network-agent.service
systemctl is-active --quiet cube-sandbox-webui.service
te_capture_service_state "$output_dir/raw/services-preflight.txt"
if [[ ! -f "$output_dir/started-at.txt" ]]; then
  printf '%s\n' "$(te_now)" >"$output_dir/started-at.txt"
fi
chmod 0600 "$output_dir/started-at.txt"
if [[ "$direct_db_task_users" == true ]]; then
  te_require_command python3
  python3 -c 'import bcrypt' >/dev/null
fi
te_record_step "$output_dir" preflight PASS 'non-localhost endpoint and required runtime/tooling available'

if [[ "$mode" == preflight-only ]]; then
  if [[ -n "$credential_file" ]]; then
    te_validate_credential_file "$credential_file"
  fi
  printf 'PASS: preflight completed; raw output: %s\n' "$output_dir"
  trap - EXIT INT TERM
  exit 0
fi

if [[ "$allow_public_login_hint" == true ]]; then
  credential_file="$output_dir/raw/login-credential.json"
  browser_phase credential-from-public-hint
fi
if [[ "$direct_db_task_users" == true ]]; then
  credential_file=$task_credential_a
  secondary_credential_file=$task_credential_b
  if [[ "$mode" != cleanup-only ]]; then
    current_step='provision-task-users'
    create_direct_db_task_users
  fi
fi
if [[ -n "$credential_file" ]]; then
  te_validate_credential_file "$credential_file"
fi
if [[ -n "$secondary_credential_file" ]]; then
  te_validate_credential_pair "$credential_file" "$secondary_credential_file"
fi

if [[ "$mode" == cleanup-only ]]; then
  cleanup_resources true
  printf 'PASS: exact recorded cleanup attempted; output: %s\n' "$output_dir"
  trap - EXIT INT TERM
  exit 0
fi

browser_phase discover
browser_phase provision
browser_phase core
browser_phase security
browser_phase grace-expiry
browser_phase concurrency

current_step='shorten-idle-timeout'
install -d -m 0700 "$output_dir/rollback"
cubelet_config=/usr/local/services/cubetoolbox/Cubelet/config/config.toml
idle_window="$output_dir/summaries/idle-runtime-window.tsv"
rendered_config="$output_dir/rollback/cubelet-config.idle-test.toml"
sudo -n cp -a "$cubelet_config" "$output_dir/rollback/cubelet-config.toml"
# shellcheck disable=SC2024
# Only the root-owned source is read under sudo.
sudo -n stat -c 'mode=%a owner=%U group=%G size=%s' "$cubelet_config" >"$output_dir/rollback/cubelet-config.metadata"
printf 'timestamp\tstate\tconfig_sha256\tcubelet_pid\n' >"$idle_window"
printf '%s\tbaseline\t%s\t%s\n' \
  "$(te_now)" \
  "$(sudo -n sha256sum "$cubelet_config" | awk '{print $1}')" \
  "$(systemctl show cube-sandbox-cubelet.service -p MainPID --value)" >>"$idle_window"
te_render_cubelet_idle_timeout "$cubelet_config" 1 >"$rendered_config"
chmod 0600 "$rendered_config"
sudo -n cp -a "$cubelet_config" "${cubelet_config}.commit7-idle"
sudo -n cp -- "$rendered_config" "${cubelet_config}.commit7-idle"
sudo -n chown --reference="$cubelet_config" "${cubelet_config}.commit7-idle"
sudo -n chmod --reference="$cubelet_config" "${cubelet_config}.commit7-idle"
sudo -n mv -f "${cubelet_config}.commit7-idle" "$cubelet_config"
rm -f -- "$rendered_config"
cubelet_config_changed=true
sudo -n awk '
  $0 == "[plugins.\"io.cubelet.cubebox-service.v1.cubebox-service\".terminal]" { in_target=1; next }
  in_target && /^[[:space:]]*\[/ { in_target=0 }
  in_target && /^[[:space:]]*idle_timeout_minutes[[:space:]]*=[[:space:]]*1[[:space:]]*$/ { count++ }
  END { exit count == 1 ? 0 : 1 }
' "$cubelet_config"
sudo -n systemctl restart cube-sandbox-cubelet.service
te_wait_cubelet_health
printf '%s\ttest-one-minute\t%s\t%s\n' \
  "$(te_now)" \
  "$(sudo -n sha256sum "$cubelet_config" | awk '{print $1}')" \
  "$(systemctl show cube-sandbox-cubelet.service -p MainPID --value)" >>"$idle_window"
te_record_step "$output_dir" "$current_step" PASS 'Cubelet terminal idle timeout set to one minute; only Cubelet restarted'
browser_phase idle
restore_cubelet_config

browser_phase drain
browser_phase audit-correlation
cleanup_resources false

current_step='final-resource-check'
browser_phase verify-cleanup
if ! cleanup_direct_db_task_users; then
  te_record_step "$output_dir" cleanup-task-users FAIL 'exact task-owned DB user cleanup failed after resource verification'
  exit 1
fi
cleanup_generated_login_hint
te_capture_service_state "$output_dir/raw/services-final.txt"
df -h / /data/cubelet >"$output_dir/summaries/disk-final.txt"
te_record_step "$output_dir" "$current_step" PASS 'exact task resources absent; bounded health checks completed'

current_step='manifest'
browser_phase manifest
te_record_step "$output_dir" "$current_step" PASS 'bounded manifest and artifact hashes written'

trap - EXIT INT TERM
printf 'PASS: real-cluster terminal evidence completed: %s\n' "$output_dir"
