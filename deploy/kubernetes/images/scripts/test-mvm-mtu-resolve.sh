#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit checks for resolve_mvm_mtu in component-entrypoint.sh (no container
# required). Covers the auto/pinned/disabled settings and the guard rails:
# auto never raises the packaged value, and nothing goes below the virtio
# minimum of 1280.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${SCRIPT_DIR}/component-entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Load entrypoint helpers without executing main.
# shellcheck disable=SC1090
source <(sed '/^main "$@"/d' "${ENTRY}")

assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "${got}" != "${want}" ]]; then
    printf 'FAIL: %s (got=%q want=%q)\n' "${msg}" "${got}" "${want}" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${msg}"
}

# Assert an input is rejected AND that the message names the guard that
# rejected it. Without the second half a case can pass for the wrong reason:
# "banana" reaches the arithmetic guard as 0 and is refused as "below the
# minimum", so dropping the format check entirely would leave this suite green.
expect_fail() { # <setting> <expected message substring> [link_mtu]
  local out rc=0
  out="$(resolve_mvm_mtu "$1" "${3:-eth0}" "${CFG}" 2>&1)" || rc=$?
  if (( rc == 0 )); then
    printf 'FAIL: %s (accepted %q, expected rejection)\n' "$2" "$1" >&2
    exit 1
  fi
  if [[ "${out}" != *"$2"* ]]; then
    printf 'FAIL: rejected %q, but not by the expected guard\n  want substring: %q\n  got: %q\n' \
      "$1" "$2" "${out}" >&2
    exit 1
  fi
  printf 'ok: %s rejected by the "%s" guard\n' "$1" "$2"
}

# A config.toml carrying the packaged guest MTU.
make_cfg() {
  local path="$1" mtu="$2"
  cat >"${path}" <<TOML
[plugins."io.cubelet.internal.v1.network"]
    eth_name = "eth0"
    cidr = "172.16.0.0/18"
    mvm_mtu = ${mtu}
    tap_init_num = 0
TOML
}

CFG="${TMP}/config.toml"
make_cfg "${CFG}" 1500

# auto is driven by the uplink MTU the caller parses out of `ip -o link`, so
# the function itself stays pure and needs no fake interface.
resolve_auto() { # <link_mtu> <packaged_mtu>
  make_cfg "${CFG}" "$2"
  resolve_mvm_mtu auto "$1" "${CFG}" 2>/dev/null
}

# --- explicit settings ---
assert_eq "$(resolve_mvm_mtu 0 eth0 "${CFG}")" "0" "0 keeps the packaged value"
assert_eq "$(resolve_mvm_mtu '' eth0 "${CFG}")" "0" "empty keeps the packaged value"
assert_eq "$(resolve_mvm_mtu 1450 eth0 "${CFG}")" "1450" "an integer pins mvm_mtu"
assert_eq "$(resolve_mvm_mtu 1280 eth0 "${CFG}")" "1280" "the virtio minimum is accepted"

assert_eq "$(resolve_mvm_mtu 65535 eth0 "${CFG}")" "65535" "the maximum is accepted"

# --- leading zeros are normalized, never written back raw ---
# TOML rejects a leading zero, so writing "010000" back would produce a
# config.toml cubelet cannot parse; bash would also read it as octal 4096.
assert_eq "$(resolve_mvm_mtu 010000 eth0 "${CFG}")" "10000" "a leading zero is stripped, not read as octal"
assert_eq "$(resolve_mvm_mtu 01450 eth0 "${CFG}")" "1450" "a leading-zero value normalizes to its decimal form"
assert_eq "$(resolve_mvm_mtu 000 eth0 "${CFG}")" "0" "an all-zero value keeps the packaged value"

# --- explicit settings that must be rejected, by the right guard ---
expect_fail 1279 "below the virtio minimum"
expect_fail 0800 "below the virtio minimum"
expect_fail banana 'must be "auto", 0, or a positive integer'
expect_fail 1450x 'must be "auto", 0, or a positive integer'
expect_fail 65536 "above the maximum"
expect_fail 99999999999999999999 "above the maximum"
# 2^64 + 1450: bash arithmetic wraps this to 1450, so it clears BOTH range
# checks and the full 20-digit string reaches config.toml. Only the digit-count
# gate, applied before (( )) sees the value, rejects it.
expect_fail 18446744073709553066 "above the maximum"

# --- a pin above the detected uplink is honoured but warned about ---
# This is the misconfiguration that silently recreates the blackhole the
# default exists to prevent, so it must not pass unremarked.
warn_out="$(resolve_mvm_mtu 9000 1450 "${CFG}" 2>&1 >/dev/null)"
assert_eq "$(resolve_mvm_mtu 9000 1450 "${CFG}" 2>/dev/null)" "9000" "a pin above the uplink is still honoured"
if [[ "${warn_out}" != *"exceeds the detected uplink MTU 1450"* ]]; then
  printf 'FAIL: pinning above the uplink should warn (got=%q)\n' "${warn_out}" >&2
  exit 1
fi
printf 'ok: pinning above the uplink warns\n'

# --- auto ---
assert_eq "$(resolve_auto 1450 1500)" "1450" "auto lowers to an overlay-CNI uplink"
assert_eq "$(resolve_auto 1500 1500)" "0" "auto is a no-op when the uplink matches"
assert_eq "$(resolve_auto 9000 1500)" "0" "auto never raises the packaged value"
assert_eq "$(resolve_auto 1200 1500)" "0" "auto refuses an uplink below the virtio minimum"

# auto with an unreadable interface must keep the packaged value.
assert_eq "$(resolve_mvm_mtu auto "" "${CFG}" 2>/dev/null)" "0" \
  "auto keeps the packaged value when the uplink MTU cannot be read"

# --- the patch actually lands in the config ---
# patch_mvm_mtu is the write-back run_cubelet calls, sourced from the
# entrypoint itself so these checks cannot drift from the shipped expression.
apply_mvm_mtu_patch() { # <cfg> <value>
  patch_mvm_mtu "$1" "$2" >/dev/null
}

make_cfg "${CFG}" 1500
target="$(resolve_mvm_mtu auto 1450 "${CFG}" 2>/dev/null)"
apply_mvm_mtu_patch "${CFG}" "${target}"
assert_eq "$(sed -n 's/^[[:space:]]*mvm_mtu[[:space:]]*=[[:space:]]*\([0-9]\{1,\}\).*/\1/p' "${CFG}")" "1450" \
  "the resolved value is written back into config.toml"
assert_eq "$(grep -c 'eth_name\|cidr\|tap_init_num' "${CFG}")" "3" \
  "the neighbouring keys are untouched"

# --- a commented example line is never rewritten ---
# An unanchored s/mvm_mtu = [0-9]+/ matches inside a comment too, silently
# editing documentation and, if the comment is the only match, patching
# nothing that cubelet reads.
COMMENTED="${TMP}/commented.toml"
cat >"${COMMENTED}" <<'TOML'
    # mvm_mtu = 1500 (example: lower this on an overlay CNI)
    mvm_mtu = 1500
TOML
apply_mvm_mtu_patch "${COMMENTED}" 1450
assert_eq "$(sed -n '1p' "${COMMENTED}")" \
  "    # mvm_mtu = 1500 (example: lower this on an overlay CNI)" \
  "the commented example line is left alone"
assert_eq "$(sed -n '2p' "${COMMENTED}")" "    mvm_mtu = 1450" \
  "the real key is still patched"

# --- a value the rewrite cannot handle is skipped, not corrupted ---
# TOML accepts hexadecimal and underscore-separated integers. The digit match
# in the write-back would match only their leading digits, so rewriting would
# splice the remainder onto the new value and leave a config.toml cubelet
# cannot parse. Each of these must survive untouched, and must not be reported
# as patched.
for literal in '0x5DC' '1_500' '0o2734' '1.5e3'; do
  ODD="${TMP}/odd.toml"
  make_cfg "${ODD}" "${literal}"
  out="$(patch_mvm_mtu "${ODD}" 1450)"
  assert_eq "$(sed -n 's/^[[:space:]]*mvm_mtu[[:space:]]*=[[:space:]]*//p' "${ODD}")" "${literal}" \
    "mvm_mtu = ${literal} is left untouched"
  if [[ "${out}" == *"patched mvm_mtu"* ]]; then
    printf 'FAIL: %s was skipped but reported as patched (%q)\n' "${literal}" "${out}" >&2
    exit 1
  fi
  printf 'ok: %s is not reported as patched\n' "${literal}"
done

# --- a missing key is skipped, and never reported as patched ---
# A blind write-back logs "patched mvm_mtu -> N" while changing nothing, which
# is the failure this whole change exists to prevent: the operator reads a
# success line and the blackhole is still there.
NOKEY="${TMP}/nokey.toml"
cat >"${NOKEY}" <<'TOML'
[plugins."io.cubelet.internal.v1.network"]
    eth_name = "eth0"
    # mvm_mtu = 1500 (example: lower this on an overlay CNI)
TOML
before="$(cat "${NOKEY}")"
out="$(patch_mvm_mtu "${NOKEY}" 1450)"
assert_eq "$(cat "${NOKEY}")" "${before}" "a config without a live mvm_mtu key is left untouched"
assert_eq "${out}" "$(CUBE_ROLE="${CUBE_ROLE:-run}" log "no mvm_mtu key in ${NOKEY}, nothing to patch")" \
  "a missing key is reported as skipped, not patched"

# --- an explicit pin that cannot be applied is an error, not a warning ---
# Fail-open is right for the derived value: an unexpected config should not
# crash-loop the node. It is wrong for a pin, where it would leave the operator
# believing an MTU is set that is not — the silent mismatch this change removes.
expect_pin_fail() { # <cfg> <expected message substring>
  local out rc=0
  out="$(patch_mvm_mtu "$1" 1450 pinned 2>&1)" || rc=$?
  if (( rc == 0 )); then
    printf 'FAIL: a pin on %s was skipped instead of refused\n' "$1" >&2
    exit 1
  fi
  if [[ "${out}" != *"$2"* ]]; then
    printf 'FAIL: pin refused, but not by the expected guard\n  want: %q\n  got: %q\n' "$2" "${out}" >&2
    exit 1
  fi
  printf 'ok: a pin is refused when %s\n' "$2"
}
# The call site decides pinned-vs-derived from the setting, so check that
# mapping directly: without it the hard-fail above is unreachable in the
# entrypoint even though the function itself is correct.
for setting in 1450 1280 65535 010000; do
  mvm_mtu_is_pinned "${setting}" \
    || { printf 'FAIL: %s should count as an explicit pin\n' "${setting}" >&2; exit 1; }
  printf 'ok: %s is treated as an explicit pin\n' "${setting}"
done
for setting in auto 0 ''; do
  if mvm_mtu_is_pinned "${setting}"; then
    printf 'FAIL: %q should be a derived/keep-packaged setting, not a pin\n' "${setting}" >&2
    exit 1
  fi
  printf 'ok: %q is not treated as a pin\n' "${setting}"
done

PINNED_ODD="${TMP}/pinned-odd.toml"
make_cfg "${PINNED_ODD}" '0x5DC'
expect_pin_fail "${PINNED_ODD}" "is not a plain decimal literal"
expect_pin_fail "${NOKEY}" "no mvm_mtu key"
# ...while the derived value on the very same files stays fail-open.
out="$(patch_mvm_mtu "${PINNED_ODD}" 1450)" || {
  printf 'FAIL: the derived value must stay fail-open\n' >&2; exit 1; }
assert_eq "$(sed -n 's/^[[:space:]]*mvm_mtu[[:space:]]*=[[:space:]]*//p' "${PINNED_ODD}")" '0x5DC' \
  "the derived value leaves the same file untouched"

# Drift guard: a regression to the unanchored form would reintroduce both.
if grep -q 's/mvm_mtu = ' "${ENTRY}"; then
  printf 'FAIL: run_cubelet writes mvm_mtu with an unanchored sed\n' >&2
  exit 1
fi
printf 'ok: the write-back in component-entrypoint.sh is anchored\n'

# Wiring guard: the checks above prove the decision and the write-back, but
# they call patch_mvm_mtu directly. If run_cubelet stopped forwarding the
# pinned flag, every check would still pass while an explicit pin silently
# degraded to fail-open in the shipped path.
if ! grep -q 'mvm_mtu_is_pinned "${CUBE_SANDBOX_NETWORK_MTU:-auto}"' "${ENTRY}"; then
  printf 'FAIL: run_cubelet no longer derives the pinned flag from the setting\n' >&2
  exit 1
fi
if ! grep -q 'patch_mvm_mtu "${cfg}" "${mvm_mtu_target}" "${mvm_mtu_pinned}"' "${ENTRY}"; then
  printf 'FAIL: run_cubelet no longer forwards the pinned flag to patch_mvm_mtu\n' >&2
  exit 1
fi
printf 'ok: run_cubelet forwards the pinned flag to the write-back\n'

# --- the uplink MTU is parsed out of `ip -o link`, not sysfs ---
# sysfs is mounted from the host inside cube-node, so /sys/class/net/eth0/mtu
# reports the host NIC rather than the Pod netns interface. Guard the parser
# that replaced it.
parse_link_mtu() { sed -n 's/.* mtu \([0-9]\{1,\}\).*/\1/p' | head -1; }
assert_eq "$(printf '%s\n' '2: eth0@if2522: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1450 qdisc noqueue state UP mode DEFAULT group default qlen 1000\\    link/ether a6:b6:10:17:2e:49' | parse_link_mtu)" \
  "1450" "the uplink MTU is parsed from an ip -o link line"
assert_eq "$(printf '%s\n' '1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN' | parse_link_mtu)" \
  "65536" "a jumbo/loopback MTU parses"
assert_eq "$(printf '' | parse_link_mtu)" "" "no interface yields an empty MTU"

printf '\nall mvm_mtu resolution checks passed\n' 
