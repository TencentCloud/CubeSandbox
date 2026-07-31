#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../lib.sh
source "$TEST_DIR/../lib.sh"

failures=0
expect_pass() {
  local label=$1
  shift
  if "$@" >/dev/null 2>&1; then
    printf 'PASS: %s\n' "$label"
  else
    printf 'FAIL: %s\n' "$label" >&2
    failures=$((failures + 1))
  fi
}

expect_fail() {
  local label=$1
  shift
  if "$@" >/dev/null 2>&1; then
    printf 'FAIL: %s unexpectedly passed\n' "$label" >&2
    failures=$((failures + 1))
  else
    printf 'PASS: %s rejected\n' "$label"
  fi
}

test_task_id_good() { te_validate_task_id 'issue643-fixture-01'; }
test_task_id_bad() { te_validate_task_id '../../escape'; }
test_endpoint_good() { te_validate_endpoint 'http://172.19.0.2'; }
test_endpoint_localhost() { te_validate_endpoint 'http://127.0.0.1:12088'; }
test_endpoint_userinfo() { te_validate_endpoint 'https://user:secret@example.test'; }
test_endpoint_query() { te_validate_endpoint 'https://example.test?credential=fixture'; }
test_endpoint_fragment() { te_validate_endpoint 'https://example.test#fragment'; }
test_endpoint_bad_port() { te_validate_endpoint 'https://example.test:70000'; }
test_output_good() { te_validate_output_dir '/data/cubelet/evidence/issue643-fixture-01' 'issue643-fixture-01'; }
test_output_broad() { te_validate_output_dir '/data/cubelet' 'issue643-fixture-01'; }

for shell_file in "$TEST_DIR/../lib.sh" "$TEST_DIR/../terminal-evidence.sh" "$TEST_DIR/test.sh"; do
  if bash -n "$shell_file"; then
    printf 'PASS: shell syntax %s\n' "$(basename "$shell_file")"
  else
    printf 'FAIL: shell syntax %s\n' "$(basename "$shell_file")" >&2
    failures=$((failures + 1))
  fi
done

expect_pass 'valid task ID' test_task_id_good
expect_fail 'traversal task ID' test_task_id_bad
expect_pass 'non-localhost endpoint' test_endpoint_good
expect_fail 'localhost endpoint' test_endpoint_localhost
expect_fail 'endpoint userinfo' test_endpoint_userinfo
expect_fail 'endpoint query' test_endpoint_query
expect_fail 'endpoint fragment' test_endpoint_fragment
expect_fail 'endpoint out-of-range port' test_endpoint_bad_port
expect_pass 'task-owned output path' test_output_good
expect_fail 'broad output root' test_output_broad

fixture_dir=$(mktemp -d)
trap 'find "$fixture_dir" -depth -delete' EXIT
chmod 0700 "$fixture_dir"
mkdir "$fixture_dir/real-output"
ln -s "$fixture_dir/real-output" "$fixture_dir/output-link"
expect_fail 'symbolic-link output component' te_reject_symlink_components "$fixture_dir/output-link/run"
credential_a="$fixture_dir/credential-a.json"
credential_b="$fixture_dir/credential-b.json"
credential_same_user="$fixture_dir/credential-same-user.json"
credential_same_inode="$fixture_dir/credential-same-inode.json"
printf '%s\n' '{"username":"issue643-fixture-user-a"}' >"$credential_a"
printf '%s\n' '{"username":"issue643-fixture-user-b"}' >"$credential_b"
printf '%s\n' '{"username":"issue643-fixture-user-a"}' >"$credential_same_user"
ln "$credential_a" "$credential_same_inode"
chmod 0600 "$credential_a" "$credential_b" "$credential_same_user"
expect_pass 'distinct credential files and usernames' te_validate_credential_pair "$credential_a" "$credential_b"
expect_fail 'duplicate credential username' te_validate_credential_pair "$credential_a" "$credential_same_user"
expect_fail 'same credential inode' te_validate_credential_pair "$credential_a" "$credential_same_inode"

fixture_file="$fixture_dir/sensitive-fixture.txt"
printf '%s\n' 'Authorization: fixture' 'Cookie: fixture' 'safe line' >"$fixture_file"
chmod 0600 "$fixture_file"
authorization_count=$(te_sensitive_count "$fixture_file" 'Authorization:')
cookie_count=$(te_sensitive_count "$fixture_file" 'Cookie:')
if [[ "$authorization_count" == 1 && "$cookie_count" == 1 ]]; then
  printf 'PASS: sensitive scan returns counts only\n'
else
  printf 'FAIL: sensitive scan counts were unexpected\n' >&2
  failures=$((failures + 1))
fi

step_dir="$fixture_dir/step-run"
te_init_output "$step_dir" false
if te_run_step "$step_dir" fixture-failure "$step_dir/raw/fixture.log" bash -c 'exit 7'; then
  printf 'FAIL: failed child was promoted to success\n' >&2
  failures=$((failures + 1))
else
  step_status=$(tail -n 1 "$step_dir/steps.tsv")
  if [[ "$step_status" == *$'\tfixture-failure\tFAIL\texit=7' ]]; then
    printf 'PASS: failed child preserves exit 7 and stops the step\n'
  else
    printf 'FAIL: failed child status was not preserved\n' >&2
    failures=$((failures + 1))
  fi
fi

failure_dir="$fixture_dir/failure-capture"
te_init_output "$failure_dir" false
(
  te_capture_service_state() {
    printf 'fixture service state\n' >"$1"
    chmod 0600 "$1"
  }
  sudo() { return 0; }
  te_capture_failure_once "$failure_dir" browser-core 9
  te_capture_failure_once "$failure_dir" browser-cleanup 7
)
failure_row_count=$(wc -l <"$failure_dir/first-failure.tsv")
failure_row=$(cat "$failure_dir/first-failure.tsv")
if [[ "$failure_row_count" == 1 && "$failure_row" == *$'\tbrowser-core\tFAIL\texit=9' ]]; then
  printf 'PASS: first failure is preserved once without overwrite\n'
else
  printf 'FAIL: first failure preservation was not idempotent\n' >&2
  failures=$((failures + 1))
fi

entrypoint="$TEST_DIR/../terminal-evidence.sh"
browser_driver="$TEST_DIR/../browser.cjs"
capture_line=$(rg -n 'te_capture_failure_once .*current_step' "$entrypoint" | cut -d: -f1)
cleanup_line=$(rg -n 'if ! cleanup_resources' "$entrypoint" | cut -d: -f1)
if [[ -n "$capture_line" && -n "$cleanup_line" && "$capture_line" -lt "$cleanup_line" ]]; then
  printf 'PASS: failure capture precedes best-effort cleanup\n'
else
  printf 'FAIL: failure capture does not precede cleanup\n' >&2
  failures=$((failures + 1))
fi

if rg -q 'browser_phase cleanup \|\| true' "$entrypoint"; then
  printf 'FAIL: cleanup failure is still silently swallowed\n' >&2
  failures=$((failures + 1))
else
  printf 'PASS: cleanup failures remain visible\n'
fi

if rg -q 'cube-terminal-evidence:\$\{taskId\}' "$browser_driver" &&
  rg -q -- '--direct-db-task-users' "$entrypoint" &&
  rg -q 'TE_SECONDARY_CREDENTIAL_FILE' "$entrypoint" &&
  rg -q 'TE_REQUIRE_MULTI_CONTAINER' "$entrypoint" &&
  rg -q 'PASS_TWO_USERS_TWO_INSTANCES_FOUR_SESSIONS' "$browser_driver" &&
  rg -q "'SKIP_SINGLE_CONTAINER_TEMPLATE'" "$browser_driver"; then
  printf 'PASS: task firewall identity and real two-user/multi-container modes are guarded\n'
else
  printf 'FAIL: task identity or real matrix guards are missing\n' >&2
  failures=$((failures + 1))
fi

mysql_stdin=$(sed -n '/^te_mysql_from_file(/,/^}/p' "$TEST_DIR/../lib.sh")
task_user_cleanup=$(sed -n '/^cleanup_direct_db_task_users(/,/^}/p' "$entrypoint")
if rg -F -q "docker exec -i cube-sandbox-mysql" <<<"$mysql_stdin" &&
  rg -F -q -- '--defaults-extra-file="$defaults_file"' <<<"$mysql_stdin" &&
  ! rg -F -q 'MYSQL_PWD' <<<"$mysql_stdin" &&
  ! rg -F -q -- '--execute' <<<"$mysql_stdin" &&
  rg -F -q "WHERE username IN ('\$task_username_a','\$task_username_b')" <<<"$task_user_cleanup" &&
  rg -F -q "WHERE user_id IN ('\$task_username_a','\$task_username_b')" <<<"$task_user_cleanup" &&
  ! rg -q 'DELETE FROM terminal_sessions' <<<"$task_user_cleanup"; then
  printf 'PASS: direct DB users use stdin-only auth and exact non-destructive cleanup predicates\n'
else
  printf 'FAIL: direct DB user SQL/auth cleanup guard is unsafe or missing\n' >&2
  failures=$((failures + 1))
fi

if rg -q "const reviewArtifactPaths = \[" "$browser_driver" &&
  rg -q "'browser/core.json'" "$browser_driver" &&
  rg -q "'summaries/cleanup-health.json'" "$browser_driver"; then
  printf 'PASS: bounded review artifacts use an explicit allowlist\n'
else
  printf 'FAIL: bounded review artifact allowlist is missing\n' >&2
  failures=$((failures + 1))
fi

if rg -F -q "'/data/cubelet/cubelet.sock'" "$browser_driver" &&
  rg -F -q "'--address'" "$browser_driver"; then
  printf 'PASS: runtime residue checks use the Cubelet containerd socket\n'
else
  printf 'FAIL: runtime residue checks do not target the Cubelet containerd socket\n' >&2
  failures=$((failures + 1))
fi

network_query=$(sed -n '/^function networkResourceCounts(/,/^}/p' "$browser_driver")
if rg -q "^[[:space:]]*'sudo',$" <<<"$network_query" &&
  rg -q "^[[:space:]]*'-n',$" <<<"$network_query" &&
  rg -q "^[[:space:]]*'curl',$" <<<"$network_query"; then
  printf 'PASS: privileged network baseline query is explicit\n'
else
  printf 'FAIL: network baseline query cannot access the root-owned socket\n' >&2
  failures=$((failures + 1))
fi

network_cleanup=$(sed -n '/^async function ensureTaskNetworksReleased(/,/^}/p' "$browser_driver")
network_release=$(sed -n '/^async function releaseExactManagedNetwork(/,/^}/p' "$browser_driver")
if rg -F -q 'const deadline = Date.now() + 10_000' <<<"$network_cleanup" &&
  rg -F -q 'releaseExactManagedNetwork(sandboxId)' <<<"$network_cleanup" &&
  rg -F -q 'task network-agent record remained after exact cleanup' <<<"$network_cleanup" &&
  rg -F -q '/data/cubelet/network-agent/state/${sandboxId}.json' <<<"$network_release" &&
  rg -F -q 'http://localhost/v1/network/release' <<<"$network_release" &&
  rg -F -q "'jq'" <<<"$network_release" &&
  rg -F -q "'@-'" <<<"$network_release" &&
  rg -F -q 'input: request.stdout' <<<"$network_release" &&
  ! rg -F -q "      request.stdout," <<<"$network_release" &&
  ! rg -F -q "'bash', '-c'" <<<"$network_release" &&
  rg -F -q 'terminal-evidence:' <<<"$network_release"; then
  printf 'PASS: cleanup waits for normal network convergence then releases exact task IDs only\n'
else
  printf 'FAIL: exact task-owned network cleanup fallback is missing\n' >&2
  failures=$((failures + 1))
fi

websocket_probe=$(sed -n '/^async function attemptWebSocket(/,/^}/p' "$browser_driver")
if rg -q 'let opened = false' <<<"$websocket_probe" &&
  rg -F -q 'socket.onerror = () => {' <<<"$websocket_probe" &&
  ! rg -q 'socket.onerror = .*finish' <<<"$websocket_probe" &&
  rg -F -q 'socket.onclose = (event) => finish({' <<<"$websocket_probe"; then
  printf 'PASS: WebSocket close code wins over the preceding error event\n'
else
  printf 'FAIL: WebSocket probe can lose the protocol close code\n' >&2
  failures=$((failures + 1))
fi

if rg -F -q 'new Uint8Array(65_538)' <<<"$websocket_probe" &&
  rg -F -q 'frame[0] = 0x00' <<<"$websocket_probe" &&
  ! rg -F -q 'new Uint8Array(65_537)' <<<"$websocket_probe"; then
  printf 'PASS: oversized probe exceeds the payload limit plus channel byte\n'
else
  printf 'FAIL: oversized probe is not beyond the 64 KiB payload boundary\n' >&2
  failures=$((failures + 1))
fi

grant_query=$(rg -n 'const grantSQL = ' "$browser_driver")
if rg -F -q '\`cols\`,\`rows\`' <<<"$grant_query" &&
  ! rg -F -q ',cols,rows,' <<<"$grant_query"; then
  printf 'PASS: MySQL audit query quotes terminal dimension identifiers\n'
else
  printf 'FAIL: MySQL audit query leaves a reserved dimension identifier unquoted\n' >&2
  failures=$((failures + 1))
fi

audit_wait=$(sed -n '/^async function waitForTaskSessionsClosed(/,/^}/p' "$browser_driver")
if rg -F -q 'closed_at IS NULL' <<<"$audit_wait" &&
  rg -F -q 'timeoutMs = 120_000' <<<"$audit_wait" &&
  rg -F -q 'await new Promise' <<<"$audit_wait" &&
  ! rg -q 'UPDATE|DELETE|token_hash' <<<"$audit_wait"; then
  printf 'PASS: audit waits boundedly for natural payload-free session closure\n'
else
  printf 'FAIL: audit close-convergence wait is unsafe or missing\n' >&2
  failures=$((failures + 1))
fi

if rg -F -q '/Cubelet/config/config.toml' "$entrypoint" &&
  rg -F -q 'io.cubelet.cubebox-service.v1.cubebox-service\".terminal' "$TEST_DIR/../lib.sh" &&
  rg -F -q 'systemctl restart cube-sandbox-cubelet.service' "$entrypoint" &&
  rg -F -q 'te_wait_cubelet_health' "$entrypoint" &&
  ! sed -n '/^current_step='\''shorten-idle-timeout'\''/,/^browser_phase idle/p' "$entrypoint" | rg -q 'cubeops|one-click.env'; then
  printf 'PASS: idle probe changes and restarts the Cubelet-owned timer config\n'
else
  printf 'FAIL: idle probe does not target the Cubelet-owned timer config\n' >&2
  failures=$((failures + 1))
fi

cubelet_config_fixture="$fixture_dir/cubelet-config.toml"
cubelet_config_rendered="$fixture_dir/cubelet-config.rendered.toml"
printf '%s\n' \
  '[plugins."io.cubelet.cubebox-service.v1.cubebox-service"]' \
  '  destroy_dead_line = "60s"' \
  '[plugins."io.cubelet.images-service.v1.images-service"]' >"$cubelet_config_fixture"
te_render_cubelet_idle_timeout "$cubelet_config_fixture" 1 >"$cubelet_config_rendered"
if [[ $(rg -c '^\[plugins\."io\.cubelet\.cubebox-service\.v1\.cubebox-service"\.terminal\]$' "$cubelet_config_rendered") == 1 ]] &&
  [[ $(rg -c '^[[:space:]]*idle_timeout_minutes[[:space:]]*=[[:space:]]*1$' "$cubelet_config_rendered") == 1 ]] &&
  rg -F -q '[plugins."io.cubelet.images-service.v1.images-service"]' "$cubelet_config_rendered"; then
  printf 'PASS: idle config renderer appends one bounded Cubelet terminal section\n'
else
  printf 'FAIL: idle config renderer did not append the expected terminal section\n' >&2
  failures=$((failures + 1))
fi
mv -f "$cubelet_config_rendered" "$cubelet_config_fixture"
te_render_cubelet_idle_timeout "$cubelet_config_fixture" 2 >"$cubelet_config_rendered"
if [[ $(rg -c '^\[plugins\."io\.cubelet\.cubebox-service\.v1\.cubebox-service"\.terminal\]$' "$cubelet_config_rendered") == 1 ]] &&
  [[ $(rg -c '^[[:space:]]*idle_timeout_minutes[[:space:]]*=[[:space:]]*2$' "$cubelet_config_rendered") == 1 ]] &&
  ! rg -q 'idle_timeout_minutes[[:space:]]*=[[:space:]]*1' "$cubelet_config_rendered"; then
  printf 'PASS: idle config renderer replaces the existing value without duplication\n'
else
  printf 'FAIL: idle config renderer duplicated or failed to replace the value\n' >&2
  failures=$((failures + 1))
fi

concurrency_probe=$(sed -n '/^async function concurrency(/,/^}/p' "$browser_driver")
security_probe=$(sed -n '/^async function security(/,/^}/p' "$browser_driver")
if rg -F -q "state.createdSandboxIds.length < 2" <<<"$concurrency_probe" &&
  rg -F -q "'cube-terminal-evidence-ordinal': '2'" <<<"$concurrency_probe" &&
  rg -F -q 'secondarySandboxCleanup:' <<<"$concurrency_probe" &&
  rg -F -q 'second sandbox remained after the concurrency phase' <<<"$concurrency_probe" &&
  rg -F -q 'const nonRunningSandboxId = sandboxId' <<<"$security_probe" &&
  rg -F -q 'capturedDuringCorePause: true' <<<"$security_probe" &&
  ! rg -F -q '/pause' <<<"$security_probe"; then
  printf 'PASS: second sandbox is created only for concurrency and then exactly removed\n'
else
  printf 'FAIL: bounded two-instance resource lifecycle is missing\n' >&2
  failures=$((failures + 1))
fi

core_probe=$(sed -n '/^async function core(/,/^}/p' "$browser_driver")
audit_probe=$(sed -n '/^async function auditCorrelation(/,/^}/p' "$browser_driver")
if [[ $(rg -F -c 'await browser.newContext' <<<"$concurrency_probe") == 2 ]] &&
  rg -F -q 'login(pages[0], credentialFile)' <<<"$concurrency_probe" &&
  rg -F -q 'login(pages[2], secondaryCredentialFile)' <<<"$concurrency_probe" &&
  rg -F -q 'const assignments = [sandboxIds[0], sandboxIds[1], sandboxIds[0], sandboxIds[1]]' <<<"$concurrency_probe" &&
  rg -F -q 'next.concurrencySessions' <<<"$concurrency_probe" &&
  rg -F -q "next.phases.concurrency = { status: 'PASS'" <<<"$concurrency_probe"; then
  printf 'PASS: concurrency uses two isolated contexts and two users across both instances\n'
else
  printf 'FAIL: two-user x two-instance browser isolation is incomplete\n' >&2
  failures=$((failures + 1))
fi

if rg -F -q 'if (requireMultiContainer)' <<<"$core_probe" &&
  rg -F -q 'primaryRoleMarker' <<<"$core_probe" &&
  rg -F -q 'sidecarRoleMarker' <<<"$core_probe" &&
  rg -F -q 'secondMetadata.containerId !== initialMetadata.containerId' <<<"$core_probe"; then
  printf 'PASS: required real multi-container binding checks distinct IDs and both role markers\n'
else
  printf 'FAIL: required real multi-container binding checks are incomplete\n' >&2
  failures=$((failures + 1))
fi

if rg -F -q '/data/log/CubeOps/cubeops-req.log' <<<"$audit_probe" &&
  rg -F -q '/data/log/CubeMaster/cubemaster-req.log' <<<"$audit_probe" &&
  rg -F -q '/data/log/Cubelet/Cubelet-req.log' <<<"$audit_probe" &&
  rg -F -q 'concurrencyUserMapping' <<<"$audit_probe" &&
  rg -F -q "'raw', 'audit-correlation-attempt.json'" <<<"$audit_probe" &&
  ! rg -F -q "boundedJournal('cube-sandbox-cubemaster.service'" <<<"$audit_probe"; then
  printf 'PASS: audit uses real runtime log sinks, user mapping, and preserves correlation attempts\n'
else
  printf 'FAIL: audit correlation still uses the wrong sink or lacks preserved user mapping\n' >&2
  failures=$((failures + 1))
fi

if ((failures > 0)); then
  exit 1
fi
printf 'PASS: terminal evidence fixture tests\n'
