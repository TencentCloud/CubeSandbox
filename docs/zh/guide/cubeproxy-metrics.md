# CubeProxy 请求指标

CubeProxy 为选定的沙箱数据面路由提供 Prometheus 格式的请求指标。这些指标用于观测请求流量、并发请求、成功率、失败率和请求延迟，帮助定位路由或后端问题。

指标端点由 CubeProxy 的独立监听器提供：

```text
http://127.0.0.1:18082/metrics
```

CubeProxy 将指标保存在 Nginx 内存共享字典中。CubeProxy 重启后，Counter、Gauge 和 Histogram 样本都会重置。

## 指标监听器

通过环境变量 `CUBE_PROXY_METRICS_LISTEN` 配置指标监听器。默认值为：

```text
127.0.0.1:18082
```

如果 Prometheus 从其他主机抓取 CubeProxy，需要将监听器配置为 Prometheus 网络可访问的地址。例如：

```text
CUBE_PROXY_METRICS_LISTEN=0.0.0.0:18082
```

指标监听器只提供精确的 `GET /metrics` 端点。其他路径返回 HTTP 404。该端点不使用 CubeProxy 的普通数据面路由，也不使用管理服务器。

::: warning 网络访问
指标监听器本身不提供鉴权或 TLS。不要将其暴露在不可信网络中。应将监听地址绑定到可信管理地址，并通过防火墙或安全组限制访问来源。
:::

## Prometheus 抓取配置

可以使用手工 Prometheus 抓取任务：

```yaml
scrape_configs:
  - job_name: cubesandbox-cube-proxy
    metrics_path: /metrics
    scrape_interval: 30s
    scrape_timeout: 10s
    static_configs:
      - targets:
          - <cube-proxy-host>:18082
```

将目标地址替换为 Prometheus 可以访问的 CubeProxy 内部地址。如果 CubeProxy 保持默认监听地址 `127.0.0.1:18082`，Prometheus 必须运行在同一主机上，或使用本地抓取代理。

## 指标族

### `cube_proxy_request_total`

Counter，表示已完成且被统计的请求次数。

标签如下：

- `route` — CubeProxy 归一化后的路由名称。
- `method` — `GET` 或 `POST`。
- `result` — `success` 或 `fail`。

HTTP 状态码为 `200` 至 `399` 的响应记录为 `success`，其他状态码记录为 `fail`。

### `cube_proxy_inflight_requests`

Gauge，表示已经开始但尚未完成的被统计请求数量。

标签如下：

- `route` — CubeProxy 归一化后的路由名称。

### `cube_proxy_request_duration_seconds`

Histogram，表示已完成且被统计的请求耗时，单位为秒。Prometheus 会导出标准 Histogram 样本：

```text
cube_proxy_request_duration_seconds_bucket{...}
cube_proxy_request_duration_seconds_sum{...}
cube_proxy_request_duration_seconds_count{...}
```

该 Histogram 使用 `route`、`method` 和 `result` 标签。当前 bucket 的范围从 `0.001` 秒到 `8.192` 秒，并包含 `+Inf`。

## 当前采集范围

当前实现只记录以下归一化路由：

| 路由 | URI 形式 | 典型用途 |
| --- | --- | --- |
| `sandbox_exec` | `/process.Process/*` 和 `/sandbox/<sandbox-id>/<container-port>/process.Process/*` | 沙箱进程执行和连接请求。 |
| `sandbox_files` | `/files` 和 `/sandbox/<sandbox-id>/<container-port>/files` | 沙箱文件请求。 |

其他 URI 不会计入这些指标。路由标签经过归一化处理，因此不会因为每个沙箱的 ID 和容器端口不同而创建独立的时间序列。


## 请求生命周期

CubeProxy 在 rewrite 阶段开始统计请求，增加对应路由的 in-flight Gauge，并将归一化后的路由写入请求变量。

请求结束时，CubeProxy 读取 method、最终状态码和 Nginx 请求耗时，然后更新 Counter 和 Histogram，并减少 in-flight Gauge。

Lua 生成的响应（例如校验失败或路由失败）会使用响应辅助函数传入的明确状态码记录。后端 peer 选择失败会记录为 HTTP 503 失败。每个请求都有完成标记，避免请求同时经过提前响应路径和 log 阶段时被重复记录。

## 验证指标端点

在能够访问监听地址的主机上请求端点：

```bash
curl -fsS http://127.0.0.1:18082/metrics
```

CubeProxy 刚启动时，响应中可能只有 HELP 和 TYPE 元数据，或者暂时没有请求样本。完成一次被统计的沙箱执行或文件请求后，再检查对应的 Counter 和 Histogram 样本。

## PromQL 示例

### 被统计请求速率

```promql
sum by (route, method, result) (
  rate(cube_proxy_request_total[5m])
)
```

### 失败请求速率

```promql
sum by (route, method) (
  rate(cube_proxy_request_total{result="fail"}[5m])
)
```

### 当前并发请求数

```promql
sum by (route) (
  cube_proxy_inflight_requests
)
```

### 请求 P95 延迟

```promql
histogram_quantile(
  0.95,
  sum by (le, route, method) (
    rate(cube_proxy_request_duration_seconds_bucket[5m])
  )
)
```

## 故障排查

| 现象 | 检查项 |
| --- | --- |
| `curl` 无法连接 | 确认 CubeProxy 正常运行，检查 `CUBE_PROXY_METRICS_LISTEN`，并确认监听地址和防火墙规则允许请求。 |
| Prometheus 无法访问端点 | 默认监听器只绑定本机回环地址。将 `CUBE_PROXY_METRICS_LISTEN` 设置为 Prometheus 可访问的内部地址，然后重启 CubeProxy。 |
| metrics listener 返回 HTTP 404 | 使用精确路径 `/metrics`。独立指标监听器上的其他路径会按设计返回 404。 |
| HTTP 200 但没有请求样本 | 完成一次被统计的 `sandbox_exec` 或 `sandbox_files` 请求后再次抓取。首次抓取前可能只有元数据。 |
| 期望的路由没有指标 | 当前只统计归一化后的 `sandbox_exec` 和 `sandbox_files` 路由，其他 CubeProxy 路由不在当前采集范围内。 |
| 重启后 Counter 清零 | 指标保存在内存中。CubeProxy 重启会创建新的共享字典，Prometheus 应将其视为普通 Counter 和 Gauge reset。 |
| Prometheus 报鉴权错误 | 指标监听器没有内置鉴权。检查外围防火墙、安全组、代理或网络策略配置。 |
