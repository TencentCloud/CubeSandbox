#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

stop_by_pidfile "cubelet" "^${TOOLBOX_ROOT}/Cubelet/bin/cubelet --config"
stop_by_pidfile "cube-api" "^${TOOLBOX_ROOT}/CubeAPI/bin/cube-api"
stop_by_pidfile "cubemaster"
stop_by_pidfile "network-agent"

# Clean up TAP devices to prevent conflicts on next start
# This ensures network-agent recover() starts from a clean state and avoids
# race conditions during rapid restart when leftover TAP devices exist.
#
# TAP devices are named with the pattern: z<IP>.<IP>.<IP>.<IP> where the IP
# is from the CIDR configured in CUBE_SANDBOX_CIDR. We clean all devices
# matching this pattern regardless of the specific CIDR, since they are all
# created by network-agent for sandbox instances.
cleanup_tap_devices() {
  local cleanup_count=0

  # Match all TAP devices with pattern: z<digits>.<digits>.<digits>.<digits>
  # This covers all CIDR ranges (e.g., z192.168.x.x, z10.x.x.x, etc.)
  for iface in $(ip link show 2>/dev/null | grep -oE 'z[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' || true); do
    if ip link delete "$iface" 2>/dev/null; then
      cleanup_count=$((cleanup_count + 1))
    fi
  done

  if [[ $cleanup_count -gt 0 ]]; then
    log "cleaned $cleanup_count TAP device(s)"
  fi
}

cleanup_tap_devices

log "local services stopped"
