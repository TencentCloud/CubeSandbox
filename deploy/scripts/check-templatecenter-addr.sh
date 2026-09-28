#!/usr/bin/env bash
# Diagnose why CubeMaster reports "CUBE_TEMPLATE_CENTER_ADDR is not configured"
# even though you believe the variable is set.
#
# The read path in CubeMaster is a plain os.Getenv("CUBE_TEMPLATE_CENTER_ADDR")
# evaluated on every request (see Config.TemplateCenterAddr). If it reports the
# variable empty, then the running CubeMaster PROCESS does not actually have it
# in its environment -- regardless of what your interactive shell shows.
#
# Usage: bash check-templatecenter-addr.sh
set -euo pipefail

VAR="CUBE_TEMPLATE_CENTER_ADDR"

echo "=== 1. Your current shell ==="
if [ -n "${!VAR:-}" ]; then
  # %q-style: show with surrounding quotes so trailing/leading spaces are visible.
  printf '  %s=[%s] (len=%d)\n' "$VAR" "${!VAR}" "${#VAR}"
else
  echo "  $VAR is NOT set in this shell"
fi

echo
echo "=== 2. Running CubeMaster process environment (the source of truth) ==="
PIDS=$(pgrep -f 'cubemaster' || true)
if [ -z "$PIDS" ]; then
  echo "  No cubemaster process found (pgrep -f cubemaster). Adjust the pattern if your binary is named differently."
else
  for pid in $PIDS; do
    echo "  --- pid $pid ---"
    if [ -r "/proc/$pid/environ" ]; then
      if tr '\0' '\n' < "/proc/$pid/environ" | grep -q "^${VAR}="; then
        val=$(tr '\0' '\n' < "/proc/$pid/environ" | grep "^${VAR}=" | cut -d= -f2-)
        printf '  %s=[%s] (len=%d)\n' "$VAR" "$val" "${#val}"
      else
        echo "  $VAR is ABSENT from process $pid environment  <-- this is why it fails"
      fi
    else
      echo "  cannot read /proc/$pid/environ (permission? not Linux?)"
    fi
  done
fi

echo
echo "=== 3. How is CubeMaster started? ==="
echo "  If started via systemd:  systemctl cat cube-sandbox-cubemaster | grep -i environment"
echo "    -> the variable must be in an Environment= line or EnvironmentFile=,"
echo "       then 'systemctl daemon-reload && systemctl restart cube-sandbox-cubemaster'"
echo "  If started via docker:   docker inspect <container> | grep -A20 Env"
echo "    -> the variable must be in the container's env (docker run -e / compose environment:)"
echo "  If started via supervisor/k8s: check the supervisor program env / pod env"
echo
echo "  NOTE: exporting the variable in your login shell does NOT propagate to an"
echo "  already-running (or separately-launched) CubeMaster process."

echo
echo "=== 4. Quick fix to verify ==="
echo "  Restart CubeMaster with the variable injected, then re-run this script:"
echo "    sudo systemctl edit cube-sandbox-cubemaster   # add: [Service] Environment=CUBE_TEMPLATE_CENTER_ADDR=http://<tc>:8090"
echo "    sudo systemctl daemon-reload && sudo systemctl restart cube-sandbox-cubemaster"
