# Extensible scheduler plugins

CubeMaster can route each request to a scheduling Profile composed of mandatory safety guards, optional filters, weighted scores, selection settings, and failure policies. If `scheduler.profiles` is absent, the existing filter/score configuration is compiled into a compatible `default` Profile.

## Profile configuration

Only request labels listed in `profile_route_label_keys` may affect routing or be sent to an external plugin. A non-default Profile must have an instance-type or label condition. Routes are evaluated in configuration order and the first match wins.

```yaml
scheduler:
  profile_route_label_keys: [workload]
  profiles:
    - name: burst
      route:
        instance_types: ["S.*", "M.*"]
        labels: {workload: burst}
      filters:
        - name: skip-high-create
          type: expr
          expr: "node.creating < 8"
      scores:
        - name: prefer-idle
          type: expr
          expr: "node.cpu_util < 60.0 ? 80.0 : 20.0"
          weight: 2
      selection: {top_n: 5, method: spread}
      failure:
        filter: fail-closed
        score: default-score
        no_candidate: fail
```

Custom Profiles always run the `node_safety`, `cpu`, `mem`, `disk`, `template_locality`, and `realtime_create_num` guards. They cannot be disabled or repeated as optional filters. `node_safety` checks health, metric freshness, the MVM limit, and CPU-load validity on both the normal and backoff paths.

`selection.method` accepts `random`, `spread`, and `highest`. `spread` currently aliases `random` by design: both pick uniformly from the top-`top_n` scored nodes, and only `highest` always picks the best-scored node. A custom Profile without an explicit `top_n` defaults to 1, making `random`/`spread` a deterministic best-node pick; the legacy `default` Profile uses `priority_select_num` instead. Set `top_n` explicitly when you expect spreading.

Label-based routing is only populated on the sandbox create path. Migration and restore-placement scheduling never set routing labels, so label-routed Profiles are unreachable there and those requests always use the fallback pipeline. Instance-type routes, in contrast, are evaluated on every scheduling request: migrate and restore-placement flows carry no routing labels but still match `instance_types` (restore sets its instance type; migrate's is empty, which `.*` still matches). A broad route such as `instance_types: [".*"]` therefore pulls those non-create flows into a Profile, where expr/gRPC plugins see an empty or zero-valued request. Scope `instance_types` to the types you actually intend to route, and note that the mandatory `cpu`/`mem` guards fail closed — a hard error with no backoff — if a request reaches them without a resource spec.

## Plugin types

- `go` (default): compiled into CubeMaster and registered by name through the unified Registry.
- `expr`: CEL compiled at startup; a Filter must return `bool`, and a Score must return a value from 0 through 100.
- `grpc`: an independent process that completes a protocol/capability handshake at startup. CubeMaster validates timeouts, consecutive-failure circuit breaking, snapshot versions, and returned nodes and scores.

In-process implementations use the existing `filter.Selector` or `score.Selector` interface and register through `plugin.RegisterGoFilter` / `plugin.RegisterGoScore` from package initialization. The CubeMaster binary must import that package and be rebuilt; duplicate names are rejected at startup.

CEL receives strongly typed, read-only, versioned protobuf `node` and `request` objects. Unknown fields, invalid type operations, and invalid return types are rejected when the Profile is activated. Common node fields include `cpu_util`, `cpu_load`, `quota_cpu`, `allocated_cpu`, `quota_mem_mb`, `allocated_mem_mb`, `creating`, `local_creating`, `mvm_num`, `labels`, `local_templates`, `template_local`, and `snapshot_storage_writable`. `reserved` is reserved for future use and currently always 0. Request fields include `instance_type`, `cpu_millis`, `memory_bytes`, `system_disk_size`, `template_id`, and `labels`.

External plugin example:

```yaml
      filters:
        - name: company-policy
          type: grpc
          socket_path: /run/cube/company-scheduler.sock
          timeout: 100ms
          circuit_breaker_failures: 3
          circuit_breaker_cooldown: 30s
```

The versioned protocol is in `pkgs/proto/services/schedulerplugin/v1/plugin.proto`. CubeMaster calls `Handshake`, then `SyncSnapshot`, followed by batched `Filter` or `Score` requests. A Unix Domain Socket is recommended in production. A runnable server is available in `CubeMaster/examples/scheduler-plugin`:

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## Failure semantics

- Mandatory guards are always fail-closed.
- Filters default to `fail-closed`; explicitly configured `fail-open` emits a risk warning.
- Scores default to `default-score`, which substitutes the plugin's `default_score` after a failure; `fail-closed` is also available.
- A built-in `go` scorer that returns an empty result (e.g. `affinity_score` when the request has no node-preference affinity, or `image_score` without applicable resource weights) is treated as "not applicable" and skipped — no error and no `default_score` substitution. A result covering only some candidates is still a failure.
- `no_candidate` supports `fail` and `backoff`. Mandatory guards never trigger backoff: a guard failure — including an emptied candidate set — always fails fast. Backoff applies only when the optional filters or the final selection leave no candidate; a backoff attempt reruns the guards, filters, and scores on the relaxed candidate set, and that guard re-run is the attempt's own safety net.
- `no_candidate` defaults to `fail` when unset. The legacy `default` pipeline always backs off, so adopting the first custom Profile turns "no candidate after filters" into a hard `SelectNodesNoRes` failure — no backoff attempt and no fallback to the default pipeline. Consider explicitly setting `no_candidate: backoff` while adopting Profiles.

Configuration is compiled as one unit at startup or during a hot reload. If a plugin name, route, expression, weight, selection method, or failure policy is invalid, the new Profile set is not activated and the scheduler continues using the previous complete pipeline.

## Operational caveats

- External gRPC plugins are dialed and handshaked synchronously while the Profile set compiles at CubeMaster startup. Any dial or handshake error aborts `InitScheduler` and the process exits, so a plugin that is merely temporarily unavailable — still starting, restarting, or a stale socket — prevents the entire master from booting, including default-profile traffic that never touches the plugin. This is deliberately asymmetric with the hot-reload path, which rejects a broken Profile set and keeps the previous pipeline. Ensure every configured external plugin is up before CubeMaster starts (ordering via systemd, a sidecar, or your supervisor), or do not configure external plugins at all.
- Each external plugin client holds a mutex across the whole snapshot-sync + RPC sequence, intentionally serializing every concurrent request routed through that plugin binding. With the default 100ms `timeout`, one degraded plugin caps the affected create path at roughly 10 requests/s, and a backoff attempt pays a second snapshot freeze and sync. Keep plugin RPCs fast, plan capacity around this ceiling, and treat plugin latency as scheduler latency.
- `cpu_millis` and `memory_bytes` are plain integers in the gRPC and CEL request context, so "no resource spec" is indistinguishable from a zero-size request — the restore-placement path passes an empty spec.

## Compatibility notes

- Legacy `enable_filters` / `enable_scorers` entries that are not registered plugins now abort CubeMaster startup with an error naming the offending entry; previously they were silently skipped. Remove stale entries from the config or register the plugin before starting the new version.
