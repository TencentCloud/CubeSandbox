---
title: Scheduler Profile Configuration Example
description: Copyable CubeMaster runtime scheduler.profile / scheduler.profiles YAML examples, including built-in presets and binpack_score. Runtime overlay is not equivalent to offline simulator strategy profiles.
status: working_guide
source: CubeMaster/pkg/base/config/config.go
updated: 2026-09-10
---

# Scheduler Profile Configuration Example

Copyable YAML for CubeMaster **runtime** Profile overlay and `binpack_score`.
This page focuses on Profiles + binpack. The umbrella also registers
`external_http_score`; wire-protocol details live in the CubeMaster scheduler
config guide, not in the Profile overlay examples below.

## Scope

This document shows how to set `scheduler.profile` and `scheduler.profiles` so
config `preHandle` applies Filter/Score selector lists and merges Profile
`resource_weights` over the base `scheduler.score.resource_weights` map.

In scope:

- user-defined runtime Profile overlay;
- built-in presets `balanced_spread`, `template_locality_first`,
  `binpack_utilization` (empty `scheduler.profile` still leaves defaults);
- enabling `binpack_score` by name (directly or via Profile);
- keeping `plugin_conf` on `scheduler.score.plugin_conf`.

Out of scope:

- treating runtime presets as formula-equivalent to offline simulator
  `weightsForProfile` / `schedulerbench`;
- putting `plugin_conf` under `scheduler.profiles.<name>.score`;
- changing production Filter/Score defaults when `scheduler.profile` is empty;
- HTTP sidecar request/response examples (see the scheduler config guide);
- live multi-node performance claims.

## Runtime Profile Contract

Source: `CubeMaster/pkg/base/config/config.go`
(`SchedulerConf`, `SchedulerProfileConf`, `SchedulerProfileScoreConf`,
`applySchedulerProfile`, `builtinSchedulerProfiles`,
`validateSchedulerProfileSelectors`).

| YAML path | Role |
|---|---|
| `scheduler.profile` | Name of the active overlay. Empty string (default) means no expansion. Built-in names apply without a user map key. |
| `scheduler.profiles` | User-defined map of named overlays. A user key with the same name as a built-in **overrides** the built-in. |
| `scheduler.profiles.<name>.filter.enable_filters` | Replaces `scheduler.filter.enable_filters` when the profile provides a non-nil list. Dropping names that were in the base list fails config load unless `allow_dropped_filters: true` is set on that Profile. |
| `scheduler.profiles.<name>.allow_dropped_filters` | Opt-in for Profiles: allow `enable_filters` replace to drop base admission filters (for example `disk`, `thirtparty`). Default `false` for user Profiles **and** built-in presets. Selecting a built-in on a longer base list therefore requires an explicit same-name `profiles.<builtin>.allow_dropped_filters: true` (or keeping dropped names in the Profile list). A same-name entry that sets only this flag (no filter/score) falls back to the built-in and applies the flag. |
| `scheduler.profiles.<name>.score.enable_scorers` | Replaces `scheduler.score.enable_scorers` when the profile provides a non-nil list. Dropping names that were in the base list fails config load unless `allow_dropped_scorers: true` is set on that Profile. |
| `scheduler.profiles.<name>.allow_dropped_scorers` | Opt-in for Profiles: allow `enable_scorers` replace to drop base scorers (for example an operator's `external_http_score`). Default `false` for user Profiles **and** built-in presets — same safety model as filters. |
| `scheduler.profiles.<name>.score.resource_weights` | Merged over `scheduler.score.resource_weights`; Profile keys win and unrelated base keys remain. These are factor weights, not plugin weights. |
| `scheduler.score.plugin_conf.*` | Per-scorer params. **Not** a profile overlay field. |

A runtime Profile is a **selector overlay**. It does not change
`Select()` phase order. Built-in presets are scene-oriented combinations of
existing filters/scores (plus thin `binpack_score`). They are **not**
offline simulator models and share names with simulator strategy profiles
only as strings.

`SchedulerProfileScoreConf` intentionally omits `plugin_conf`. When
`scheduler.profile` is set, every registered scorer listed in the final
effective `enable_scorers` requires its corresponding
`scheduler.score.plugin_conf` block; missing configuration, or a factor
scorer with no positive `resource_weights` entry, fails before scheduler
construction. Built-in presets additionally reject an explicitly disabled
required scorer (`disable: true` / `weight: 0`). User Profiles may keep an
enabled name with an intentional disable. Empty `scheduler.profile` keeps
the pre-upgrade load path: ineffective factor/plugin combinations may still
load and become runtime no-ops.

The three built-ins inject self-contained defaults **only when** the
corresponding `plugin_conf` entry is `nil`: `balanced_spread` for
`real_time_weighted_average`, `template_locality_first` for `image_score`,
and `binpack_utilization` for `binpack_score`. An operator-supplied
`plugin_conf` / `enable_weight_factors` block is never overwritten by
built-in defaults.

### Precedence

1. **User `scheduler.profiles.<name>`** with the same key as a built-in
   replaces the built-in overlay entirely.
2. **Explicit `scheduler.score.plugin_conf.*`** wins over built-in injected
   defaults (defaults apply only when the pointer is `nil`).
3. **Profile `resource_weights` same-key override**: base map is copied
   first, then Profile keys are written last (Profile wins for colliding
   keys; unrelated base keys remain).

Factor-based `real_time_weighted_average`,
`multi_factor_weighted_average`, and `image_score` are constructed only when
at least one of their `enable_weight_factors` has a positive
`resource_weights` value. Plugin-only `binpack_score` and
`external_http_score` do not use that map as a construction gate and do not
require a fake `resource_weights` block. Legacy `affinity_score` keeps
empty-profile compatibility: with no Profile and `resource_weights: null`
(omitted), it is not constructed; with a Profile or a non-nil
`resource_weights` map it constructs normally.
`plugin_conf.binpack_score.weight < 0` is always rejected at config load;
`weight: 0` disables Select; omitting the block keeps the runtime default.

Unknown `scheduler.profile` names (not user-defined and not built-in) and
unknown filter/score names in the selected overlay fail closed in
`preHandleScheduler` before the scheduler runs.

Allowed filter names (must match `CubeMaster/pkg/selector/filter/init.go`):

- `cpu`
- `mem`
- `template_locality`
- `realtime_create_num`
- `disk`
- `thirtparty`

Allowed score names (must match `CubeMaster/pkg/selector/score/init.go`):

- `real_time_weighted_average`
- `multi_factor_weighted_average`
- `affinity_score`
- `image_score`
- `binpack_score`
- `external_http_score`

### Operator notes

**Filter list replace (admission risk).** When a Profile (built-in or user)
provides a non-nil `filter.enable_filters` list, that list **replaces**
`scheduler.filter.enable_filters` entirely. It does **not** merge with the
base list. Dropping any name that was present in the base list **fails config
load** unless `allow_dropped_filters: true`. Built-in presets keep that flag
`false`, so stock configs (`cpu` / `mem` / `template_locality` /
`realtime_create_num`) must opt in when selecting
`balanced_spread` / `template_locality_first` / `binpack_utilization` by name
(or keep the dropped names in the Profile list). They still replace with short
lists — audit effective `enable_filters` if you previously relied on `disk` /
`thirtparty`. A same-name `profiles.<builtin>` entry with only
`allow_dropped_filters` / `allow_dropped_scorers` (no filter/score) falls back
to the built-in rather than applying an empty overlay.

**Score list replace (business-scorer risk).** The same replace semantics apply
to `score.enable_scorers`: dropping a base scorer (for example an operator's
`external_http_score`) **fails config load** unless `allow_dropped_scorers: true`
or the name is kept in the Profile list. Built-ins also default that flag to
`false`.

**`weight: 0` disables scorers.** For the four legacy Score plugins
(`real_time_weighted_average`, `multi_factor_weighted_average`,
`affinity_score`, `image_score`), plugin `weight: 0` (or `disable: true`)
disables the scorer and skips its `Select`. Because those `weight` fields are
YAML `float64`, omitting the `weight` key inside a present
`plugin_conf.<scorer>` block also decodes to `0` and disables the scorer — set
an explicit positive `weight` to keep them active. **`binpack_score` is
different:** its `weight` is `*float64`, so omitting `weight` inside a present
`plugin_conf.binpack_score` block keeps the runtime default of `1` (enabled);
only an explicit `0` (or `disable: true`) disables it. **`external_http_score`
is different again:** `weight: 0` keeps `Disable()==false` but Select is an
inert no-op (no HTTP round-trip); use `disable: true` for an explicit off
switch. **Negative** plugin `weight` is rejected at config load for every
registered scorer (including `binpack_score` / `external_http_score`); configs
that previously started with a negative weight fail `config.Init` after
upgrade. Omitting the entire `plugin_conf.<scorer>` block while listing a
factor/affinity scorer or `external_http_score` in `enable_scorers` also fails
config load (empty Profile included). `binpack_score` may omit the whole block
and keep runtime defaults; under a non-empty Profile, built-ins may inject
defaults when the pointer is still `nil`.

**`binpack_score` occupancy weights.** `cpu_weight` / `mem_weight` /
`mvm_weight` are `*float64` like plugin `weight`: omit → default `1` for that
dimension; explicit `0` **excludes** the dimension from the occupancy blend;
negatives fail config load. Only the plugin-level `weight: 0` (or `disable: true`)
disables the scorer entirely. Do not mix `binpack_score` with remaining-capacity /
spread scorers in the same `enable_scorers` list; occupancy polarity is inverted
relative to `real_time_weighted_average` / `multi_factor_weighted_average`, so the
blend can cancel. Built-in `binpack_utilization` only enables `binpack_score`.

**Factor scorers fail closed under a Profile.** When `scheduler.profile` is
non-empty, each factor-based scorer in the final `enable_scorers` list
(`real_time_weighted_average`, `multi_factor_weighted_average`,
`image_score`) requires a non-empty `enable_weight_factors` and at least
one positive `resource_weights` entry for those factors. Empty or omitted
factor lists, or all-zero / missing factor weights, fail closed in
`preHandleScheduler` before the scheduler runs. With an empty Profile, a
present-but-ineffective factor list still constructs the scorer (Warn) so a
later hot-reload of `resource_weights` / factors can activate Select without
restart; Select no-ops while effective weight remains zero.

**MVM occupancy capacity.** `binpack_score` (and other scorers that share
the helper) compute MVM occupancy with `localcache.MaxMvmLimit(n)`, the
authoritative per-node capacity fallback (instance-type /
`node_max_mvm_num` path). Do **not** assume raw `node.MaxMvmLimit` alone
is the denominator.

**Restart vs hot-update.** Profile expansion runs inside config
`Init` / `preHandle` (`applySchedulerProfile` via `preHandleScheduler`).
CubeMaster **does** hot-reload `conf.yaml` through the file watcher: on
change, `listener.OnEvent` re-runs `preHandle` and, on success, updates the
in-memory `Config`. On `preHandle` / `validate` failure it logs FATAL via
`CubeLog.Fatalf` (which writes a FATAL line and does **not** call `os.Exit`)
and keeps the previous Config — the bad overlay is not applied. Selecting a
Profile makes unknown names in the **effective** `enable_filters` /
`enable_scorers` lists fail closed at that reload boundary as well.
However, the scheduler Filter/Score plugin slices are built once in
`scheduler.InitScheduler` (`filter.NewSelector` / `score.NewSelector`) and
are **not** rebuilt on config hot-reload. Changing `scheduler.profile`,
Profile `enable_filters` / `enable_scorers`, or otherwise swapping which
selectors are registered therefore requires a **CubeMaster restart** to
take effect on the scheduling pipeline. Live-read plugin params (for
example `weight` / `disable` on an already-constructed scorer) may update
from the reloaded Config without restart, but selector-set changes do not.

## Built-in Profile Examples

Leave `scheduler.profile` empty to keep the current Filter/Score config.
Setting a built-in name does **not** require a matching key under
`scheduler.profiles`.

`balanced_spread` (high-concurrency short-lived sandboxes) supplies a default
`real_time_weighted_average` block:

```yaml
scheduler:
  profile: balanced_spread
  profiles:
    balanced_spread:
      allow_dropped_filters: true
      # Required if base enable_scorers is non-empty (e.g. external_http_score):
      allow_dropped_scorers: true
```

`template_locality_first` (repeated same-template creates) likewise supplies
safe `image_score` defaults:

```yaml
scheduler:
  profile: template_locality_first
  profiles:
    template_locality_first:
      allow_dropped_filters: true
      allow_dropped_scorers: true
```

`binpack_utilization` (mixed-size / long-lived) injects plugin weight 1 and
equal CPU/memory/MVM occupancy weights when its plugin block is omitted:

```yaml
scheduler:
  profile: binpack_utilization
  profiles:
    binpack_utilization:
      allow_dropped_filters: true
      allow_dropped_scorers: true
```

These overlays are scene-oriented selector combinations. They are **not**
the same as simulator `weightsForProfile`, and they make **no** live
performance claims. Remember the filter-replace warning above: built-ins
replace `enable_filters` with a shorter list and can drop `disk` /
`thirtparty` / other base admission filters — the same-name
`allow_dropped_filters: true` opt-in above is required on longer base lists.
Built-ins also replace `enable_scorers`; if the base list already includes
operator scorers (for example `external_http_score`), set
`allow_dropped_scorers: true` (as in the blocks above) or keep those names in
the Profile scorer list.

## Minimal Profile Example

Leave `scheduler.profile` empty to keep the current Filter/Score config.
When a name is set, it must exist in `scheduler.profiles` unless it is a
built-in. The name below (`locality_combo`) is an operator-chosen key, not a
CubeMaster built-in.

```yaml
scheduler:
  profile: locality_combo
  profiles:
    locality_combo:
      allow_dropped_filters: true
      filter:
        enable_filters:
          - cpu
          - mem
          - template_locality
      score:
        enable_scorers:
          - image_score
          - affinity_score
        resource_weights:
          image_id: 1
          template_id: 2
  score:
    plugin_conf:
      image_score:
        weight: 1
        enable_weight_factors:
          - image_id
          - template_id
      affinity_score:
        weight: 1
```

What this overlay copies at `preHandle`:

- `scheduler.filter.enable_filters` becomes `cpu`, `mem`, `template_locality`;
- `scheduler.score.enable_scorers` becomes `image_score`, `affinity_score`;
- the Profile factor weights merge over existing
  `scheduler.score.resource_weights`; plugin weights remain under
  `plugin_conf`.

Omitted overlay sections are left untouched. A filter-only profile does not
clear existing `enable_scorers`; a score-only profile does not clear existing
`enable_filters`. When the Profile **does** provide `enable_filters`, that
list replaces the base list (see Operator notes).

## BinpackScore Example

A profile (or direct `enable_scorers`) may enable `binpack_score`. Plugin
weight and occupancy factor weights stay on
`scheduler.score.plugin_conf.binpack_score`. Do **not** put `plugin_conf`
under `scheduler.profiles.<name>.score`.

```yaml
scheduler:
  profile: binpack_combo
  profiles:
    binpack_combo:
      allow_dropped_filters: true
      filter:
        enable_filters:
          - cpu
          - mem
      score:
        enable_scorers:
          - binpack_score
  score:
    plugin_conf:
      binpack_score:
        weight: 1
        cpu_weight: 1
        mem_weight: 1
        mvm_weight: 1
        disable: false
```

If `binpack_score` is listed in the final `enable_scorers` under a non-empty
Profile but `plugin_conf.binpack_score` is omitted, built-in
`binpack_utilization` injects defaults; a user Profile may also omit the block
and keep the same runtime defaults (`BinpackPluginWeight(nil)` → weight 1).
Explicit `weight: 0` disables Select. Negative weight is always rejected at
config load. `cpu_weight` / `mem_weight` / `mvm_weight` are pointers: omit →
default `1`; explicit `0` excludes that dimension. MVM occupancy uses
`localcache.MaxMvmLimit`, not raw `node.MaxMvmLimit` alone.

Invalid (will not overlay `plugin_conf`; the Go type has no such field):

```yaml
# Do not do this. scheduler.profiles.<name>.score has no plugin_conf.
scheduler:
  profiles:
    binpack_combo:
      score:
        enable_scorers:
          - binpack_score
        plugin_conf:          # not a SchedulerProfileScoreConf field
          binpack_score:
            weight: 1
```

## Related docs

`external_http_score` is registered in this umbrella. For endpoint / timeout /
mode / fail-open semantics, see the CubeMaster scheduler config guide
(External HTTP score plugin). Review units #1699 / #1700 / #1708 remain useful
history for the separate topic PRs; this umbrella combines them. With a
selected Profile, unknown names in the **effective** `enable_filters` /
`enable_scorers` lists fail closed at config load; with an empty Profile,
unknown base `enable_scorers` names are still warn-skipped at `NewSelector`
(legacy compatibility).

## Runtime Profile vs Simulator Profile

Runtime Profile and simulator strategy profiles share the word "profile"
but are **not equivalent**.

| | Runtime Profile | Simulator strategy profile |
|---|---|---|
| Where | CubeMaster config: `scheduler.profile` / `scheduler.profiles` | Offline simulator / `schedulerbench` `weightsForProfile` |
| What it is | User-defined overlay of existing filter/score selector names and factor `resource_weights` | Scoring-weight preset inside the offline placement model |
| Built-in names | `balanced_spread`, `template_locality_first`, `binpack_utilization` (selector overlay; user map key overrides). Operators may also choose other map keys. | Simulator-only weights that may reuse the same strings |
| `plugin_conf` | Not overlayable. Params stay on `scheduler.score.plugin_conf` | Not CubeMaster scheduler YAML |

**Runtime built-in preset names** (selector overlay, not simulator weights):

- `balanced_spread`
- `template_locality_first`
- `binpack_utilization`

**Placement prerequisite:** score **ranking** from these presets steers placement
only when `scheduler.priority_select_num >= 1` (top-n truncate) and/or
`least_select_name` is weight-aware (`sw` / `rw` / `rrw`). When
`priority_select_num` is **omitted** (code fallback `-1`) or set below `1`, and
`least_select_name` stays `random`, final selection is uniform over the
post-filter scored set — ranking is observability-only. **Shipped**
`CubeMaster/conf.yaml`, the Helm chart, single-node config, and one-click
Terraform set `priority_select_num` to `1` or more, so on those stock deploys
the top-scored node wins (not observability-only). Filter-list replace from a
built-in can still change admission independently. Offline simulator /
`schedulerbench` argmax-over-score is equivalent to production only around
`priority_select_num: 1`; do not read bench deltas as omitted-key fallback
placement deltas. Config load Warns when a Profile enables scorers under the
inert omitted-key fallbacks.

Copying one of those strings into `scheduler.profile` loads the CubeMaster
built-in overlay unless you also define a matching user key under
`scheduler.profiles` (user wins). Offline simulator workloads do not prove a
real multi-node deployment, CubeAPI/Cubelet create path, real create latency,
or production performance.

## Do / Do Not

**Do**

- User-define extra profile map keys (`locality_combo`, `binpack_combo`, or any
  other operator-chosen name).
- Use only registered selector names listed above.
- Keep `plugin_conf` on `scheduler.score.plugin_conf`.
- Put scorer/plugin weights under `plugin_conf.<scorer>.weight`; do not use a
  scorer name as a `resource_weights` key.
- Leave `scheduler.profile` empty when you want existing Filter/Score config
  unchanged.
- Treat runtime built-in presets and simulator `weightsForProfile` as two
  paths that share names but are **not** formula-equivalent.
- After selecting a Profile, audit effective `enable_filters` so required
  admission filters (`disk`, `thirtparty`, …) were not dropped by replace.
- Set explicit positive `weight` on every `plugin_conf.<scorer>` you intend
  to keep active; restart CubeMaster after Profile / selector-list changes.

**Do Not**

- Claim runtime presets use the same scoring formula as the offline simulator.
- Put `plugin_conf` under `scheduler.profiles.<name>.score`.
- Invent filter or score names outside the allowed sets above.
- Equate runtime `scheduler.profiles` with simulator `weightsForProfile`.
- Treat a Profile overlay as a change to
  `PreFilter -> Filter -> Score -> PostScore`.
- Treat this page as the HTTP wire-protocol / fail-open contract (use the
  scheduler config guide instead).
- Assume omitted `cpu_weight` / `mem_weight` / `mvm_weight` means "exclude"
  (omit → default `1`; use explicit `0` to exclude a binpack dimension).
- Expect Profile / `enable_filters` / `enable_scorers` swaps to rebuild the
  live selector set without a CubeMaster restart (membership of the selector
  set is still restart-scoped; `resource_weights` and plugin params hot-reload).

## Verification

These checks confirm the YAML contract against current source. They are not
live-cluster proof.

From the repository root:

```bash
rg -n "scheduler.profile|scheduler.profiles|binpack_score" CubeMaster/pkg/base/config/config.go
rg -n "binpack_score" CubeMaster/pkg/selector/score/init.go
```

From `CubeMaster`:

```bash
go test ./pkg/base/config ./pkg/selector/score ./pkg/scheduler -count=1
```

Relevant tests:

- `TestPreHandleScheduler_NoProfileLeavesSchedulerUnchanged`
- `TestPreHandleScheduler_ProfileAppliesFilterAndScore`
- `TestPreHandleScheduler_UnknownProfileReturnsError`
- `TestPreHandleScheduler_BuiltinProfilesApplyWithoutUserMap`
- `TestPreHandleScheduler_UserProfileOverridesBuiltin`
- `TestInit_BinpackScoreWeightSemantics`
- `TestRunScoreFilterBinpackScorePrefersFullerNode`
- `TestRunScoreFilterBuiltinProfileOverlayChangesPlacementOrder`
