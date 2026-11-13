#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Behavioral tests for cube-egress-net-stop.sh: the teardown wrapper must
# resolve the init script exactly like the start wrapper (explicit env,
# auto-detect, legacy pre-#2034 name) and exec it with `down` — a start
# that accepts the legacy name and a stop that rejects it would leave the
# rules behind on teardown.

set -euo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STOP_SH_SRC="${TESTS_DIR}/../scripts/systemd/cube-egress-net-stop.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

LAYOUT="${TMP}/toolbox"
mkdir -p "${LAYOUT}/scripts/systemd" "${LAYOUT}/scripts/cube-egress" "${TMP}/bin"
cp "${STOP_SH_SRC}" "${LAYOUT}/scripts/systemd/cube-egress-net-stop.sh"

cat > "${LAYOUT}/scripts/systemd/common.sh" <<FIXTURE
TOOLBOX_ROOT="${LAYOUT}"
log() { echo "[stub] \$*" >&2; }
die() { echo "[stub] FATAL: \$*" >&2; exit 1; }
require_root() { :; }
ensure_executable() { [[ -x "\$1" ]] || die "required executable not found: \$1"; }
FIXTURE

run_stop() {
    env PATH="${TMP}/bin:/usr/bin:/bin" \
        "${LAYOUT}/scripts/systemd/cube-egress-net-stop.sh"
}

bash -n "${STOP_SH_SRC}" || fail "syntax"

# --- 1: no init script anywhere -> hard error (same contract as start) ---
if run_stop 2>"${TMP}/err"; then
    fail "stop exited 0 with no init script present"
fi
grep -q "no egress iptables init script" "${TMP}/err" \
    || fail "missing-init error does not mention the init script"

# --- 2: explicit CUBE_EGRESS_NET_INIT missing -> ensure_executable error ---
if CUBE_EGRESS_NET_INIT="${TMP}/no-such-init.sh" run_stop 2>"${TMP}/err"; then
    fail "stop exited 0 with a nonexistent CUBE_EGRESS_NET_INIT"
fi
grep -q "required executable not found" "${TMP}/err" \
    || fail "explicit-path miss did not hit ensure_executable"

make_fake_init() {
    local name="$1" exit_code="$2"
    cat > "${LAYOUT}/scripts/cube-egress/${name}" <<FIXTURE
#!/usr/bin/env bash
echo "init \$*" >> "${TMP}/calls"
exit ${exit_code}
FIXTURE
    chmod +x "${LAYOUT}/scripts/cube-egress/${name}"
}

# --- 3: current name auto-detected -> exec 'down' ---
make_fake_init cube-egress-iptables-init.sh 0
run_stop 2>/dev/null || fail "stop failed with current-name init present"
grep -q '^init down$' "${TMP}/calls" || fail "current-name init not exec'd with 'down'"

# --- 4: legacy pre-#2034 name only -> still tears down (the symmetry fix) ---
rm "${LAYOUT}/scripts/cube-egress/cube-egress-iptables-init.sh"
make_fake_init cube-proxy-iptables-init.sh 0
: > "${TMP}/calls"
run_stop 2>"${TMP}/err" || { cat "${TMP}/err"; fail "stop failed on legacy-name-only box (asymmetry regression)"; }
grep -q '^init down$' "${TMP}/calls" || fail "legacy-name init not exec'd with 'down'"

# --- 5: teardown failure is propagated ---
make_fake_init cube-proxy-iptables-init.sh 4
set +e; run_stop 2>/dev/null; RC=$?; set -e
[ "${RC}" -eq 4 ] || fail "init down exit status not propagated (got ${RC}, want 4)"

echo "test_egress_net_stop: all scenarios passed"
