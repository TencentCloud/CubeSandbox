#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Behavioral tests for cube-egress-net-start.sh (issue #2141): the wrapper
# must wait (bounded) for the ingress interface (cube-dev), fail explicitly
# on timeout, and only then exec the init script, passing its exit status
# through to systemd.
#
# Fully offline: the real wrapper is copied into a throwaway layout with a
# stub common.sh (bypasses require_root, points TOOLBOX_ROOT at the
# fixture), `ip` is a PATH-injected fake whose "link show" succeeds only
# while a flag file exists, and the init script is a recording fake whose
# exit code is controlled by another file.

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

# --- 2: no init script anywhere → hard error, not a silent exit 0 -----------
# (the fixture's scripts/cube-egress dir is still empty here)
if run_wrapper 2>"${TMP}/err"; then
    fail "wrapper exited 0 with no init script present"
fi
grep -q "no egress iptables init script" "${TMP}/err" \
    || fail "missing-init error does not mention the init script"

# --- 3: explicit CUBE_EGRESS_NET_INIT pointing at a missing file → the
# ensure_executable error, distinct from the auto-detect miss above
if CUBE_EGRESS_NET_INIT="${TMP}/no-such-init.sh" run_wrapper 2>"${TMP}/err"; then
    fail "wrapper exited 0 with a nonexistent CUBE_EGRESS_NET_INIT"
fi
grep -q "required executable not found" "${TMP}/err" \
    || fail "explicit-path miss did not hit ensure_executable"

# --- 3b: non-numeric wait/interval values are rejected up front -------------
if CUBE_EGRESS_NET_WAIT_TIMEOUT=abc run_wrapper 2>/dev/null; then
    fail "wrapper accepted a non-numeric CUBE_EGRESS_NET_WAIT_TIMEOUT"
fi

# Now place the fake init script (records the action, exit code from file).
cat > "${LAYOUT}/scripts/cube-egress/cube-egress-iptables-init.sh" <<EOF
#!/usr/bin/env bash
echo "init \$*" >> "${INIT_LOG}"
if [[ -f "${TMP}/init.exit" ]]; then
    exit "\$(cat "${TMP}/init.exit")"
fi
exit 0
EOF
chmod +x "${LAYOUT}/scripts/cube-egress/cube-egress-iptables-init.sh"

# --- 4: interface already present → install immediately ---------------------
: > "${INIT_LOG}"
touch "${TMP}/present"
run_wrapper 2>"${TMP}/out" || fail "wrapper failed with interface present"
[[ "$(cat "${INIT_LOG}")" == "init up" ]] \
    || fail "init script not exec'd with 'up' (got: $(cat "${INIT_LOG}"))"
grep -q '^ip link show dev cube-dev$' "${IP_LOG}" \
    || fail "wrapper never probed for cube-dev"

# --- 5: core #2141 scenario — absent then present ----------------------------
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

# --- 6: interface never appears → explicit timeout failure -------------------
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

# --- 7: init failure is propagated (systemd must see it and retry) -----------
: > "${INIT_LOG}"
touch "${TMP}/present"
echo 7 > "${TMP}/init.exit"
set +e
run_wrapper 2>"${TMP}/out"
STATUS=$?
set -e
[[ "${STATUS}" -eq 7 ]] \
    || fail "init exit status not propagated (got ${STATUS}, want 7)"
rm -f "${TMP}/init.exit"

# --- 8: unit file shape -------------------------------------------------------
grep -q '^Type=oneshot$' "${UNIT_SRC}" || fail "unit is not Type=oneshot"
# Type=oneshot disables the start timeout by default; an explicit
# TimeoutStartSec would only invite a wrong "90s kills the wait" reading.
grep -q '^TimeoutStartSec' "${UNIT_SRC}" \
    && fail "unit sets TimeoutStartSec — redundant for oneshot and misleading"
grep -q '^Restart=on-failure$' "${UNIT_SRC}" \
    || fail "unit lacks Restart=on-failure (a failed init would stay dead)"
grep -q '^StartLimitIntervalSec=60s$' "${UNIT_SRC}" \
    || fail "unit lacks StartLimitIntervalSec (default limit never triggers with RestartSec=5s)"
grep -q '^StartLimitBurst=3$' "${UNIT_SRC}" \
    || fail "unit lacks StartLimitBurst"
grep -q '^ExecStart=.*cube-egress-net-start\.sh$' "${UNIT_SRC}" \
    || fail "unit ExecStart does not point at the start wrapper"
grep -q '^ExecStop=.*cube-egress-net-stop\.sh$' "${UNIT_SRC}" \
    || fail "unit ExecStop no longer tears the rules down"

echo "test_egress_net_start: all scenarios passed"
