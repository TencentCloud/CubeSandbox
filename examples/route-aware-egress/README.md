# Route-Aware Egress Examples

[中文文档](README_zh.md)

These examples show how to use CubeSandbox with cube-router enabled, so sandbox
egress follows the host Linux routing table instead of always leaving through
the primary NIC.

The examples intentionally keep host networking setup outside the Python code.
GRE tunnels, secondary NICs, cloud routes, security groups, and remote gateway
NAT are environment-specific. The scripts create a sandbox, generate traffic,
and print the host-side commands that should be used to verify the selected
egress path.

## Prerequisites

- CubeSandbox is installed and running.
- cube-router is enabled:

```bash
CUBE_SANDBOX_CUBE_ROUTER_ENABLE=1
```

- A template ID is available.
- Python dependencies are installed:

```bash
pip install -r requirements.txt
```

- Local environment is configured:

```bash
cp .env.example .env
# edit .env
```

## Example 1: Dual NIC

Use this when the Cube node has two NICs:

- `eth0` reaches the public internet through the default route.
- `eth1` reaches an internal target through a more specific host route.

Example host configuration:

```bash
ip route get 1.1.1.1
ip route replace 10.206.0.12/32 via 10.206.0.1 dev eth1 src 10.206.0.15
ip route get 10.206.0.12
```

Example `.env` values:

```bash
export PUBLIC_TARGET_IP="1.1.1.1"
export PUBLIC_TARGET_TCP_PORT="53"
export SECONDARY_NIC_TARGET_IP="10.206.0.12"
# Optional: set when the internal target has a TCP service.
export SECONDARY_NIC_TARGET_TCP_PORT=""
export PRIMARY_NIC_NAME="eth0"
export SECONDARY_NIC_NAME="eth1"
```

Run:

```bash
python dual_nic.py
```

Expected result:

- Sandbox can connect to `PUBLIC_TARGET_IP:PUBLIC_TARGET_TCP_PORT`.
- Sandbox sends a UDP probe to `SECONDARY_NIC_TARGET_IP`; verify the selected
  secondary-NIC path with `tcpdump`.
- If `SECONDARY_NIC_TARGET_TCP_PORT` is set, the script also verifies TCP
  reachability to the internal target.
- `tcpdump -ni cube-router` sees both flows before final host routing.
- Public target packets leave through `eth0`.
- Internal target packets leave through `eth1`.

## Example 2: GRE Tunnel Gateway

Use this when selected sandbox traffic should go through a GRE tunnel, and the
GRE remote node acts as the gateway to the target network or public internet.

Example host assumptions:

- Cube node has a GRE device named `natgre`.
- Cube node GRE address is `169.254.100.1`.
- GRE remote address is `169.254.100.2`.
- Host routing sends selected cube-router NAT traffic into `natgre`.
- GRE remote node has IP forwarding and NAT configured.

Typical checks:

```bash
ip addr show natgre
ip route get 169.254.100.2
tcpdump -ni cube-router 'host 169.254.100.2 or host 1.1.1.1'
tcpdump -ni natgre 'host 169.254.100.2 or host 1.1.1.1'
```

Example `.env` values:

```bash
export GRE_TUNNEL_NAME="natgre"
export GRE_REMOTE_TUNNEL_IP="169.254.100.2"
export GRE_INTERNET_TARGET_IP="1.1.1.1"
export GRE_INTERNET_TARGET_TCP_PORT="53"
export GRE_UNDERLAY_REMOTE_IP="<remote-node-public-or-private-ip>"
```

Run:

```bash
python gre_tunnel_gateway.py
```

Expected result:

- Sandbox sends a UDP probe to the GRE remote tunnel IP; verify the tunnel path
  with `tcpdump`.
- Sandbox can connect to
  `GRE_INTERNET_TARGET_IP:GRE_INTERNET_TARGET_TCP_PORT` through the GRE remote
  gateway.
- `tcpdump` shows packets on `cube-router`, then on the GRE device, then on the
  GRE underlay path.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `missing required environment variable` | `.env` still contains placeholders | Fill in the target IPs |
| Sandbox cannot reach the internal target | Host route does not select the secondary NIC | Check `ip route get <target>` on the Cube node |
| Sandbox reaches GRE peer but not internet | GRE remote is not forwarding/NATing | Check remote `ip_forward` and NAT rules |
| No packets on `cube-router` | cube-router disabled or sandbox was created before network setup changed | Enable cube-router and create a new sandbox |
| `ping: Operation not permitted` | The sandbox image does not grant `CAP_NET_RAW` to ping | Use these examples' TCP/UDP socket probes instead of `ping` |
