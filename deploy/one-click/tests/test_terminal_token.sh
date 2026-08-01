#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck disable=SC1091
source "${ONE_CLICK_DIR}/lib/common.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

token_a="$(generate_terminal_internal_token)"
token_b="$(generate_terminal_internal_token)"
[[ "${#token_a}" -eq 64 && "${token_a}" =~ ^[0-9a-f]{64}$ ]] \
  || fail "generated token does not have the expected format"
[[ "${token_a}" != "${token_b}" ]] || fail "two generated tokens unexpectedly match"
if (validate_terminal_internal_token "too-short") >/dev/null 2>&1; then
  fail "short explicit token unexpectedly passed validation"
fi

token_file="${tmp_dir}/terminal-token"
write_terminal_internal_token_file "${token_file}" "${token_a}"
[[ "$(stat -c '%a' "${token_file}")" == "600" ]] || fail "token file mode is not 600"
[[ "$(<"${token_file}")" == "${token_a}" ]] || fail "token file content mismatch"

fresh_env="${tmp_dir}/fresh.env"
: >"${fresh_env}"
chmod 600 "${fresh_env}"
upsert_env_kv "${fresh_env}" CUBE_TERMINAL_INTERNAL_TOKEN "${token_a}"
[[ "$(stat -c '%a' "${fresh_env}")" == "600" ]] || fail "fresh env mode is not 600"
[[ "$(read_env_key "${fresh_env}" CUBE_TERMINAL_INTERNAL_TOKEN)" == "${token_a}" ]] \
  || fail "fresh token was not persisted"

new_example="${tmp_dir}/new.example"
old_runtime="${tmp_dir}/old.runtime"
old_baseline="${tmp_dir}/old.example"
merged_env="${tmp_dir}/merged.env"
diff_file="${tmp_dir}/diff.txt"
printf 'CUBE_TERMINAL_INTERNAL_TOKEN=\n' >"${new_example}"
printf 'CUBE_TERMINAL_INTERNAL_TOKEN=%s\n' "${token_a}" >"${old_runtime}"
printf 'CUBE_TERMINAL_INTERNAL_TOKEN=\n' >"${old_baseline}"

merge_env_three_way \
  "${new_example}" \
  "${old_runtime}" \
  "${old_baseline}" \
  "" \
  "${merged_env}" \
  "${diff_file}" >/dev/null

[[ "$(read_env_key "${merged_env}" CUBE_TERMINAL_INTERNAL_TOKEN)" == "${token_a}" ]] \
  || fail "upgrade did not preserve the existing token"
grep -q 'CUBE_TERMINAL_INTERNAL_TOKEN=\*\*\*REDACTED\*\*\*' "${diff_file}" \
  || fail "upgrade diff did not redact the token"
! grep -q -F "${token_a}" "${diff_file}" || fail "upgrade diff leaked the token"

remove_env_kv "${merged_env}" CUBE_TERMINAL_INTERNAL_TOKEN
! grep -q '^CUBE_TERMINAL_INTERNAL_TOKEN=' "${merged_env}" \
  || fail "shared runtime env retained the terminal token"

for start_script in cubemaster-start.sh cubeops-start.sh; do
  grep -q -F 'load_terminal_internal_token' \
    "${ONE_CLICK_DIR}/scripts/systemd/${start_script}" \
    || fail "${start_script} does not load the isolated shared token"
done
[[ "$(grep -R -l -F '.terminal-internal-token' "${ONE_CLICK_DIR}/scripts/systemd" | wc -l)" -eq 1 ]] \
  || fail "terminal token file is referenced outside the shared systemd helper"
grep -q -F 'write_terminal_internal_token_file' \
  "${ONE_CLICK_DIR}/install.sh" \
  || fail "install.sh does not persist the isolated shared token"
# This pattern intentionally contains literal shell expansions.
# shellcheck disable=SC2016
grep -q -F 'remove_env_kv "${RUNTIME_ENV_FILE}" "CUBE_TERMINAL_INTERNAL_TOKEN"' \
  "${ONE_CLICK_DIR}/install.sh" \
  || fail "install.sh does not remove the token from the shared runtime env"
grep -q -F '".terminal-internal-token"' "${ONE_CLICK_DIR}/lib/common.sh" \
  || fail "upgrade backup does not include the isolated token file"

echo "terminal token generation and upgrade preservation tests OK"
