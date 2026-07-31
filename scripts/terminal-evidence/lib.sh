#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

te_now() {
  date -u +'%Y-%m-%dT%H:%M:%SZ'
}

te_die() {
  printf 'FAIL: %s\n' "$*" >&2
  return 1
}

te_require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    te_die "required command is unavailable: $1"
    return 1
  }
}

te_validate_task_id() {
  local task_id=${1:-}
  [[ "$task_id" =~ ^[a-z0-9][a-z0-9-]{5,63}$ ]] ||
    {
      te_die 'task ID must be 6-64 lowercase letters, digits, or hyphens'
      return 1
    }
}

te_validate_endpoint() {
  local endpoint=${1:-}
  [[ "$endpoint" =~ ^https?://(\[[0-9A-Fa-f:]+\]|[A-Za-z0-9.-]+)(:([0-9]{1,5}))?/?$ ]] ||
    {
      te_die 'endpoint must be an http(s) origin without credentials, path, query, or fragment'
      return 1
    }
  local endpoint_port=${BASH_REMATCH[3]:-}
  if [[ -n "$endpoint_port" ]] &&
    ((10#$endpoint_port < 1 || 10#$endpoint_port > 65535)); then
    te_die 'endpoint port must be between 1 and 65535'
    return 1
  fi
  case "$endpoint" in
    http://localhost*|https://localhost*|http://127.*|https://127.*|http://\[::1\]*|https://\[::1\]*)
      te_die 'real-cluster endpoint must not be localhost'
      return 1
      ;;
  esac
}

te_reject_symlink_components() {
  local absolute_path=${1:-}
  [[ "$absolute_path" == /* ]] || {
    te_die 'path must be absolute'
    return 1
  }
  local current=''
  local component
  local -a components=()
  IFS='/' read -r -a components <<<"${absolute_path#/}"
  for component in "${components[@]}"; do
    [[ -n "$component" ]] || continue
    current="${current}/${component}"
    [[ ! -L "$current" ]] || {
      te_die "path must not contain a symbolic-link component: $current"
      return 1
    }
  done
}

te_validate_output_dir() {
  local output_dir=${1:-}
  local task_id=${2:-}
  [[ "$output_dir" == /data/cubelet/* ]] ||
    {
      te_die 'output directory must be an absolute task-owned path below /data/cubelet'
      return 1
    }
  [[ "$output_dir" != *'/../'* && "$output_dir" != *'/./'* && "$output_dir" != */.. ]] ||
    {
      te_die 'output directory must not contain dot traversal segments'
      return 1
    }
  [[ "$output_dir" == *"$task_id"* ]] ||
    {
      te_die 'output directory must contain the exact task ID'
      return 1
    }
  [[ "$output_dir" != /data/cubelet && "$output_dir" != /data/cubelet/ ]] ||
    {
      te_die 'output directory must not be the /data/cubelet root'
      return 1
    }
  te_reject_symlink_components "$output_dir" || return 1

  local existing_parent=$output_dir
  while [[ ! -e "$existing_parent" ]]; do
    existing_parent=${existing_parent%/*}
  done
  [[ -d "$existing_parent" ]] || {
    te_die 'nearest existing output parent must be a directory'
    return 1
  }
  local resolved_parent
  resolved_parent=$(CDPATH='' cd -- "$existing_parent" && pwd -P)
  [[ "$resolved_parent" == /data/cubelet || "$resolved_parent" == /data/cubelet/* ]] ||
    {
      te_die 'resolved output parent must remain below /data/cubelet'
      return 1
    }
}

te_init_output() {
  local output_dir=$1
  local resume_run=$2
  if [[ -e "$output_dir" && "$resume_run" != true ]]; then
    if find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
      te_die "output directory is not empty; use a new task ID or --resume-run: $output_dir"
    fi
  fi
  install -d -m 0700 \
    "$output_dir" \
    "$output_dir/raw" \
    "$output_dir/browser" \
    "$output_dir/browser-cache" \
    "$output_dir/browser-config" \
    "$output_dir/browser-tmp" \
    "$output_dir/screenshots" \
    "$output_dir/summaries" \
    "$output_dir/rollback"
  if [[ "$resume_run" != true || ! -f "$output_dir/steps.tsv" ]]; then
    : >"$output_dir/steps.tsv"
  fi
  chmod 0600 "$output_dir/steps.tsv"
}

te_record_step() {
  local output_dir=$1
  local step=$2
  local status=$3
  local detail=${4:-}
  detail=${detail//$'\n'/ }
  detail=${detail//$'\t'/ }
  printf '%s\t%s\t%s\t%s\n' "$(te_now)" "$step" "$status" "$detail" >>"$output_dir/steps.tsv"
}

te_run_step() {
  local output_dir=$1
  local step=$2
  local log_path=$3
  shift 3
  te_record_step "$output_dir" "$step" START 'command started'
  if "$@" >"$log_path" 2>&1; then
    te_record_step "$output_dir" "$step" PASS 'exit=0'
    return 0
  else
    local exit_code=$?
    te_record_step "$output_dir" "$step" FAIL "exit=$exit_code"
    return "$exit_code"
  fi
}

te_sensitive_count() {
  local target=$1
  local pattern=$2
  if [[ ! -e "$target" ]]; then
    printf '0\n'
    return 0
  fi
  LC_ALL=C rg -a -o --no-filename -- "$pattern" "$target" 2>/dev/null | wc -l || true
}

te_firewall_comment() {
  local task_id=$1
  te_validate_task_id "$task_id" || return 1
  printf 'cube-terminal-evidence:%s\n' "$task_id"
}

te_validate_credential_file() {
  local credential_file=$1
  [[ -f "$credential_file" ]] || {
    te_die "credential file is missing: $credential_file"
    return 1
  }
  local mode
  mode=$(stat -c '%a' "$credential_file")
  [[ "$mode" == 600 || "$mode" == 400 ]] ||
    {
      te_die 'credential file must be mode 0600 or 0400'
      return 1
    }
}

te_credential_username() {
  local credential_file=$1
  te_validate_credential_file "$credential_file" || return 1
  jq -er '.username | select(type == "string" and length > 0)' "$credential_file" >/dev/null ||
    {
      te_die 'credential file has no valid username'
      return 1
    }
  jq -r '.username' "$credential_file"
}

te_validate_credential_pair() {
  local primary_file=$1
  local secondary_file=$2
  te_validate_credential_file "$primary_file" || return 1
  te_validate_credential_file "$secondary_file" || return 1
  [[ $(stat -Lc '%d:%i' "$primary_file") != $(stat -Lc '%d:%i' "$secondary_file") ]] ||
    {
      te_die 'primary and secondary credentials must be distinct files'
      return 1
    }
  local primary_username secondary_username
  primary_username=$(te_credential_username "$primary_file")
  secondary_username=$(te_credential_username "$secondary_file")
  [[ "$primary_username" != "$secondary_username" ]] ||
    {
      te_die 'primary and secondary credentials must use distinct usernames'
      return 1
    }
}

te_validate_task_username() {
  local username=${1:-}
  [[ "$username" =~ ^[a-z0-9][a-z0-9-]{5,127}$ ]] ||
    {
      te_die 'task username must be 6-128 lowercase letters, digits, or hyphens'
      return 1
    }
}

te_mysql_from_file() {
  local sql_file=$1
  [[ -f "$sql_file" ]] || {
    te_die 'MySQL stdin file is missing'
    return 1
  }
  # shellcheck disable=SC2024
  # The caller owns the mode-0600 stdin file.
  sudo -n docker exec -i cube-sandbox-mysql sh -c '
    set -eu
    umask 077
    defaults_file=$(mktemp /tmp/c7e-mysql.XXXXXX)
    trap '\''rm -f -- "$defaults_file"'\'' EXIT HUP INT TERM
    printf "[client]\nuser=%s\npassword=%s\n" "${MYSQL_USER:?}" "${MYSQL_PASSWORD:?}" >"$defaults_file"
    mysql --defaults-extra-file="$defaults_file" \
      --protocol=socket \
      --database="${MYSQL_DATABASE:?}" \
      --batch \
      --skip-column-names \
      --raw
  ' <"$sql_file"
}

te_wait_cubeops_health() {
  local _attempt
  for _attempt in $(seq 1 40); do
    if systemctl is-active --quiet cube-sandbox-cubeops.service &&
      curl -fsS --connect-timeout 2 --max-time 3 -o /dev/null http://127.0.0.1:3010/health; then
      return 0
    fi
    sleep 1
  done
  te_die 'CubeOps did not become healthy within 40 seconds'
}

te_wait_cubelet_health() {
  local _attempt
  for _attempt in $(seq 1 60); do
    if systemctl is-active --quiet cube-sandbox-cubelet.service &&
      sudo -n test -S /data/cubelet/cubelet.sock &&
      [[ $(systemctl show cube-sandbox-cubelet.service -p MainPID --value) =~ ^[1-9][0-9]*$ ]]; then
      return 0
    fi
    sleep 1
  done
  te_die 'Cubelet did not become healthy within 60 seconds'
}

te_render_cubelet_idle_timeout() {
  local source_path=$1
  local idle_minutes=$2
  [[ -f "$source_path" ]] || {
    te_die 'Cubelet config source is missing'
    return 1
  }
  [[ "$idle_minutes" =~ ^[1-9][0-9]*$ ]] || {
    te_die 'Cubelet idle timeout must be a positive integer'
    return 1
  }

  awk -v idle_minutes="$idle_minutes" '
    function finish_section() {
      if (in_target && !found_key) {
        printf "      idle_timeout_minutes = %s\n", idle_minutes
        found_key = 1
      }
    }
    BEGIN {
      target = "[plugins.\"io.cubelet.cubebox-service.v1.cubebox-service\".terminal]"
      in_target = 0
      found_section = 0
      found_key = 0
    }
    /^[[:space:]]*\[/ {
      finish_section()
      normalized = $0
      sub(/^[[:space:]]*/, "", normalized)
      if (normalized == target) {
        in_target = 1
        found_section = 1
      } else {
        in_target = 0
      }
    }
    in_target && /^[[:space:]]*idle_timeout_minutes[[:space:]]*=/ {
      printf "      idle_timeout_minutes = %s\n", idle_minutes
      found_key = 1
      next
    }
    { print }
    END {
      finish_section()
      if (!found_section) {
        print ""
        print target
        printf "      idle_timeout_minutes = %s\n", idle_minutes
      }
    }
  ' "$source_path"
}

te_capture_service_state() {
  local output_path=$1
  local tmp_path="${output_path}.tmp.$$"
  install -d -m 0700 "$(dirname -- "$output_path")"
  systemctl show \
    cube-sandbox-cubeops.service \
    cube-sandbox-cubemaster.service \
    cube-sandbox-cubelet.service \
    cube-sandbox-network-agent.service \
    cube-sandbox-webui.service \
    cube-sandbox-coredns.service \
    -p MainPID \
    -p NRestarts \
    -p Id \
    -p ActiveState \
    -p SubState \
    -p FragmentPath \
    --no-pager >"$tmp_path"
  chmod 0600 "$tmp_path"
  mv -f "$tmp_path" "$output_path"
}

te_remove_exact_firewall_rule() {
  local task_id=$1
  local endpoint_ip=$2
  local endpoint_port=$3
  local comment
  comment=$(te_firewall_comment "$task_id")
  [[ "$endpoint_ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] ||
    {
      te_die 'firewall cleanup requires an IPv4 endpoint'
      return 1
    }
  [[ "$endpoint_port" =~ ^[0-9]+$ ]] || {
    te_die 'firewall cleanup requires a numeric port'
    return 1
  }

  local rule=(
    OUTPUT
    -p tcp
    -d "${endpoint_ip}/32"
    --dport "$endpoint_port"
    -m comment
    --comment "$comment"
    -j REJECT
    --reject-with tcp-reset
  )
  while sudo -n iptables -C "${rule[@]}" >/dev/null 2>&1; do
    sudo -n iptables -D "${rule[@]}"
  done
}

te_capture_failure_once() {
  local output_dir=$1
  local step=$2
  local exit_code=$3
  local failure_path="$output_dir/first-failure.tsv"
  [[ -s "$failure_path" ]] && return 0

  install -d -m 0700 "$output_dir/raw"
  printf '%s\t%s\tFAIL\texit=%s\n' "$(te_now)" "$step" "$exit_code" >"$failure_path"
  chmod 0600 "$failure_path"
  te_capture_service_state "$output_dir/raw/first-failure-services.txt" || true
  df -h / /data/cubelet >"$output_dir/raw/first-failure-disk.txt" 2>/dev/null || true
  chmod 0600 "$output_dir/raw/first-failure-disk.txt" 2>/dev/null || true

  {
    printf 'captured_at=%s\n' "$(te_now)"
    printf 'state_file_present=%s\n' "$([[ -f "$output_dir/state.json" ]] && printf true || printf false)"
    if [[ -f "$output_dir/state.json" ]]; then
      printf 'state_file_bytes=%s\n' "$(stat -c '%s' "$output_dir/state.json")"
      printf 'state_file_sha256=%s\n' "$(sha256sum "$output_dir/state.json" | awk '{print $1}')"
    fi
    printf 'task_browser_processes=%s\n' "$(ps -eo comm=,args= | awk -v root="$output_dir" '$1 ~ /^(chrome|chromium|node)$/ && index($0, root) { count++ } END { print count + 0 }')"
    printf 'terminal_journal_files=%s\n' "$(sudo -n find /data/cubelet/state/terminal-journal -type f -print 2>/dev/null | wc -l || true)"
    printf 'terminal_fifos=%s\n' "$(sudo -n find /data/cubelet/state/terminal-fifo -type p -print 2>/dev/null | wc -l || true)"
  } >"$output_dir/raw/first-failure-residue.txt"
  chmod 0600 "$output_dir/raw/first-failure-residue.txt"
}
