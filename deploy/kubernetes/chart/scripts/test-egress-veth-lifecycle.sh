#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "${SCRIPT_DIR}/../../../.." && pwd)"

unshare -n sleep 60 &
host_pid=$!
trap 'kill "${host_pid}" 2>/dev/null || true' EXIT

export CUBE_EGRESS_ROLE=primary
export CUBE_ROUTER_NAT_IP=172.20.0.2
export CUBE_EGRESS_HOST_NETNS_PID="${host_pid}"

"${REPO_ROOT}/EgressProxy/proxy/setup-veth.sh"

ip -4 address show dev egress-host0 |
  grep -F '169.254.240.2/30' >/dev/null
ip route show 172.20.0.2/32 |
  grep -F 'via 169.254.240.1 dev egress-host0' >/dev/null
nsenter -t "${host_pid}" -n -- ip -4 address show dev cube-egress-p0 |
  grep -F '169.254.240.1/30' >/dev/null
nsenter -t "${host_pid}" -n -- ip -d link show dev cube-egress-p0 |
  grep -F 'alias cube-egress:primary' >/dev/null
nsenter -t "${host_pid}" -n -- \
  ping -c 2 -W 1 169.254.240.2 >/dev/null

proxy_mtu="$(cat /sys/class/net/egress-host0/mtu)"
host_mtu="$(
  nsenter -t "${host_pid}" -n -- \
    ip -o link show dev cube-egress-p0 |
    awk '{ for (i = 1; i <= NF; i++) if ($i == "mtu") print $(i + 1) }'
)"
[ "${proxy_mtu}" = "${host_mtu}" ]

nsenter -t "${host_pid}" -n -- ip link del cube-egress-p0
nsenter -t "${host_pid}" -n -- ip link add cube-egress-p0 type dummy
if "${REPO_ROOT}/EgressProxy/proxy/setup-veth.sh" 2>/dev/null; then
  printf 'setup-veth unexpectedly replaced an unowned host interface\n' >&2
  exit 1
fi
nsenter -t "${host_pid}" -n -- ip link show cube-egress-p0 >/dev/null

nsenter -t "${host_pid}" -n -- \
  ip link set cube-egress-p0 alias cube-egress:primary-foreign
if "${REPO_ROOT}/EgressProxy/proxy/setup-veth.sh" 2>/dev/null; then
  printf 'setup-veth accepted a prefix-matching foreign alias\n' >&2
  exit 1
fi
nsenter -t "${host_pid}" -n -- ip link show cube-egress-p0 >/dev/null

nsenter -t "${host_pid}" -n -- ip link del cube-egress-p0
nsenter -t "${host_pid}" -n -- ip link add foreign-link type dummy
nsenter -t "${host_pid}" -n -- ip address add 169.254.240.100/24 dev foreign-link
if "${REPO_ROOT}/EgressProxy/proxy/setup-veth.sh" 2>/dev/null; then
  printf 'setup-veth unexpectedly reused an occupied link-local range\n' >&2
  exit 1
fi
nsenter -t "${host_pid}" -n -- ip -4 address show dev foreign-link |
  grep -F '169.254.240.100/24' >/dev/null

printf 'EgressProxy veth lifecycle integration test passed\n'
