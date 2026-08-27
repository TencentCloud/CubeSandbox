#!/bin/sh
set -eu

POD_IP="${POD_IP:?POD_IP is required}"
CUBE_ROUTER_NAT_IP="${CUBE_ROUTER_NAT_IP:?CUBE_ROUTER_NAT_IP is required}"
CUBE_EGRESS_ROLE="${CUBE_EGRESS_ROLE:?CUBE_EGRESS_ROLE is required}"

case "${CUBE_EGRESS_ROLE}" in
  primary)
    VETH_HOST_IP=169.254.240.1
    VETH_POD_IP=169.254.240.2
    ;;
  standby)
    VETH_HOST_IP=169.254.240.5
    VETH_POD_IP=169.254.240.6
    ;;
  *)
    printf 'unsupported EgressProxy role: %s\n' "${CUBE_EGRESS_ROLE}" >&2
    exit 1
    ;;
esac

test "$(cat /proc/sys/net/ipv4/ip_forward)" = "1"
ip link show egress-host0 >/dev/null

iptables -w -t filter -N CUBE-EGRESS-PROXY 2>/dev/null || true
iptables -w -t filter -F CUBE-EGRESS-PROXY
iptables -w -t filter -A CUBE-EGRESS-PROXY \
  -s "${CUBE_ROUTER_NAT_IP}/32" \
  -i egress-host0 -o eth0 -j ACCEPT
iptables -w -t filter -A CUBE-EGRESS-PROXY \
  -s "${VETH_HOST_IP}/32" \
  -i egress-host0 -o eth0 -j ACCEPT
iptables -w -t filter -A CUBE-EGRESS-PROXY \
  -i eth0 -o egress-host0 \
  -m conntrack --ctstate ESTABLISHED,RELATED \
  -j ACCEPT
iptables -w -t filter -A CUBE-EGRESS-PROXY -j DROP

while iptables -w -t filter -C FORWARD \
  -j CUBE-EGRESS-PROXY 2>/dev/null; do
  iptables -w -t filter -D FORWARD -j CUBE-EGRESS-PROXY
done
iptables -w -t filter -I FORWARD 1 -j CUBE-EGRESS-PROXY

iptables -w -t nat -N CUBE-EGRESS-PROXY-SNAT 2>/dev/null || true
iptables -w -t nat -F CUBE-EGRESS-PROXY-SNAT
iptables -w -t nat -A CUBE-EGRESS-PROXY-SNAT \
  -o eth0 -j SNAT --to-source "${POD_IP}"
for source_ip in "${CUBE_ROUTER_NAT_IP}" "${VETH_HOST_IP}"; do
  while iptables -w -t nat -C POSTROUTING \
    -s "${source_ip}/32" \
    -j CUBE-EGRESS-PROXY-SNAT 2>/dev/null; do
    iptables -w -t nat -D POSTROUTING \
      -s "${source_ip}/32" \
      -j CUBE-EGRESS-PROXY-SNAT
  done
  iptables -w -t nat -I POSTROUTING 1 \
    -s "${source_ip}/32" \
    -j CUBE-EGRESS-PROXY-SNAT
done

: > /run/cube-egress/ready
exec /opt/cube-egress/health-server.sh "${VETH_POD_IP}"
