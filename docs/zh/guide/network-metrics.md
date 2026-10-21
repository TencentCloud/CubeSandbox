# 沙箱网络指标

Cubelet 在以下端点提供 CubeVS 网络数据面的 Prometheus 指标：

```text
http://<cubelet-node>:9998/v1/metrics/network
```

该端点读取固定（pinned）的 eBPF map 和最新的 SNAT 使用量快照。抓取指标不会访问沙箱，也不会向沙箱数据面注入流量。

::: warning 网络访问
Cubelet 的 HTTP 服务本身不提供鉴权或 TLS。仅应在可信的管理网络中开放 `9998` 端口，或通过防火墙、安全组限制访问来源。详见[网络加固](./network-hardening.md)。
:::

## Prometheus 配置

```yaml
scrape_configs:
  - job_name: cubesandbox-network
    metrics_path: /v1/metrics/network
    static_configs:
      - targets:
          - <compute-node-ip>:9998
```

该端点最多同时处理两个抓取请求，额外的并发请求会收到 HTTP 503。

在 Cubelet 节点上验证端点：

```bash
curl -fsS http://127.0.0.1:9998/v1/metrics/network | \
  grep '^cube_cubebox_network_'
```

## 沙箱流量指标

流量指标使用 `sandbox_ip` 标签。数据面首次观察到流量时创建计数器，Cubelet 释放沙箱 TAP 时删除计数器。

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `cube_cubebox_network_sandbox_ingress_packets_total` | Counter | 送入沙箱数据面的报文数。 |
| `cube_cubebox_network_sandbox_ingress_bytes_total` | Counter | 送入沙箱数据面的字节数。 |
| `cube_cubebox_network_sandbox_ingress_drop_packets_total` | Counter | 到达沙箱前被丢弃的入向报文数。 |
| `cube_cubebox_network_sandbox_ingress_drop_bytes_total` | Counter | 到达沙箱前被丢弃的入向字节数。 |
| `cube_cubebox_network_sandbox_egress_packets_total` | Counter | 沙箱数据面发出的报文数。 |
| `cube_cubebox_network_sandbox_egress_bytes_total` | Counter | 沙箱数据面发出的字节数。 |
| `cube_cubebox_network_sandbox_egress_drop_packets_total` | Counter | 数据面丢弃的出向报文数。 |
| `cube_cubebox_network_sandbox_egress_drop_bytes_total` | Counter | 数据面丢弃的出向字节数。 |
| `cube_cubebox_network_sandbox_snat_alloc_failures_total` | Counter | SNAT 源端口分配失败次数。 |

SNAT 分配失败同时属于出向丢包，因此失败计数器和丢包计数器存在重叠，不应直接相加。

## SNAT 指标

SNAT 指标使用 `snat_ip` 标签，数据来自最近一次完成的 session reaper 快照。

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `cube_cubebox_network_snat_sessions_in_use` | Gauge | 使用该 SNAT IP 的活跃会话数。 |
| `cube_cubebox_network_snat_ports_capacity` | Gauge | 每个目的端和协议元组可用的源端口范围大小。 |
| `cube_cubebox_network_snat_ports_unused` | Gauge | 该 SNAT IP 上未被任何活跃会话使用的源端口号数量。 |

`snat_ports_unused` 表示节点 SNAT IP 的整体端口占用。CubeVS 可让不同目的端或协议元组复用同一个源端口号，因此该值不等于某个特定目的端发生分配失败前的精确余量。

## 导出器健康状态

`cube_cubebox_network_exporter_up{source="sandbox"}` 表示是否成功读取沙箱指标 map；同名指标的 `source="snat"` 表示是否存在有效的最新 SNAT 快照。

某个来源的值为 `0` 时，只影响该来源；其他可读的指标族仍会正常导出。

## PromQL 示例

沙箱出向吞吐量：

```promql
rate(cube_cubebox_network_sandbox_egress_bytes_total[5m])
```

沙箱出向丢包速率：

```promql
rate(cube_cubebox_network_sandbox_egress_drop_packets_total[5m])
```

SNAT 源端口占用率：

```promql
1 -
cube_cubebox_network_snat_ports_unused
/
cube_cubebox_network_snat_ports_capacity
```

该比例表示节点 SNAT IP 的整体占用，不表示特定目的端的分配余量。

## 故障排查

| 现象 | 处理方式 |
| --- | --- |
| 无法连接 `9998` | 检查 `cube-sandbox-cubelet.service`、监听地址、防火墙和安全组。 |
| `exporter_up{source="sandbox"}` 为 `0` | 检查 Cubelet 日志中的 CubeVS pinned map 打开或读取错误。 |
| `exporter_up{source="snat"}` 为 `0` | 检查 Cubelet 日志，并确认 CubeVS session reaper 正在发布最新快照。 |
| 运行中的沙箱没有流量指标 | 先产生网络流量再抓取；首次观察到报文时才创建指标条目。 |
| 已删除沙箱的指标仍存在 | 等待 TAP 清理完成，并检查 Cubelet 的清理错误。 |
