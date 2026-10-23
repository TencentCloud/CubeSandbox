# CubeAPI 业务指标

CubeAPI 为沙箱创建和删除操作提供 Prometheus 格式的业务指标，端点为：

```text
http://<cube-api-host>:3000/metrics
```

该端点由 CubeAPI 自身提供，使用 Prometheus 文本 exposition 格式。

## 验证端点

在能够访问 CubeAPI 的主机上执行：

```bash
curl -i http://<cube-api-host>:3000/metrics
```

响应使用以下 Content-Type：

```text
text/plain; version=0.0.4; charset=utf-8
```

CubeAPI 刚启动时，响应中可能只有指标元数据，或者暂时没有样本，直到完成一次沙箱创建或删除操作。这是预期行为，因为当前实现记录的是业务操作，而不是进程健康状态 Gauge。

## Prometheus 抓取配置

为 CubeAPI 地址增加一个 Prometheus scrape job：

```yaml
scrape_configs:
  - job_name: cubesandbox-cube-api
    metrics_path: /metrics
    scrape_interval: 30s
    scrape_timeout: 10s
    static_configs:
      - targets:
          - <cube-api-host>:3000
```

将 target 替换为 CubeAPI Service 的内部地址，并确保抓取目标位于 CubeAPI 所在的可信网络中。


## 指标族

### `cubeapi_business_request_total`

Counter，表示已完成业务操作的次数。

标签如下：

- `operation` — `sandbox_create` 或 `sandbox_destroy`。
- `result` — `success` 或 `fail`。
- `code` — 成功时为 `0`，失败时为映射后的应用错误码。

当前映射的失败错误码如下：

| 错误码 | 含义 |
| ---: | --- |
| `400` | 请求错误 |
| `401` | 未授权 |
| `404` | 未找到 |
| `409` | 冲突 |
| `429` | 请求过多 |
| `500` | 内部错误 |
| `501` | 未实现 |
| `503` | 服务不可用 |

示例：

```text
cubeapi_business_request_total{code="0",operation="sandbox_create",result="success"} 12
cubeapi_business_request_total{code="404",operation="sandbox_destroy",result="fail"} 2
```

### `cubeapi_business_request_duration_seconds`

Histogram，表示已完成业务操作的耗时，单位为秒。Prometheus 会导出标准 Histogram 样本：

```text
cubeapi_business_request_duration_seconds_bucket{...}
cubeapi_business_request_duration_seconds_sum{...}
cubeapi_business_request_duration_seconds_count{...}
```

该 Histogram 使用与 `cubeapi_business_request_total` 相同的 `operation`、`result` 和 `code` 标签。

## 当前采集范围

当前 CubeAPI 实现只记录以下操作：

| 操作 | 记录时机 |
| --- | --- |
| `sandbox_create` | `POST /sandboxes` 成功完成或失败时。 |
| `sandbox_destroy` | `DELETE /sandboxes/:sandboxID` 成功完成或失败时。 |


## PromQL 示例

### 沙箱创建速率

```promql
rate(cubeapi_business_request_total{operation="sandbox_create"}[5m])
```

### 失败的沙箱操作

```promql
sum by (operation, code) (
  rate(cubeapi_business_request_total{result="fail"}[5m])
)
```

### 沙箱创建 P95 延迟

```promql
histogram_quantile(
  0.95,
  sum by (le) (
    rate(cubeapi_business_request_duration_seconds_bucket{
      operation="sandbox_create"
    }[5m])
  )
)
```

## 故障排查

| 现象 | 检查项 |
| --- | --- |
| `curl` 无法连接 | 确认 CubeAPI 正常运行，端口 `3000` 可从 Prometheus 网络访问，并确认防火墙或安全组允许请求。 |
| HTTP 200 但没有业务样本 | 先执行一次沙箱创建或删除操作，再次抓取。当前端点只有在业务操作完成后才会导出对应样本。 |
| Prometheus 报鉴权或网络错误 | 检查 Ingress、防火墙或内部代理是否限制了 `/metrics`；CubeAPI 路由本身不经过普通 API 鉴权中间件。 |
| 重启后 Counter 清零 | 指标 registry 保存在内存中。CubeAPI 重启会创建新的 registry，Prometheus 应将其视为普通 Counter reset。 |
