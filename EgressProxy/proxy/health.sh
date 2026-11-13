#!/bin/sh
set -eu

CUBE_ROUTER_NAT_IP="${CUBE_ROUTER_NAT_IP:?CUBE_ROUTER_NAT_IP is required}"
CUBE_EGRESS_ROLE="${CUBE_EGRESS_ROLE:?CUBE_EGRESS_ROLE is required}"

case "${CUBE_EGRESS_ROLE}" in
  primary) VETH_HOST_IP=169.254.240.1 ;;
  standby) VETH_HOST_IP=169.254.240.5 ;;
  *) exit 1 ;;
esac

test -f /run/cube-egress/ready
test "$(cat /proc/sys/net/ipv4/ip_forward)" = "1"
ip link show egress-host0 >/dev/null
ip route show "${CUBE_ROUTER_NAT_IP}/32" |
  grep -F "dev egress-host0" >/dev/null
iptables -w -t filter -C FORWARD -j CUBE-EGRESS-PROXY
iptables -w -t nat -C POSTROUTING \
  -s "${CUBE_ROUTER_NAT_IP}/32" \
  -j CUBE-EGRESS-PROXY-SNAT
iptables -w -t nat -C POSTROUTING \
  -s "${VETH_HOST_IP}/32" \
  -j CUBE-EGRESS-PROXY-SNAT
