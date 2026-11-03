# Extensible scheduler plugins

CubeMaster routes each request to a scheduling **Profile** composed of mandatory safety guards, optional filters, weighted scores, selection settings, and failure policies. Which profile serves a request is decided by the [strategy selection rules](./cubemaster-scheduler-config.md#which-scheduling-strategy-is-active); this page covers how to configure and extend profiles.

## Upgrade behavior changes

Clusters that upgrade without touching their scheduler configuration are affected by two behavior changes:

| Change | Who is affected | How to keep the old behavior |
|---|---|---|
| **Empty scheduler config now activates the factory Profiles.** A zero-config deployment used to run without filtering or scoring, picking a random pre-filter candidate. Now every request passes the mandatory guards and the factory scorers with spread selection. CubeMaster logs a warning when it injects the factory set. | Every deployment with neither `scheduler.profiles` nor legacy `scheduler.filter` / `scheduler.score` / `scheduler.postscore` | Set `scheduler.disable_factory_profiles: true`, or configure any legacy block (compiled into a compatible `default` Profile) |
| **Template locality scoring is boolean 100/0.** The `template_id` factor of `image_score` used to grade nodes linearly by local replica size over a `[23MB, 80GB]` window; now every node holding the template scores 100, others 0, matching the `template_local` fact exposed to CEL and gRPC plugins. (The old linear value was numerically crushed by resource factors at GiB scale, so the preference was already weak — but not zero.) | Deployments running `image_score`: the factory `template_reuse` Profile (0.7-weight factor) and legacy clusters with `enable_scorers: [image_score]` may see material placement changes for template requests | No opt-out; tune the scorer weights if placement shifts undesirably |

## Factory Profiles

Three built-in Profiles ship inside the binary (`CubeMaster/pkg/base/config/scheduler_factory.yaml`):

- `burst_balance` (label `workload=burst_balance`) — spreads high-rate, similarly sized requests across nodes by resource watermark and in-flight creates;
- `template_reuse` (label `workload=template_reuse`) — prefers nodes already holding a local replica of the template;
- `mixed_binpack` (default) — tighter placement that preserves entirely free nodes.

With no `scheduler.profiles` and no legacy filter/score/postscore blocks, this set is injected automatically so a zero-config deployment still schedules with real strategies. Any explicit configuration replaces it entirely; with only legacy blocks present, they are compiled into a compatible `default` Profile as before.

## Configuring a Profile

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

`selection.method` controls how the final node is picked from the scored candidates: `highest` takes the single highest-scored node; `spread` deterministically picks the node with the fewest running sandboxes among the top `top_n` candidates (ties keep score order), pushing placement apart; `random` (the default) draws a score-weighted random node among the top `top_n`. `top_n: -1` widens the candidate window to every node that passed filtering.

Custom Profiles always run the `node_safety`, `cpu`, `mem`, `disk`, `template_locality`, and `realtime_create_num` guards. They cannot be disabled or repeated as optional filters. `node_safety` checks health, metric freshness, the MVM limit, and CPU-load validity on both the normal and backoff paths.

## Verifying which profile served a request

Every scheduler metric carries the routed profile as the `profile` label. The quickest checks:

- `scheduler_attempts_total{profile=...}` — which profile is receiving traffic (unrouted failures land in `profile="default"`);
- `scheduler_plugin_duration_seconds{profile,kind,plugin}` — per-plugin latency, useful when tuning weights;
- `sandbox_create_duration_seconds{profile,result}` — end-to-end create latency per profile.

## Failure semantics

- Mandatory guards are always fail-closed.
- Filters default to `fail-closed`; explicitly configured `fail-open` emits a risk warning.
- Scores default to `default-score`, which substitutes the plugin's `default_score` after a failure; `fail-closed` is also available.
- `no_candidate` supports `fail` and `backoff`. A custom Profile using backoff still reruns its guards, filters, and scores.

Configuration is compiled as one unit at startup or during a hot reload. If a plugin name, route, expression, weight, selection method, or failure policy is invalid, the new Profile set is not activated and the scheduler continues using the previous complete pipeline.

## Admission and reservations

The reservation ledger and synchronous Redis reservation check have been removed from the create path. Cross-replica admission is advisory: during the metric reporting window multiple Masters can admit against the same apparent capacity, and a node that turns out to be full fails the create on the first attempt instead of failing over to another node. Delete the obsolete `scheduler.reservation_redis_error_policy` setting. See [Reservations and Redis](./multi-node-deploy.md#reservations-and-redis) for the full semantics, upgrade caveats, and the follow-up work.

## Built-in scorers and the legacy score tree

Three built-in scorers still read their factor switches from the legacy global `scheduler.score` tree rather than from Profile `args`:

- `real_time_weighted_average` requires `scheduler.score.plugin_conf.real_time_weighted_average` and `scheduler.score.resource_weights`;
- `image_score` requires `scheduler.score.plugin_conf.image_score`;
- `multi_factor_weighted_average` requires `scheduler.score.plugin_conf.multi_factor_weighted_average` (its background refresh loop also runs off that block).

A Profile that references one of these scorers without the required legacy block is **rejected at compile time** (startup or hot reload), so a silently no-op scorer is not possible. The factory Profiles embed a matching legacy `scheduler.score` subtree inside `scheduler_factory.yaml` for exactly this reason — when you customize a factory Profile, edit the weights on the Profile entry, but keep (or adjust) that legacy subtree for the factor switches.

## Plugin types

- `go` (default): compiled into CubeMaster and registered by name through the unified Registry.
- `expr`: CEL compiled at startup; a Filter must return `bool`, and a Score must return a value from 0 through 100.
- `grpc`: an independent process that completes a protocol/capability handshake at startup. CubeMaster validates timeouts, consecutive-failure circuit breaking, snapshot versions, and returned nodes and scores.

In-process implementations use the existing `filter.Selector` or `score.Selector` interface and register through `plugin.RegisterGoFilter` / `plugin.RegisterGoScore` from package initialization. The CubeMaster binary must import that package and be rebuilt; duplicate names are rejected at startup.

CEL receives strongly typed, read-only, versioned protobuf `node` and `request` objects. Unknown fields, invalid type operations, and invalid return types are rejected when the Profile is activated. Common node fields include `cpu_util`, `cpu_load`, `quota_cpu`, `allocated_cpu`, `quota_mem_mb`, `allocated_mem_mb`, `creating`, `local_creating`, `mvm_num`, `labels`, `local_templates`, `template_local`, and `snapshot_storage_writable`. Request fields include `instance_type`, `cpu_millis`, `memory_bytes`, `system_disk_size`, `template_id`, and `labels`.

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

The versioned protocol is in `pkgs/proto/services/schedulerplugin/v1/plugin.proto`. CubeMaster calls `Handshake` once at startup, then batched `Filter` or `Score` requests. Every request carries the full frozen candidate snapshot for its `snapshot_version`, so plugin servers are stateless and concurrent scheduling attempts can share one connection without serializing. A Unix Domain Socket is recommended in production. A runnable server is available in `CubeMaster/examples/scheduler-plugin`:

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## Plugin developer contract notes

Two contract details matter when writing a Score plugin:

- A score plugin whose dimension does not apply to a request can return `score.ErrNotApplicable` to be skipped explicitly: it then contributes no scores and no weight and is not treated as a failure, even under `fail-closed` / `default-score` policies.
- Returning an empty score list with a nil error is instead a contract violation for profile-mode scorers and triggers the configured failure policy. The built-in legacy-coupled scorers report `ErrNotApplicable` when their legacy factor configuration is missing, disabled, or has no non-zero-weighted factor.
