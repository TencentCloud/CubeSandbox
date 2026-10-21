# Sandbox Network Metrics

Cubelet exposes Prometheus metrics for the CubeVS network datapath at:

```text
http://<cubelet-node>:9998/v1/metrics/network
```

The endpoint reads pinned eBPF maps and the latest SNAT usage snapshot. Scraping it does not contact sandboxes or add packets to their data path.

::: warning Network access
Cubelet's HTTP service has no built-in authentication or TLS. Expose port `9998` only on a trusted management network, or restrict it with firewall or security-group rules. See [Network Hardening](./network-hardening.md).
:::

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: cubesandbox-network
    metrics_path: /v1/metrics/network
    static_configs:
      - targets:
          - <compute-node-ip>:9998
```

The endpoint processes at most two scrapes concurrently. Additional concurrent requests receive HTTP 503.

Verify the endpoint on a Cubelet node:

```bash
curl -fsS http://127.0.0.1:9998/v1/metrics/network | \
  grep '^cube_cubebox_network_'
```

## Sandbox traffic metrics

Traffic series use the `sandbox_ip` label. Counters are created when the datapath first observes traffic and are removed when Cubelet releases the sandbox TAP.

| Metric | Type | Meaning |
| --- | --- | --- |
| `cube_cubebox_network_sandbox_ingress_packets_total` | Counter | Packets delivered to the sandbox datapath. |
| `cube_cubebox_network_sandbox_ingress_bytes_total` | Counter | Bytes delivered to the sandbox datapath. |
| `cube_cubebox_network_sandbox_ingress_drop_packets_total` | Counter | Ingress packets dropped before reaching the sandbox. |
| `cube_cubebox_network_sandbox_ingress_drop_bytes_total` | Counter | Ingress bytes dropped before reaching the sandbox. |
| `cube_cubebox_network_sandbox_egress_packets_total` | Counter | Packets emitted from the sandbox datapath. |
| `cube_cubebox_network_sandbox_egress_bytes_total` | Counter | Bytes emitted from the sandbox datapath. |
| `cube_cubebox_network_sandbox_egress_drop_packets_total` | Counter | Egress packets dropped by the datapath. |
| `cube_cubebox_network_sandbox_egress_drop_bytes_total` | Counter | Egress bytes dropped by the datapath. |
| `cube_cubebox_network_sandbox_snat_alloc_failures_total` | Counter | SNAT source-port allocation failures. |

An SNAT allocation failure is also an egress drop, so the failure and drop counters overlap and must not be added together.

## SNAT metrics

SNAT series use the `snat_ip` label and come from the latest completed session-reaper snapshot.

| Metric | Type | Meaning |
| --- | --- | --- |
| `cube_cubebox_network_snat_sessions_in_use` | Gauge | Live sessions using the SNAT IP. |
| `cube_cubebox_network_snat_ports_capacity` | Gauge | Source-port range size available to each destination/protocol tuple. |
| `cube_cubebox_network_snat_ports_unused` | Gauge | Source-port numbers not used by any live session on the SNAT IP. |

`snat_ports_unused` is a node-IP-wide occupancy view. CubeVS can reuse one source-port number for different destination/protocol tuples, so this gauge is not the exact headroom before allocation failure for a particular destination.

## Exporter health

`cube_cubebox_network_exporter_up{source="sandbox"}` reports whether the sandbox metrics map was read successfully. The same metric with `source="snat"` reports whether a fresh SNAT snapshot was available.

A value of `0` affects only that source. Other readable metric families remain available.

## PromQL examples

Sandbox egress throughput:

```promql
rate(cube_cubebox_network_sandbox_egress_bytes_total[5m])
```

Sandbox packet drop rate:

```promql
rate(cube_cubebox_network_sandbox_egress_drop_packets_total[5m])
```

SNAT source-port occupancy:

```promql
1 -
cube_cubebox_network_snat_ports_unused
/
cube_cubebox_network_snat_ports_capacity
```

Treat this ratio as node-IP-wide occupancy, not destination-specific allocation headroom.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| Cannot connect to port `9998` | Check `cube-sandbox-cubelet.service`, the listener address, firewall, and security-group rules. |
| `exporter_up{source="sandbox"}` is `0` | Check Cubelet logs for pinned CubeVS map open or read errors. |
| `exporter_up{source="snat"}` is `0` | Check Cubelet logs and confirm the CubeVS session reaper is publishing fresh snapshots. |
| A running sandbox has no traffic series | Generate traffic and scrape again; the entry is created on the first observed packet. |
| A deleted sandbox still appears | Wait for TAP cleanup to finish and inspect Cubelet cleanup errors. |
