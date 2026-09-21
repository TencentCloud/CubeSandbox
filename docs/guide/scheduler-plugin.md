# Extensible scheduler plugins

CubeMaster can route each request to a scheduling Profile composed of mandatory safety guards, optional filters, weighted scores, selection settings, and failure policies.

Three built-in Profiles ship inside the binary (`CubeMaster/pkg/base/config/scheduler_factory.yaml`): `burst_balance` and `template_reuse`, selected by the request label `workload=burst_balance` / `workload=template_reuse`, plus `mixed_binpack` as the default for everything else. When the configuration contains neither `scheduler.profiles` nor the legacy `scheduler.filter` / `scheduler.score` / `scheduler.postscore` blocks, this built-in set is injected automatically, so a zero-config deployment still schedules with real strategies. Any explicit `scheduler.profiles` or legacy filter/score/postscore configuration replaces the built-in set entirely; with only legacy filter/score present, it is compiled into a compatible `default` Profile as before.

## Behavior changes when upgrading

Clusters that upgrade without touching their scheduler configuration should be aware of two behavior changes:

- **An empty scheduler configuration now activates the factory Profiles.** Previously, a deployment with neither `scheduler.profiles` nor legacy `scheduler.filter` / `scheduler.score` / `scheduler.postscore` ran without any filtering or scoring and picked a random pre-filter candidate. With the factory set injected, every request passes the mandatory guards (`node_safety`, `cpu`, `mem`, `disk`, `template_locality`, `realtime_create_num`) and is scored by the factory scorers with `spread` / `top_n` selection, so placement decisions differ from the old random pick. CubeMaster logs a warning when it injects the factory set. To keep the previous behavior, set `scheduler.disable_factory_profiles: true` (an explicit opt-out), or configure the legacy `scheduler.filter` / `scheduler.score` blocks (they are compiled into a compatible `default` Profile) or an explicit `scheduler.profiles` section.
- **Template locality scoring is now a boolean 100/0 factor — this is a real behavior change.** Before, the `template_id` factor of `image_score` graded nodes linearly by the total size of the node's local replicas of the template, mapped through a `[23MB, 80GB]` window onto `[0, 100]`; now every node holding the template gets exactly 100 and every other node gets 0, matching the `template_local` fact exposed to CEL and gRPC plugins. (The old linear value was numerically crushed by the resource factors at GiB scale, so the locality preference was already weak in practice — but it was not zero.) How much placement actually shifts depends on which scorers and weights a deployment runs: for the factory `template_reuse` Profile, `image_score` is the dominant 0.7-weight factor, and a legacy cluster that enables only `image_score` (`score.enable_scorers: [image_score]`) can see a material placement change for template requests without touching its configuration.

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

`selection.method` controls how the final node is picked from the scored candidates: `highest` takes the single highest-scored node; `spread` deterministically picks the node with the fewest running sandboxes among the top `top_n` candidates (ties keep score order), pushing placement apart; `random` (the default) draws a score-weighted random node among the top `top_n`. `top_n: -1` widens the candidate window to every node that passed filtering.

Custom Profiles always run the `node_safety`, `cpu`, `mem`, `disk`, `template_locality`, and `realtime_create_num` guards. They cannot be disabled or repeated as optional filters. `node_safety` checks health, metric freshness, the MVM limit, and CPU-load validity on both the normal and backoff paths.

After selection, CubeMaster re-reads the node and performs the normal local admission checks before dispatching to Cubelet. There is no reservation ledger or synchronous Redis reservation on the create path. Metrics update independently, so a visibility gap may remain before the next report; cross-replica admission is therefore advisory and can over-admit during that window. Delete the obsolete `scheduler.reservation_redis_error_policy` setting from existing configuration. Redis remains in use for node metrics and post-create proxy metadata, among other functions.

Multiple Masters still use reported metrics and the `realtime_create_num` estimate of local concurrency multiplied by Master count. They no longer atomically reserve capacity across replicas, so concurrent over-admission remains possible during metric propagation. Cubelet already limits create concurrency and individual sandbox cgroup usage; node-wide atomic quota admission is follow-up work and is not implemented by this change. Do not assume every excess CPU/memory request will be rejected by Cubelet. Concretely, a node that turns out to be full fails the create on the first attempt rather than triggering a retry on another node: Cubelet performs no CPU/memory/MVM quota admission today, and the resulting error codes are not in any retry set. Turning that rejection into a real retryable failover is part of the follow-up work.

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

## Failure semantics

- Mandatory guards are always fail-closed.
- Filters default to `fail-closed`; explicitly configured `fail-open` emits a risk warning.
- Scores default to `default-score`, which substitutes the plugin's `default_score` after a failure; `fail-closed` is also available.
- `no_candidate` supports `fail` and `backoff`. A custom Profile using backoff still reruns its guards, filters, and scores.

Configuration is compiled as one unit at startup or during a hot reload. If a plugin name, route, expression, weight, selection method, or failure policy is invalid, the new Profile set is not activated and the scheduler continues using the previous complete pipeline.

## Built-in scorers and the legacy score tree

Three built-in scorers still read their factor switches from the legacy global `scheduler.score` tree rather than from Profile `args`:

- `real_time_weighted_average` requires `scheduler.score.plugin_conf.real_time_weighted_average` and `scheduler.score.resource_weights`;
- `image_score` requires `scheduler.score.plugin_conf.image_score`;
- `multi_factor_weighted_average` requires `scheduler.score.plugin_conf.multi_factor_weighted_average` (its background refresh loop also runs off that block).

A Profile that references one of these scorers without the required legacy block is **rejected at compile time** (startup or hot reload), so a silently no-op scorer is not possible. The factory Profiles injected on a zero-config deployment carry a matching legacy `scheduler.score` subtree inside `scheduler_factory.yaml` for exactly this reason — when you customize a factory Profile, edit the weights on the Profile entry, but keep (or adjust) that legacy subtree for the factor switches.

A score plugin whose dimension does not apply to a request can return `score.ErrNotApplicable` to be skipped explicitly: it then contributes no scores and no weight and is not treated as a failure, even under `fail-closed` / `default-score` policies. Returning an empty score list with a nil error is instead a contract violation for profile-mode scorers and triggers the configured failure policy. The built-in legacy-coupled scorers (`real_time_weighted_average`, `image_score`, `multi_factor_weighted_average`) report `ErrNotApplicable` when their legacy factor configuration is missing, disabled, or has no non-zero-weighted factor; profile compilation rejects those configurations upfront, so this only fires when the legacy tree changes under a hot reload.
