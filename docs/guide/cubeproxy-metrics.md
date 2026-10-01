# CubeProxy Request Metrics

CubeProxy exposes Prometheus-formatted request metrics for selected sandbox data-plane routes. These metrics help operators observe request traffic, in-flight requests, success and failure rates, and request latency while diagnosing routing or backend problems.

The metrics endpoint is served by a dedicated CubeProxy listener:

```text
http://127.0.0.1:18082/metrics
```

CubeProxy stores the metrics in an in-memory Nginx shared dictionary. A CubeProxy restart resets the counters, gauges, and histogram samples.

## Metrics listener

The metrics listener is configured with the `CUBE_PROXY_METRICS_LISTEN` environment variable. Its default value is:

```text
127.0.0.1:18082
```

For Prometheus to scrape CubeProxy from another host, configure the listener with an address reachable from the Prometheus network. For example:

```text
CUBE_PROXY_METRICS_LISTEN=0.0.0.0:18082
```

The metrics listener exposes only the exact `GET /metrics` endpoint. Other paths return HTTP 404. The endpoint does not use CubeProxy's normal data-plane routes or admin server.

::: warning Network access
The metrics listener does not provide built-in authentication or TLS. Do not expose it to an untrusted network. Bind it to a trusted management address and restrict access with firewall or security-group rules.
:::

## Prometheus scrape configuration

The endpoint can be scraped with a manual Prometheus job:

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

Replace the target with the internal CubeProxy address that is reachable from Prometheus. If CubeProxy keeps the default listener `127.0.0.1:18082`, Prometheus must run on the same host or use a local scrape agent.

## Metric families

### `cube_proxy_request_total`

A Counter containing the number of completed tracked requests.

Labels:

- `route` — the normalized CubeProxy route.
- `method` — `GET` or `POST`.
- `result` — `success` or `fail`.

A response with an HTTP status from `200` through `399` is recorded as `success`. Other status codes are recorded as `fail`.

### `cube_proxy_inflight_requests`

A Gauge containing the number of currently tracked requests that have started but have not yet been completed.

Labels:

- `route` — the normalized CubeProxy route.

### `cube_proxy_request_duration_seconds`

A Histogram containing the duration of completed tracked requests in seconds. Prometheus exposes the usual histogram samples:

```text
cube_proxy_request_duration_seconds_bucket{...}
cube_proxy_request_duration_seconds_sum{...}
cube_proxy_request_duration_seconds_count{...}
```

The histogram uses the `route`, `method`, and `result` labels. The current buckets range from `0.001` seconds through `8.192` seconds, plus `+Inf`.

## Current collection scope

The current implementation records only these normalized routes:

| Route | URI forms | Typical use |
| --- | --- | --- |
| `sandbox_exec` | `/process.Process/*` and `/sandbox/<sandbox-id>/<container-port>/process.Process/*` | Sandbox process execution and connection requests. |
| `sandbox_files` | `/files` and `/sandbox/<sandbox-id>/<container-port>/files` | Sandbox file requests. |

Other URIs are not included in these metrics. The route label is intentionally normalized so sandbox IDs and container ports do not create a separate time series for every sandbox.


## Request lifecycle

CubeProxy starts tracking a request during its rewrite phase. It increments the route's in-flight gauge and stores the normalized route in the request variables.

At request completion, CubeProxy reads the method, final status, and Nginx request time, then updates the counter and histogram and decrements the in-flight gauge.

Lua-generated responses, such as validation or routing failures, are recorded with the explicit status passed to the response helper. Backend peer-selection failures are recorded as HTTP 503 failures. A per-request completion marker prevents the same request from being recorded twice when both an early response path and the log phase run.

## Verify the endpoint

From a host that can reach the listener, query the endpoint:

```bash
curl -fsS http://127.0.0.1:18082/metrics
```

A newly started CubeProxy may expose only HELP and TYPE metadata or no request samples. Complete a tracked sandbox execution or file request before checking the corresponding counter and histogram samples.

## PromQL examples

### Tracked request rate

```promql
sum by (route, method, result) (
  rate(cube_proxy_request_total[5m])
)
```

### Failed request rate

```promql
sum by (route, method) (
  rate(cube_proxy_request_total{result="fail"}[5m])
)
```

### Current in-flight requests

```promql
sum by (route) (
  cube_proxy_inflight_requests
)
```

### P95 request latency

```promql
histogram_quantile(
  0.95,
  sum by (le, route, method) (
    rate(cube_proxy_request_duration_seconds_bucket[5m])
  )
)
```

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| `curl` cannot connect | Confirm CubeProxy is running, check `CUBE_PROXY_METRICS_LISTEN`, and verify that the listener address and firewall rules allow the request. |
| Prometheus cannot reach the endpoint | The default listener is loopback-only. Set `CUBE_PROXY_METRICS_LISTEN` to an internal address reachable from Prometheus and restart CubeProxy. |
| HTTP 404 from the metrics listener | Use the exact path `/metrics`. Other paths on the dedicated metrics listener intentionally return 404. |
| HTTP 200 but no request samples | Complete a tracked `sandbox_exec` or `sandbox_files` request and scrape again. Metadata may be present before the first sample. |
| Expected route is missing | Only the normalized `sandbox_exec` and `sandbox_files` routes are currently tracked. Other CubeProxy routes are outside the current collection scope. |
| Counters reset after restart | Metrics are stored in memory. A CubeProxy restart creates a new shared dictionary, so Prometheus should treat the values as normal counter and gauge resets. |
| Prometheus reports authentication errors | The metrics listener has no built-in authentication. Check the surrounding firewall, security group, proxy, or network policy configuration. |
