#!/bin/sh
set -eu

ROLE="${CUBE_EGRESS_ROLE:?CUBE_EGRESS_ROLE is required}"
CUBE_ROUTER_NAT_IP="${CUBE_ROUTER_NAT_IP:?CUBE_ROUTER_NAT_IP is required}"
POD_IFACE=egress-host0
HOST_NETNS_PID="${CUBE_EGRESS_HOST_NETNS_PID:-1}"

case "${ROLE}" in
  primary)
    HOST_IFACE=cube-egress-p0
    HOST_IP=169.254.240.1
    POD_IP=169.254.240.2
    ;;
  standby)
    HOST_IFACE=cube-egress-p1
    HOST_IP=169.254.240.5
    POD_IP=169.254.240.6
    ;;
  *)
    printf 'unsupported EgressProxy role: %s\n' "${ROLE}" >&2
    exit 1
    ;;
esac

MTU="$(cat /sys/class/net/eth0/mtu)"
TEMP_HOST_IFACE="ce-${ROLE}"
OWNER_ALIAS="cube-egress:${ROLE}"

for owned_iface in cube-egress-p0 cube-egress-p1; do
  case "${owned_iface}" in
    cube-egress-p0) expected_alias=cube-egress:primary ;;
    cube-egress-p1) expected_alias=cube-egress:standby ;;
  esac
  if nsenter -t "${HOST_NETNS_PID}" -n -- \
    ip link show "${owned_iface}" >/dev/null 2>&1; then
    actual_alias="$(
      nsenter -t "${HOST_NETNS_PID}" -n -- \
        ip -d -o link show "${owned_iface}" |
        awk '{
          for (i = 1; i <= NF; i++) {
            if ($i == "alias") {
              print $(i + 1)
              exit
            }
          }
        }'
    )"
    if [ "${actual_alias}" = "${expected_alias}" ]; then
      continue
    fi
    printf 'host interface %s already exists and is not owned by %s\n' \
      "${owned_iface}" "${expected_alias}" >&2
    exit 1
  fi
done

if conflict="$(
  nsenter -t "${HOST_NETNS_PID}" -n -- ip -4 -o address show |
    awk '
      function ip_to_int(ip, octet) {
        split(ip, octet, "[.]")
        return (((octet[1] * 256 + octet[2]) * 256 + octet[3]) * 256 +
          octet[4])
      }
      $2 == "cube-egress-p0" || $2 == "cube-egress-p1" { next }
      {
        split($4, cidr, "/")
        size = 2 ^ (32 - cidr[2])
        address = ip_to_int(cidr[1])
        network = address - (address % size)
        last = network + size - 1
        target = ip_to_int("169.254.240.0")
        if (network <= target + 7 && last >= target) {
          print $2 " " $4
        }
      }
    '
)"; [ -n "${conflict}" ]; then
  printf 'link-local range 169.254.240.0/29 is occupied: %s\n' \
    "${conflict}" >&2
  exit 1
fi

if nsenter -t "${HOST_NETNS_PID}" -n -- \
  ip link show "${HOST_IFACE}" >/dev/null 2>&1; then
  if ! nsenter -t "${HOST_NETNS_PID}" -n -- \
    ip -d -o link show "${HOST_IFACE}" |
    awk '{
      for (i = 1; i <= NF; i++) {
        if ($i == "alias") {
          print $(i + 1)
          exit
        }
      }
    }' |
    grep -Fx "${OWNER_ALIAS}" >/dev/null; then
    printf 'host interface %s already exists and is not owned by %s\n' \
      "${HOST_IFACE}" "${OWNER_ALIAS}" >&2
    exit 1
  fi
  nsenter -t "${HOST_NETNS_PID}" -n -- ip link del "${HOST_IFACE}"
fi
ip link del "${POD_IFACE}" 2>/dev/null || true
ip link add "${POD_IFACE}" type veth peer name "${TEMP_HOST_IFACE}"
ip link set "${TEMP_HOST_IFACE}" netns "${HOST_NETNS_PID}"

ip link set "${POD_IFACE}" mtu "${MTU}"
ip address replace "${POD_IP}/30" dev "${POD_IFACE}"
ip link set "${POD_IFACE}" up
ip route replace "${CUBE_ROUTER_NAT_IP}/32" \
  via "${HOST_IP}" dev "${POD_IFACE}"

nsenter -t "${HOST_NETNS_PID}" -n -- \
  ip link set "${TEMP_HOST_IFACE}" name "${HOST_IFACE}"
nsenter -t "${HOST_NETNS_PID}" -n -- \
  ip link set "${HOST_IFACE}" alias "${OWNER_ALIAS}"
nsenter -t "${HOST_NETNS_PID}" -n -- \
  ip link set "${HOST_IFACE}" mtu "${MTU}"
nsenter -t "${HOST_NETNS_PID}" -n -- \
  ip address replace "${HOST_IP}/30" dev "${HOST_IFACE}"
nsenter -t "${HOST_NETNS_PID}" -n -- \
  ip link set "${HOST_IFACE}" up

echo 1 > /proc/sys/net/ipv4/ip_forward
