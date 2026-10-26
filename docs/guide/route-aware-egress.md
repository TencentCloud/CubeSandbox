# Route-Aware Egress

By default, CubeSandbox keeps the original egress behavior: CubeVS sends sandbox outbound packets directly to the node's primary network interface. This is simple and works well for single-NIC nodes, but it does not let the Linux routing table choose another egress device, such as a secondary NIC, GRE tunnel, VXLAN device, VPN interface, or policy-routed path.

The optional **cube-router** mode changes only the host-side egress path. Sandbox network policy, DNS allow-list learning, CubeEgress L7 proxying, and port mapping keep their existing semantics.

## When To Enable It

Enable cube-router when sandbox traffic should follow host routing instead of always leaving from the primary NIC. Common cases include:

- The node has multiple NICs and different destination CIDRs should leave through different interfaces.
- Some destinations are reachable only through a tunnel device, such as GRE, VXLAN, WireGuard, or an internal VPN.
- You need route-aware sandbox egress without changing the host's global default route.

Keep it disabled when the primary NIC direct path is enough. The default is disabled for upgrade compatibility.

## How It Works

When cube-router is disabled, CubeVS performs the original direct SNAT path:

```mermaid
flowchart LR
    sandbox[Sandbox] --> tap[TAP]
    tap --> fromCube[CubeVS from_cube]
    fromCube --> nic[Primary NIC]
```

When cube-router is enabled, CubeVS first normalizes outbound packets to an internal NAT IP, then injects them into an internal dummy device named `cube-router`. From there, the Linux kernel performs normal forwarding, conntrack, routing lookup, and final MASQUERADE:

```mermaid
flowchart LR
    sandbox[Sandbox] --> tap[TAP]
    tap --> fromCube[CubeVS from_cube]
    fromCube --> router[cube-router]
    router --> kernel[Linux routing table]
    kernel --> egress[Selected egress device]
```

This means CubeSandbox does not need to understand every device type. If Linux can route traffic through that device, sandbox egress can use it.

Port mapping remains bound to the node's primary NIC. It is not exposed on every possible egress device.

## Enable During Installation

Set these variables in `deploy/one-click/.env` before running the one-click installer or upgrade:

```bash
CUBE_SANDBOX_CUBE_ROUTER_ENABLE=1
# Optional. Leave empty to derive addresses from CUBE_SANDBOX_NETWORK_CIDR.
CUBE_SANDBOX_CUBE_ROUTER_CIDR=
```

If `CUBE_SANDBOX_CUBE_ROUTER_CIDR` is empty, CubeSandbox reserves the last two usable IPs from the sandbox CIDR. With the default sandbox CIDR `192.168.0.0/18`, this becomes:

| Address | Purpose |
| --- | --- |
| `192.168.63.253/32` | `cube-router` device IP |
| `192.168.63.254` | internal SNAT IP used before host routing |

If you set a custom cube-router CIDR, it must be an aligned private IPv4 CIDR with a mask between `/16` and `/30`. CubeSandbox uses `.1` as the router IP and `.2` as the internal SNAT IP:

```bash
CUBE_SANDBOX_CUBE_ROUTER_ENABLE=1
CUBE_SANDBOX_CUBE_ROUTER_CIDR=10.254.0.0/24
```

The custom CIDR must not overlap with existing host routes or interface addresses.

When cube-router is enabled for the first time, existing outbound connections from running sandboxes are interrupted. They work normally after the application reconnects through the new cube-router path.

## Verify

After installation, check that the device, route, and NAT rule exist:

```bash
ip addr show cube-router
ip route | grep cube-router
iptables -t nat -S POSTROUTING | grep MASQUERADE
```

Create a sandbox with normal egress policy, then test a destination whose host route should use a non-primary device:

```bash
ip route get <destination-ip>
tcpdump -ni cube-router host <destination-ip>
tcpdump -ni <egress-device> host <destination-ip>
```

You should see the packet first on `cube-router` and then on the device selected by the host route. The packet source on the final egress device should be the egress device's host IP after MASQUERADE.

## Notes

- cube-router does not change sandbox `allow_out`, `deny_out`, or L7 `rules`.
- CubeEgress still receives HTTP/HTTPS traffic selected by CubeVS and the host TPROXY rules.
- Do not change the host's global default route just to test cube-router. Add specific host routes or policy routes for the destinations you want to validate.
- To disable the feature, set `CUBE_SANDBOX_CUBE_ROUTER_ENABLE=0` and run the normal upgrade/reinstall flow so generated config and host networking are reconciled.

## Example 1: GRE Remote As Internet Gateway

This example sends the default egress path through a GRE tunnel, so sandbox internet traffic reaches the public network through the GRE remote node. Private-address traffic, such as `10.0.0.0/8`, `172.16.0.0/12`, and `192.168.0.0/16`, stays on the Cube node's physical NIC.

If you configure this over SSH, keep an explicit route for the GRE underlay peer and your SSH client before replacing the default route.

Example topology:

| Role | Example value |
| --- | --- |
| Cube node underlay IP | `203.0.113.10` |
| Cube node physical NIC | `eth0` |
| Cube node physical gateway | `203.0.113.1` |
| GRE remote underlay IP | `198.51.100.20` |
| GRE tunnel name | `natgre` |
| Cube-side GRE IP | `169.254.100.1/30` |
| Remote-side GRE IP | `169.254.100.2/30` |
| GRE remote internet NIC | `eth0` |

The cloud security group or firewall must allow GRE, which is IP protocol `47`, between the two underlay IPs.

On the GRE remote node:

```bash
#!/usr/bin/env bash
set -euo pipefail

CUBE_UNDERLAY_IP="203.0.113.10"
REMOTE_UNDERLAY_IP="198.51.100.20"
REMOTE_EGRESS_NIC="eth0"
TUNNEL_NAME="natgre"
REMOTE_TUNNEL_CIDR="169.254.100.2/30"

modprobe ip_gre
ip tunnel del "${TUNNEL_NAME}" 2>/dev/null || true
ip tunnel add "${TUNNEL_NAME}" mode gre local "${REMOTE_UNDERLAY_IP}" remote "${CUBE_UNDERLAY_IP}" ttl 255
ip addr replace "${REMOTE_TUNNEL_CIDR}" dev "${TUNNEL_NAME}"
ip link set "${TUNNEL_NAME}" up mtu 1476

sysctl -w net.ipv4.ip_forward=1
iptables -t nat -C POSTROUTING -s 169.254.100.0/30 -o "${REMOTE_EGRESS_NIC}" -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -s 169.254.100.0/30 -o "${REMOTE_EGRESS_NIC}" -j MASQUERADE
```

On the Cube node:

```bash
#!/usr/bin/env bash
set -euo pipefail

CUBE_UNDERLAY_IP="203.0.113.10"
REMOTE_UNDERLAY_IP="198.51.100.20"
UNDERLAY_NIC="eth0"
UNDERLAY_GW="203.0.113.1"
TUNNEL_NAME="natgre"
CUBE_TUNNEL_CIDR="169.254.100.1/30"
SSH_CLIENT_IP=""

modprobe ip_gre
ip tunnel del "${TUNNEL_NAME}" 2>/dev/null || true
ip tunnel add "${TUNNEL_NAME}" mode gre local "${CUBE_UNDERLAY_IP}" remote "${REMOTE_UNDERLAY_IP}" ttl 255
ip addr replace "${CUBE_TUNNEL_CIDR}" dev "${TUNNEL_NAME}"
ip link set "${TUNNEL_NAME}" up mtu 1476

# Keep the GRE underlay path on the physical NIC. This avoids recursive routing
# after the default route is switched to the GRE tunnel.
ip route replace "${REMOTE_UNDERLAY_IP}/32" via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"

# Keep private-address traffic on the physical NIC instead of the GRE internet
# gateway. Adjust these routes if your internal network uses a different layout.
ip route replace 10.0.0.0/8 via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"
ip route replace 172.16.0.0/12 via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"
ip route replace 192.168.0.0/16 via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"

# Optional but recommended when operating over SSH. Set this to your SSH client
# public/source IP to avoid losing the remote session.
if [[ -n "${SSH_CLIENT_IP}" ]]; then
  ip route replace "${SSH_CLIENT_IP}/32" via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"
fi

# Send the default route through the GRE remote gateway.
ip route replace default dev "${TUNNEL_NAME}"
```

Expected host route excerpt:

```bash
default dev natgre scope link
10.0.0.0/8 via 203.0.113.1 dev eth0 src 203.0.113.10
172.16.0.0/12 via 203.0.113.1 dev eth0 src 203.0.113.10
192.168.0.0/16 via 203.0.113.1 dev eth0 src 203.0.113.10
198.51.100.20 via 203.0.113.1 dev eth0 src 203.0.113.10
169.254.100.0/30 dev natgre proto kernel scope link src 169.254.100.1
```

Verify on the Cube node:

```bash
ip addr show natgre
ip route get 1.1.1.1
ip route get 10.1.2.3
tcpdump -ni cube-router 'host 1.1.1.1 or host 169.254.100.2'
tcpdump -ni natgre 'host 1.1.1.1 or host 169.254.100.2'
tcpdump -ni eth0 'proto gre or host 198.51.100.20 or net 10.0.0.0/8'
```

Then create a sandbox whose `allow_out` includes the tunnel endpoint, the internet target, and any private target you want to test, for example `169.254.100.2/32`, `1.1.1.1/32`, and `10.1.2.3/32`. Internet traffic should appear on `cube-router`, leave through `natgre`, and be NATed again by the GRE remote node before reaching the internet. Private-address traffic should appear on `cube-router` and then leave through the physical NIC.

## Example 2: Dual NIC, One For Internet And One For Internal Network

This example keeps the node default route on one NIC for internet access, and routes private-address traffic through another physical NIC. Sandbox traffic follows the same host routing decision after it enters `cube-router`.

Example topology:

| Role | Example value |
| --- | --- |
| Internet NIC | `eth0`, `10.206.0.4` |
| Internet gateway | `10.206.0.1` |
| Internal NIC | `eth1`, `10.206.0.15` |
| Internal gateway | `10.206.0.1` |
| Internet target | `1.1.1.1` |
| Internal target | `10.50.0.12` |

Configure the host routes on the Cube node:

```bash
# Internet traffic uses eth0 by default.
ip route replace default via 10.206.0.1 dev eth0 src 10.206.0.4

# Private-address traffic uses eth1.
ip route replace 10.0.0.0/8 via 10.206.0.1 dev eth1 src 10.206.0.15
ip route replace 172.16.0.0/12 via 10.206.0.1 dev eth1 src 10.206.0.15
ip route replace 192.168.0.0/16 via 10.206.0.1 dev eth1 src 10.206.0.15
```

Expected host route excerpt:

```bash
default via 10.206.0.1 dev eth0 src 10.206.0.4
10.0.0.0/8 via 10.206.0.1 dev eth1 src 10.206.0.15
172.16.0.0/12 via 10.206.0.1 dev eth1 src 10.206.0.15
192.168.0.0/16 via 10.206.0.1 dev eth1 src 10.206.0.15
```

Check the host routing result before testing the sandbox:

```bash
ip route get 1.1.1.1
ip route get 10.50.0.12
```

The first command should select `eth0`; the second should select `eth1`.

Create a sandbox whose `allow_out` includes both targets, for example `1.1.1.1/32` and `10.50.0.12/32`, then test from inside the sandbox:

```bash
ping -c 4 1.1.1.1
ping -c 4 10.50.0.12
```

Verify on the Cube node:

```bash
tcpdump -ni cube-router 'host 1.1.1.1 or host 10.50.0.12'
tcpdump -ni eth0 'host 1.1.1.1 or host 10.50.0.12'
tcpdump -ni eth1 'host 1.1.1.1 or host 10.50.0.12'
```

Expected result:

- Traffic to `1.1.1.1` appears on `cube-router` and then `eth0`.
- Traffic to `10.50.0.12` appears on `cube-router` and then `eth1`.
- The final source IP on each physical NIC is the IP of that NIC after MASQUERADE.
