#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# cube-egress-net-entrypoint.sh must exit promptly on SIGTERM and remove its
# rules on the way out. A cleanup-only TERM trap used to resume the reapply
# loop, so the container lingered until terminationGracePeriodSeconds expired.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${SCRIPT_DIR}/cube-egress-net-entrypoint.sh"
TMP="$(mktemp -d)"
pid=""
cleanup() {
  [[ -n "${pid}" ]] && kill -KILL "${pid}" 2>/dev/null || true
  rm -rf "${TMP}"
}
trap cleanup EXIT

mkdir -p "${TMP}/bin"
cat > "${TMP}/init.sh" <<EOF
#!/usr/bin/env bash
echo "\$1" >> "${TMP}/calls"
EOF
cat > "${TMP}/bin/ip" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${TMP}/init.sh" "${TMP}/bin/ip"

PATH="${TMP}/bin:${PATH}" \
  CUBE_EGRESS_NET_INIT="${TMP}/init.sh" \
  CUBE_EGRESS_NET_REAPPLY_INTERVAL_SECONDS=600 \
  bash "${ENTRY}" 2>"${TMP}/stderr" &
pid=$!

for _ in $(seq 1 50); do
  grep -qx up "${TMP}/calls" 2>/dev/null && break
  sleep 0.1
done
grep -qx up "${TMP}/calls" || { echo "FAIL: rules were never applied" >&2; cat "${TMP}/stderr" >&2; exit 1; }

kill -TERM "${pid}"
for _ in $(seq 1 30); do
  kill -0 "${pid}" 2>/dev/null || break
  sleep 0.1
done
if kill -0 "${pid}" 2>/dev/null; then
  echo "FAIL: entrypoint still running 3s after SIGTERM" >&2
  exit 1
fi
pid=""
echo "ok: exited within 3s of SIGTERM"

grep -qx down "${TMP}/calls" || { echo "FAIL: rules were not removed on SIGTERM" >&2; exit 1; }
echo "ok: rules removed on exit"
