#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Behavioral tests for cube-egress-net-start.sh (issue #2141): the wrapper
# must validate its tuning knobs up front, wait (bounded) for the ingress
# interface (cube-dev), fail explicitly on timeout, and run the init script
# with in-wrapper retries, leaving the unit failed only after the retries
# are exhausted.

set -euo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
START_SH_SRC="${TESTS_DIR}/../scripts/systemd/cube-egress-net-start.sh"
UNIT_SRC="${TESTS_DIR}/../systemd/cube-sandbox-cube-egress-net.service"

fail() { echo "FAIL: $*" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# --- fixture ----------------------------------------------------------------
LAYOUT="${TMP}/toolbox"
mkdir -p "${LAYOUT}/scripts/systemd" "${LAYOUT}/scripts/cube-egress" "${TMP}/bin"
cp "${START_SH_SRC}" "${LAYOUT}/scripts/systemd/cube-egress-net-start.sh"

# Stub common.sh: same contract the wrapper relies on (TOOLBOX_ROOT, log,
# die, require_root, ensure_executable) without the root requirement.
cat > "${LAYOUT}/scripts/systemd/common.sh" <<EOF
TOOLBOX_ROOT="${LAYOUT}"
log() { echo "[stub] \$*" >&2; }
die() { echo "[stub] FATAL: \$*" >&2; exit 1; }
require_root() { :; }
ensure_executable() { [[ -x "\$1" ]] || die "required executable not found: \$1"; }
EOF

# Fake ip: "link show dev <iface>" succeeds only while the flag file exists.
cat > "${TMP}/bin/ip" <<EOF
#!/usr/bin/env bash
echo "ip \$*" >> "${TMP}/ip.log"
if [[ "\$1 \$2" == "link show" && -e "${TMP}/present" ]]; then
    exit 0
fi
exit 1
EOF
chmod +x "${TMP}/bin/ip"

IP_LOG="${TMP}/ip.log"
INIT_LOG="${TMP}/init.log"
: > "${IP_LOG}"

# Runs the wrapper against the fixture. Extra CUBE_* variables can be
# passed as command-scoped assignments (bash puts them in the environment).
run_wrapper() {
    timeout 20 env PATH="${TMP}/bin:/usr/bin:/bin" \
        CUBE_EGRESS_NET_POLL_INTERVAL=1 \
        "${LAYOUT}/scripts/systemd/cube-egress-net-start.sh"
}

# --- 1: syntax ---------------------------------------------------------------
bash -n "${START_SH_SRC}" || fail "cube-egress-net-start.sh fails bash -n"

# --- 2: bad tuning values are rejected up front, naming the variable ----------
# These run BEFORE init-script resolution, so they are reachable even with
# no init script installed (the fixture dir is still empty here) — deleting
# the numeric validation in the wrapper must turn these red.
for bad in "CUBE_EGRESS_NET_WAIT_TIMEOUT=abc" \
           "CUBE_EGRESS_NET_WAIT_TIMEOUT=-1" \
           "CUBE_EGRESS_NET_POLL_INTERVAL=0" \
           "CUBE_EGRESS_NET_INIT_RETRIES=0"; do
    if env "${bad}" timeout 20 env PATH="${TMP}/bin:/usr/bin:/bin" \
            "${LAYOUT}/scripts/systemd/cube-egress-net-start.sh" \
            2>"${TMP}/err"; then
        fail "wrapper accepted bad value: ${bad}"
    fi
    grep -q "must be a" "${TMP}/err" \
        || fail "bad value ${bad} did not produce a validation error (got: $(cat "${TMP}/err"))"
done
# The error must name the offending variable (bot review: name the knob).
if CUBE_EGRESS_NET_WAIT_TIMEOUT=abc run_wrapper 2>"${TMP}/err"; then
    fail "wrapper accepted a non-numeric CUBE_EGRESS_NET_WAIT_TIMEOUT"
fi
grep -q "CUBE_EGRESS_NET_WAIT_TIMEOUT" "${TMP}/err" \
    || fail "validation error does not name CUBE_EGRESS_NET_WAIT_TIMEOUT"

# --- 3: no init script anywhere → hard error, not a silent exit 0 -----------
# (still with default-good tuning values, so this is the init-resolution
# failure, distinct from the validation failures above)
if run_wrapper 2>"${TMP}/err"; then
    fail "wrapper exited 0 with no init script present"
fi
grep -q "no egress iptables init script" "${TMP}/err" \
    || fail "missing-init error does not mention the init script"

# --- 4: explicit CUBE_EGRESS_NET_INIT pointing at a missing file → the
# ensure_executable error, distinct from the auto-detect miss above
if CUBE_EGRESS_NET_INIT="${TMP}/no-such-init.sh" run_wrapper 2>"${TMP}/err"; then
    fail "wrapper exited 0 with a nonexistent CUBE_EGRESS_NET_INIT"
fi
grep -q "required executable not found" "${TMP}/err" \
    || fail "explicit-path miss did not hit ensure_executable"

# Now place the fake init script: records each invocation; exit code from
# init.exit if present, but succeeds from the Nth call on when
# init.succeed_after=<N> exists (for retry scenarios).
cat > "${LAYOUT}/scripts/cube-egress/cube-egress-iptables-init.sh" <<EOF
#!/usr/bin/env bash
echo "init \$*" >> "${INIT_LOG}"
n="\$(wc -l < "${INIT_LOG}")"
if [[ -f "${TMP}/init.succeed_after" ]] && (( n >= \$(cat "${TMP}/init.succeed_after") )); then
    exit 0
fi
if [[ -f "${TMP}/init.exit" ]]; then
    exit "\$(cat "${TMP}/init.exit")"
fi
exit 0
EOF
chmod +x "${LAYOUT}/scripts/cube-egress/cube-egress-iptables-init.sh"

# --- 5: interface already present → install immediately ---------------------
: > "${INIT_LOG}"
touch "${TMP}/present"
run_wrapper 2>"${TMP}/out" || fail "wrapper failed with interface present"
[[ "$(cat "${INIT_LOG}")" == "init up" ]] \
    || fail "init script not run with 'up' (got: $(cat "${INIT_LOG}"))"
grep -q '^ip link show dev cube-dev$' "${IP_LOG}" \
    || fail "wrapper never probed for cube-dev"

# --- 6: core #2141 scenario — absent then present ----------------------------
# The interface appears 0.3s in; with a 1s poll the wrapper must probe at
# least twice (i.e. actually waited), call the init script only after the
# interface exists, and exit 0.
: > "${INIT_LOG}"
rm -f "${TMP}/present"
( sleep 0.3; touch "${TMP}/present" ) &
TIMER_PID=$!
run_wrapper 2>"${TMP}/out" || fail "wrapper failed on absent→present interface"
wait "${TIMER_PID}"
PROBES="$(grep -c '^ip link show dev cube-dev$' "${IP_LOG}")"
[[ "${PROBES}" -ge 2 ]] \
    || fail "expected the wrapper to re-probe while waiting, got ${PROBES}"
[[ "$(cat "${INIT_LOG}")" == "init up" ]] \
    || fail "init script not called after interface appeared"

# --- 7: interface never appears → explicit timeout failure -------------------
# The core #2141 behavior: never exit 0 while the interface is missing (the
# pre-fix wrapper gave up after 30 attempts and "succeeded"). With a 1s
# budget and no `present` flag the wrapper must die loudly; the pre-fix
# "poll 30 times then exit 0" mutant turns this red.
: > "${INIT_LOG}"
rm -f "${TMP}/present"
set +e
CUBE_EGRESS_NET_WAIT_TIMEOUT=1 run_wrapper 2>"${TMP}/out"
STATUS=$?
set -e
[[ "${STATUS}" -ne 0 ]] || fail "wrapper exited 0 although the interface never appeared"
grep -q "timed out after 1s" "${TMP}/out" \
    || fail "timeout failure does not state the deadline"
[[ ! -s "${INIT_LOG}" ]] || fail "init script ran although the interface never appeared"

# --- 8: init retries live in the wrapper -------------------------------------
touch "${TMP}/present"

# 8a: persistent init failure → retried exactly INIT_RETRIES times, then the
# wrapper dies with a visible message (unit goes failed; systemd sees
# non-zero, NOT the init script's own code — the wrapper owns the outcome).
: > "${INIT_LOG}"
echo 7 > "${TMP}/init.exit"
set +e
CUBE_EGRESS_NET_INIT_RETRIES=2 run_wrapper 2>"${TMP}/out"
STATUS=$?
set -e
[[ "${STATUS}" -ne 0 ]] || fail "wrapper exited 0 although init kept failing"
[[ "$(wc -l < "${INIT_LOG}")" -eq 2 ]] \
    || fail "expected exactly 2 init attempts, got $(wc -l < "${INIT_LOG}")"
grep -q "failed 2 time" "${TMP}/out" \
    || fail "retry exhaustion message missing (got: $(cat "${TMP}/out"))"
rm -f "${TMP}/init.exit"

# 8b: transient failure (fails once, succeeds on the 2nd call) → wrapper
# recovers in-wrapper and exits 0; the pre-retry mutant (exec-once) is red.
: > "${INIT_LOG}"
echo 1 > "${TMP}/init.exit"
echo 2 > "${TMP}/init.succeed_after"
run_wrapper 2>"${TMP}/out" || fail "wrapper did not recover from a transient init failure"
[[ "$(wc -l < "${INIT_LOG}")" -eq 2 ]] \
    || fail "expected 2 init attempts (fail then succeed), got $(wc -l < "${INIT_LOG}")"
rm -f "${TMP}/init.exit" "${TMP}/init.succeed_after"

# --- 9: unit file shape -------------------------------------------------------
grep -q '^Type=oneshot$' "${UNIT_SRC}" || fail "unit is not Type=oneshot"
# Type=oneshot disables the start timeout by default; an explicit
# TimeoutStartSec would only invite a wrong "90s kills the wait" reading.
grep -q '^TimeoutStartSec' "${UNIT_SRC}" \
    && fail "unit sets TimeoutStartSec — redundant for oneshot and misleading"
# No Restart=/StartLimit: Restart= on Type=oneshot is a load-time refusal on
# systemd < 244 (RHEL/CentOS/OpenCloudOS 8 ship 239) — such a unit would
# never run there at all. Retries live in the wrapper instead.
if grep -q '^Restart=' "${UNIT_SRC}"; then
    fail "unit sets Restart= — refused at load time on systemd < 244; keep retries in the wrapper"
fi
if grep -q '^StartLimit' "${UNIT_SRC}"; then
    fail "unit sets StartLimit* without Restart= — meaningless now that retries are in-wrapper"
fi
grep -q '^ExecStart=.*cube-egress-net-start\.sh$' "${UNIT_SRC}" \
    || fail "unit ExecStart does not point at the start wrapper"
grep -q '^ExecStop=.*cube-egress-net-stop\.sh$' "${UNIT_SRC}" \
    || fail "unit ExecStop no longer tears the rules down"

echo "test_egress_net_start: all scenarios passed"
