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

In-process implementations use the existing `filter.Selector` or `score.Selector` interface and register through `plugin.RegisterGoFilter` / `plugin.RegisterGoScore` from package initialization. The CubeMaster binary must import that package and be rebuilt; duplicate names are rejected at startup. Note the concurrency contract: every pipeline — including the legacy default one — runs its filters and scorers concurrently within one request, each plugin seeing the full candidate pool (filter results are intersected only after every filter completes), and different requests always run concurrently, so registered implementations must be read-only and thread-safe — the in-tree built-ins are. The pre-plugin scheduler ran scorers sequentially.

CEL receives strongly typed, read-only, versioned protobuf `node` and `request` objects. Unknown fields, invalid type operations, and invalid return types are rejected when the Profile is activated. Common node fields include `cpu_util`, `cpu_load`, `quota_cpu`, `allocated_cpu`, `quota_mem_mb`, `allocated_mem_mb`, `creating`, `local_creating`, `mvm_num`, `labels`, `local_templates`, `template_local`, and `snapshot_storage_writable`. `reserved` is reserved for future use and currently always 0. Request fields include `instance_type`, `cpu_millis`, `memory_bytes`, `system_disk_size`, `template_id`, and `labels`.

Node metric fields carry raw telemetry and can exceed their nominal range — an over-committed node may report `cpu_util` above 100. A score result outside [0,100] fails output validation, so an expression that subtracts from a bound (`100.0 - node.cpu_util`) should clamp explicitly with a conditional, e.g. `node.cpu_util > 100.0 ? 0.0 : 100.0 - node.cpu_util`.

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
- Scores default to `default-score`, which substitutes the plugin's `default_score` (itself defaulting to 0) after a transport/invocation failure; `fail-closed` is also available. This is deliberately the opposite direction from filters — health fails closed, quality fails open — and it is silent except for one warning log: every candidate receives the same constant score, the failed plugin's ranking contribution vanishes, and when it is the only scorer the ordering degenerates to candidate order. Deployments that depend on ranking quality should set `failure.score: fail-closed` explicitly. Malformed output is never substituted: validation-class failures (nil, empty-id, duplicate, or non-candidate entries, NaN/Inf or out-of-[0,100] scores, partial coverage) mean the plugin is defective and fail closed under every score policy, `default-score` included.
- A built-in `go` scorer that returns an empty result (e.g. `affinity_score` when the request has no node-preference affinity, or `image_score` without applicable resource weights) is treated as "not applicable" and skipped — no error and no `default_score` substitution. A result covering only some candidates is still a failure.
- `no_candidate` supports `fail` and `backoff`. Mandatory guards never trigger backoff: a guard failure — including an emptied candidate set — always fails fast. Backoff applies only when the optional filters or the final selection leave no candidate; a backoff attempt reruns the guards, filters, and scores on the relaxed candidate set, and that guard re-run is the attempt's own safety net. This also changes cluster-saturation behavior versus the legacy path: where the legacy pipeline would retry a saturated cluster through the backoff selector (for example when `realtime_create_num` leaves nothing schedulable), a custom Profile fails the request immediately — `no_candidate: backoff` does not soften guard outcomes.
- `no_candidate` defaults to `fail` when unset. The legacy `default` pipeline always backs off, so adopting the first custom Profile turns "no candidate after filters" into a hard `SelectNodesNoRes` failure — no backoff attempt and no fallback to the default pipeline. Consider explicitly setting `no_candidate: backoff` while adopting Profiles.
- A plugin signals "no suitable node" by returning an empty candidate list, never by returning an error: plugin errors are reported as internal failures governed by the plugin's `failure` policy and are never classified as `no_candidate`, so an erroring filter cannot trigger `backoff` — it fails the request (or fails open) instead.
- Output validation is stricter than the pre-plugin scheduler, and most of it now applies to the legacy default pipeline as well: a filter result containing nil, empty-id, duplicate, or non-candidate nodes fails the request, and a scorer result containing any such entry — or a NaN/Inf value — invalidates that scorer's whole result (under the legacy `ScoreSkip` binding the scorer is then dropped from the aggregate, where the pre-plugin path silently merged such entries and a stale node could even re-enter the candidate set). Profile-compiled scorers additionally require every score within [0,100] and full candidate coverage. Confirm the bounds of any third-party `go` scorer before adding it to a Profile — and note that existing `enable_scorers` deployments get the stricter handling too.

Configuration is compiled as one unit at startup or during a hot reload. If a plugin name, route, expression, weight, selection method, or failure policy is invalid, the new Profile set is not activated and the scheduler continues using the previous complete pipeline.

## Operational caveats

- External gRPC plugins are dialed and handshaked synchronously while the Profile set compiles at CubeMaster startup. Any dial or handshake error aborts `InitScheduler` and the process exits, so a plugin that is merely temporarily unavailable — still starting, restarting, or a stale socket — prevents the entire master from booting, including default-profile traffic that never touches the plugin. This is deliberately asymmetric with the hot-reload path, which rejects a broken Profile set and keeps the previous pipeline. Ensure every configured external plugin is up before CubeMaster starts (ordering via systemd, a sidecar, or your supervisor), or do not configure external plugins at all.
- An open circuit breaker is a hard failure for the whole Profile, not a no-candidate event: after `circuit_breaker_failures` consecutive failures (default 3) the plugin fails fast with `ErrCircuitOpen` for the `circuit_breaker_cooldown` window (default 30s), surfacing as a filter error that `no_candidate: backoff` cannot rescue. A flapping plugin keeps every request routed to the Profile failing across successive cooldown windows — after each cooldown a single half-open probe is allowed and immediately reopens the breaker if it fails. If the plugin is advisory, set `failure.filter: fail-open`; otherwise tune `timeout`, `circuit_breaker_failures`, and `circuit_breaker_cooldown` to how quickly the plugin really recovers.
- Each external plugin client holds a mutex across the whole snapshot-sync + RPC sequence, intentionally serializing every concurrent request routed through that plugin binding. With the default 100ms `timeout`, one degraded plugin caps the affected create path at roughly 10 requests/s, and a backoff attempt pays a second snapshot freeze and sync. Keep plugin RPCs fast, plan capacity around this ceiling, and treat plugin latency as scheduler latency.
- A plugin used as both a filter and a score builds two independent clients — separate connections and handshakes, separate circuit breakers, and separate synced snapshot versions — so one request uploads the full snapshot twice and the two breaker states can drift. Sharing one reference-counted client per (name, socket_path) is a known follow-up.
- Snapshot versions are unique per request (timestamp plus sequence), so the plugin-side snapshot never hits across requests: every request that touches a gRPC plugin re-uploads the full frozen node set, twice on the backoff path. Moving to an epoch that only changes when the node set changes is a known follow-up.
- Freezing the snapshot deep-clones every candidate (including `LocalTemplates`), the request spec, and the routing labels on every request; the legacy default pipeline pays the same allocation cost even though it runs no expr/gRPC plugins. Template requests that disallow non-local images also pay one `GetImageStateByNode` lookup per candidate inside the freeze — previously that lookup only ran when the `template_locality` filter was enabled. Benchmark before tuning — making the freeze conditional on Profiles that actually use expr/gRPC plugins is a known follow-up.
- A rejected hot reload keeps the previous Profile set, but the global config is swapped before watchers run, and built-in plugins read that global config live during Select (guard timeouts, scorer `Disable()` switches, the `real_time_weighted_average`/`image_score` sections, `EffectiveQuota*` wrappers). A rejected reload can therefore leave the old pipeline running with new config values — treat the rejection log as requiring operator action, not a no-op.
- `cpu_millis` and `memory_bytes` are plain integers in the gRPC and CEL request context, so "no resource spec" is indistinguishable from a zero-size request — the restore-placement path passes an empty spec.
- Score scales are not unified across plugin types: expr and gRPC scorers hard-enforce [0,100], but built-in `go` scorers each have their own range — `real_time_weighted_average` normalizes to roughly [0,1], while `image_score` and `affinity_score` produce values up to ~100. In a Profile that mixes `type: go` with `type: expr`/`grpc` scorers, the per-plugin `weight` must absorb the range difference or the wider-range plugin dominates the aggregate.

## Compatibility notes

- Legacy `enable_filters` / `enable_scorers` entries that are not registered plugins now abort CubeMaster startup with an error naming the offending entry; previously they were silently skipped. Remove stale entries from the config or register the plugin before starting the new version.
- Legacy `enable_scorers` entries with a non-positive weight are now skipped with a warning when the pipeline compiles (previously they silently contributed score×0); scheduling behavior is unchanged.
- The startup failure for unknown legacy entries above only applies when the legacy pipeline is actually compiled: once a default entry exists under `profiles`, `enable_filters` / `enable_scorers` are never compiled, so stale entries are silently ignored rather than rejected. Clean them up as part of migrating to Profiles.
- The legacy background score refresher for `multi_factor_weighted_average` now starts whenever that config section exists, even when the scorer is not listed in `enable_scorers`; the loop re-reads the live config on every tick, so for configured-but-not-enabled deployments the only effect is one extra idle goroutine.
- The legacy default pipeline now runs through the same concurrent runner as custom Profiles: scorers execute in parallel within a request, and malformed filter/scorer output (nil, empty-id, duplicate, or non-candidate entries, NaN/Inf values) is rejected instead of silently merged into the result. In-tree built-ins satisfy the contract; audit any third-party `go` plugin before upgrading.
- Configuring `profiles` without a `default: true` entry leaves unmatched requests on the legacy pipeline compiled from `enable_filters`/`enable_scorers`; with no legacy sections at all that fallback is an unguarded random selection. Compilation logs a RISK warning naming the compiled filter/score counts — add a default profile unless relying on the fallback is deliberate.
- Built-in `go` resource filters (`cpu`, `mem`, `disk`) error out on a request that carries no matching resource spec, exactly as the legacy pipeline did (it ran them unconditionally). A Profile that can receive spec-less requests — e.g. the restore-placement path, which passes an empty spec — must not list them as guards or filters.
- When no profile set is loaded at all (e.g. a request racing `InitScheduler` at startup), `Select` now fails closed with "scheduler profile is not initialized"; previously it fell back to the package-global selector lists, which the new init path no longer populates.
