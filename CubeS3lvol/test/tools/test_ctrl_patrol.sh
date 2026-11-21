#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Offline tests for the NVMe controller patrol in rcow_common.sh (#2288):
# a synthetic /sys/class/nvme, a mock `nvme` on PATH, and a controllable clock.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

export MOCK_SYS="${tmp}/sys"
export MOCK_LOG="${tmp}/nvme.log"
export MOCK_FAIL="${tmp}/nvme.fail"
export MOCK_SEQ="${tmp}/nvme.seq"
mkdir -p "${MOCK_SYS}" "${tmp}/bin" "${tmp}/run"
: >"${MOCK_LOG}"
echo 0 >"${MOCK_SEQ}"

cat >"${tmp}/bin/nvme" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"${MOCK_LOG}"
cmd="$1"; shift
nqn=""
while [ $# -gt 0 ]; do
	[ "$1" = "-n" ] && nqn="$2"
	shift
done
case "${cmd}" in
connect)
	[ -e "${MOCK_FAIL}" ] && exit 1
	seq=$(( $(cat "${MOCK_SEQ}") + 1 ))
	echo "${seq}" >"${MOCK_SEQ}"
	d="${MOCK_SYS}/nvme$((100 + seq))"
	mkdir -p "${d}"
	echo "${nqn}" >"${d}/subsysnqn"
	echo live >"${d}/state"
	;;
disconnect)
	for d in "${MOCK_SYS}"/nvme*; do
		[ -d "${d}" ] || continue
		[ "$(cat "${d}/subsysnqn")" = "${nqn}" ] && rm -rf "${d}"
	done
	;;
esac
exit 0
EOF
chmod +x "${tmp}/bin/nvme"
export PATH="${tmp}/bin:${PATH}"

RCOW_LVS_NAME=patrol-test
RCOW_RUN_DIR="${tmp}/run"
RCOW_NVME_SYSFS_ROOT="${MOCK_SYS}"
RCOW_NUM_SUBSYS=4
RCOW_PATROL_BACKOFF_MIN=5
RCOW_PATROL_BACKOFF_MAX=20
RCOW_PATROL_MAX_PER_ROUND=2
RCOW_PATROL_CONNECT_TIMEOUT=5
# shellcheck source=../../scripts/rcow_common.sh
source "${ROOT}/scripts/rcow_common.sh"

# The target and the clock are the two things a test cannot have for real.
FAKE_PID=4242
FAKE_NOW=1000
rcow_target_pid() { printf '%s' "${FAKE_PID}"; }
rcow_patrol_now() { printf '%s' "${FAKE_NOW}"; }

passed=0
failed=0
check() {
	local name="$1"
	shift
	if "$@"; then
		printf '\t[PASS] %s\n' "${name}"
		passed=$((passed + 1))
	else
		printf '\t[FAIL] %s\n' "${name}"
		failed=$((failed + 1))
	fi
}

ctrl() {	# ctrl <index> <nqn-number> <state>
	local d="${MOCK_SYS}/nvme$1"
	mkdir -p "${d}"
	rcow_nqn "$2" >"${d}/subsysnqn"
	echo >>"${d}/subsysnqn"
	echo "$3" >"${d}/state"
}
reset_world() {
	rm -rf "${MOCK_SYS:?}"/nvme* "${RCOW_PATROL_HOLD}" "${MOCK_FAIL}"
	: >"${MOCK_LOG}"
	RCOW_PATROL_DUE=() RCOW_PATROL_WAIT=() RCOW_PATROL_FAILS=()
	FAKE_NOW=1000
	echo "${FAKE_PID}" >"${RCOW_START_READY}"
	local i
	for i in 0 1 2 3; do ctrl "${i}" "${i}" live; done
}
connects() { grep -c '^connect' "${MOCK_LOG}" || true; }
connected_to() { grep -q "^connect .*-n $(rcow_nqn "$1")\b" "${MOCK_LOG}"; }
round() { rcow_patrol_controllers >>"${tmp}/patrol.out" 2>&1; }

echo "=== test_ctrl_patrol ==="

reset_world
round
check "a complete grid is left alone" [ "$(connects)" -eq 0 ]

reset_world
rm -rf "${MOCK_SYS}/nvme2"
round
check "first sighting waits one backoff step" [ "$(connects)" -eq 0 ]
FAKE_NOW=1004
round
check "nothing before the step has elapsed" [ "$(connects)" -eq 0 ]
FAKE_NOW=1005
round
check "a missing NQN is reconnected" connected_to 2
check "exactly once" [ "$(connects)" -eq 1 ]
FAKE_NOW=1100
round
check "and left alone once it is back" [ "$(connects)" -eq 1 ]

reset_world
ctrl 2 2 connecting
ctrl 3 3 resetting
FAKE_NOW=2000
round
round
check "the kernel's own reconnect is not interfered with" [ "$(connects)" -eq 0 ]

reset_world
ctrl 1 1 dead
ctrl 2 2 "deleting (no IO)"
round
FAKE_NOW=1005
round
check "dead and deleting controllers count as missing" \
	bash -c '[ "$(grep -c "^connect" "$0")" -eq 2 ]' "${MOCK_LOG}"

reset_world
rm -rf "${MOCK_SYS}/nvme1"
touch "${MOCK_FAIL}"
round
FAKE_NOW=1005; round		# attempt 1 fails -> next in 10s
FAKE_NOW=1014; round		# too early
check "a failed attempt backs off" [ "$(connects)" -eq 1 ]
FAKE_NOW=1015; round		# attempt 2 fails -> next in 20s (cap)
FAKE_NOW=1034; round
check "the backoff doubles" [ "$(connects)" -eq 2 ]
FAKE_NOW=1035; round		# attempt 3 fails -> still 20s
FAKE_NOW=1054; round
check "and is capped" [ "$(connects)" -eq 3 ]
check "failures are counted" [ "${RCOW_PATROL_FAILS[$(rcow_nqn 1)]}" -eq 3 ]
rm -f "${MOCK_FAIL}"
FAKE_NOW=1055; round
check "it never gives up" connected_to 1
check "and forgets the failures on success" \
	[ "${RCOW_PATROL_FAILS[$(rcow_nqn 1)]+set}" != "set" ]

reset_world
rm -rf "${MOCK_SYS}"/nvme*
round
FAKE_NOW=1005
round
check "a round is bounded" [ "$(connects)" -eq 2 ]
round
check "the rest follow in the next round" [ "$(connects)" -eq 4 ]

reset_world
rm -rf "${MOCK_SYS}/nvme0"
echo 999 >"${RCOW_START_READY}"
round
FAKE_NOW=1005
round
check "a start that has not finished is not patrolled" [ "$(connects)" -eq 0 ]
rm -f "${RCOW_START_READY}"
round
check "nor one without start.ready" [ "$(connects)" -eq 0 ]

reset_world
rcow_disconnect_all >/dev/null 2>&1
check "rcow_disconnect_all pauses the patrol" [ -e "${RCOW_PATROL_HOLD}" ]
: >"${MOCK_LOG}"
round
FAKE_NOW=1005
round
check "a planned disconnect is not undone" [ "$(connects)" -eq 0 ]
# rcow_start.sh calls it as `rcow_connect_all || ...`, i.e. without errexit.
rcow_connect_all >/dev/null 2>&1 || true
check "rcow_connect_all resumes it" [ ! -e "${RCOW_PATROL_HOLD}" ]
check "and logs the grid back in" [ "$(connects)" -eq 4 ]

reset_world
rm -rf "${MOCK_SYS}/nvme0"
exec {lockfd}>>"${RCOW_PATROL_LOCK}"
flock "${lockfd}"
round
FAKE_NOW=1005
round
check "a round skips while another holds the lock" [ "$(connects)" -eq 0 ]
exec {lockfd}>&-
round
FAKE_NOW=1010
round
check "and runs once it is free" [ "$(connects)" -eq 1 ]

# rcow_stop.sh runs rcow_disconnect_all -- and so rcow_patrol_hold -- before it
# computes UNLOAD_BUDGET as (RCOW_STOP_TIMEOUT - elapsed) - 60, floored at 10.
# A hold wait sized for a whole round (4 x 30 + 5 = 125s) spends the entire
# 120s budget, so the unload drops to that floor and a clean stop silently
# degrades into a WAL-replay stop. Asserted at the production defaults, not
# this test's shortened ones, and read off the function so it costs no waiting.
HOLD_WAIT="$(RCOW_PATROL_CONNECT_TIMEOUT=30; RCOW_PATROL_MAX_PER_ROUND=4; rcow_patrol_hold_wait)"
check "the hold wait is bounded by one connect, not a whole round" \
	[ "${HOLD_WAIT}" -le 35 ]
check "and leaves the stop's unload more than its floor" \
	[ "${HOLD_WAIT}" -lt $((120 - 60 - 10)) ]

reset_world
rm -rf "${MOCK_SYS}/nvme0"
mv "${tmp}/bin/nvme" "${tmp}/bin/nvme.off"
round
FAKE_NOW=1005
check "a round never fails, even without nvme-cli" rcow_patrol_controllers
mv "${tmp}/bin/nvme.off" "${tmp}/bin/nvme"

if [ "${failed}" -ne 0 ]; then
	echo "--- patrol output"
	cat "${tmp}/patrol.out"
fi
printf '=== result: %d passed, %d failed ===\n' "${passed}" "${failed}"
[ "${failed}" -eq 0 ]
