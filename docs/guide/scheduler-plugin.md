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
          expr: "node.creating + node.reserved < 8"
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

## Plugin types

- `go` (default): compiled into CubeMaster and registered by name through the unified Registry.
- `expr`: CEL compiled at startup; a Filter must return `bool`, and a Score must return a value from 0 through 100.
- `grpc`: an independent process that completes a protocol/capability handshake at startup. CubeMaster validates timeouts, consecutive-failure circuit breaking, snapshot versions, and returned nodes and scores.

In-process implementations use the existing `filter.Selector` or `score.Selector` interface and register through `plugin.RegisterGoFilter` / `plugin.RegisterGoScore` from package initialization. The CubeMaster binary must import that package and be rebuilt; duplicate names are rejected at startup.

CEL receives strongly typed, read-only, versioned protobuf `node` and `request` objects. Unknown fields, invalid type operations, and invalid return types are rejected when the Profile is activated. Common node fields include `cpu_util`, `cpu_load`, `quota_cpu`, `allocated_cpu`, `quota_mem_mb`, `allocated_mem_mb`, `creating`, `local_creating`, `reserved`, `mvm_num`, `labels`, `local_templates`, `template_local`, and `snapshot_storage_writable`. Request fields include `instance_type`, `cpu_millis`, `memory_bytes`, `system_disk_size`, `template_id`, and `labels`.

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

The versioned protocol is in `CubeMaster/api/services/schedulerplugin/v1/plugin.proto`. CubeMaster calls `Handshake`, then `SyncSnapshot`, followed by batched `Filter` or `Score` requests. A Unix Domain Socket is recommended in production. A runnable server is available in `CubeMaster/examples/scheduler-plugin`:

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## Failure semantics

- Mandatory guards are always fail-closed.
- Filters default to `fail-closed`; explicitly configured `fail-open` emits a risk warning.
- Scores default to `default-score`, which substitutes the plugin's `default_score` after a failure; `fail-closed` is also available.
- `no_candidate` supports `fail` and `backoff`. A custom Profile using backoff still reruns its guards, filters, and scores.

Configuration is compiled as one unit at startup or during a hot reload. If a plugin name, route, expression, weight, selection method, or failure policy is invalid, the new Profile set is not activated and the scheduler continues using the previous complete pipeline.
