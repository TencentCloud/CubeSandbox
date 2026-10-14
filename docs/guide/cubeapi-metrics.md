# CubeAPI Business Metrics

CubeAPI exposes Prometheus-formatted business metrics for sandbox creation and deletion:

```text
http://<cube-api-host>:3000/metrics
```

The endpoint is served by CubeAPI itself and uses the Prometheus text exposition format.

## Verify the endpoint

Run the following command from a host that can reach CubeAPI:

```bash
curl -i http://<cube-api-host>:3000/metrics
```

The response uses this content type:

```text
text/plain; version=0.0.4; charset=utf-8
```

A newly started CubeAPI may return only metric metadata or an empty set of samples until a sandbox create or delete operation has completed. This is expected because the current implementation records business operations rather than process health gauges.

## Prometheus scrape configuration

Add a scrape job for the CubeAPI address:

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

Replace the target with the internal address of the CubeAPI service. Keep the scrape target on the same trusted network as CubeAPI.


## Metric families

### `cubeapi_business_request_total`

A Counter containing the number of completed business operations.

Labels:

- `operation` — `sandbox_create` or `sandbox_destroy`.
- `result` — `success` or `fail`.
- `code` — `0` for success, or the mapped application error code for failures.

The currently mapped failure codes are:

| Code | Meaning |
| ---: | --- |
| `400` | Bad request |
| `401` | Unauthorized |
| `404` | Not found |
| `409` | Conflict |
| `429` | Too many requests |
| `500` | Internal error |
| `501` | Not implemented |
| `503` | Service unavailable |

Example:

```text
cubeapi_business_request_total{code="0",operation="sandbox_create",result="success"} 12
cubeapi_business_request_total{code="404",operation="sandbox_destroy",result="fail"} 2
```

### `cubeapi_business_request_duration_seconds`

A Histogram containing the duration of completed business operations in seconds. Prometheus exposes the usual histogram samples:

```text
cubeapi_business_request_duration_seconds_bucket{...}
cubeapi_business_request_duration_seconds_sum{...}
cubeapi_business_request_duration_seconds_count{...}
```

The histogram uses the same `operation`, `result`, and `code` labels as `cubeapi_business_request_total`.

## Current collection scope

The current CubeAPI implementation records only these operations:

| Operation | Recorded when |
| --- | --- |
| `sandbox_create` | `POST /sandboxes` completes successfully or fails. |
| `sandbox_destroy` | `DELETE /sandboxes/:sandboxID` completes successfully or fails. |


## PromQL examples

### Sandbox creation rate

```promql
rate(cubeapi_business_request_total{operation="sandbox_create"}[5m])
```

### Failed sandbox operations

```promql
sum by (operation, code) (
  rate(cubeapi_business_request_total{result="fail"}[5m])
)
```

### P95 sandbox creation latency

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

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| `curl` cannot connect | Confirm CubeAPI is running, port `3000` is reachable from the Prometheus network, and any firewall or security-group rule allows the request. |
| HTTP 200 but no operation samples | Perform a sandbox create or delete operation and scrape again. The current endpoint does not export sandbox operation samples until an operation is completed. |
| Prometheus returns authentication or network errors | Check whether an ingress, firewall, or internal proxy is restricting `/metrics`; the CubeAPI route itself is not protected by the normal API authentication middleware. |
| Counters reset after restart | The registry is in-memory. A CubeAPI restart creates a new registry, so Prometheus should treat the counters as a normal counter reset. |
