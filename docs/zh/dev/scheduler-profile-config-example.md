---
title: Scheduler Profile Configuration Example
description: Copyable CubeMaster runtime scheduler.profile / scheduler.profiles YAML examples, including built-in presets and binpack_score. Runtime overlay is not equivalent to offline simulator strategy profiles.
status: working_guide
source: CubeMaster/pkg/base/config/config.go
updated: 2026-09-10
---

# Scheduler Profile Configuration Example

Copyable YAML for CubeMaster **runtime** Profile overlay and `binpack_score`.
This page covers what ships in the runtime Profiles + binpack PR. An HTTP
plugin scorer (`external_http_score`) is related open work tracked in #1700
and is **not** registered or allowed here.

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
- shipping or documenting `external_http_score` as part of this PR
  (see #1700);
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
| `scheduler.profiles.<name>.filter.enable_filters` | Replaces `scheduler.filter.enable_filters` when the profile provides a non-nil list. |
| `scheduler.profiles.<name>.score.enable_scorers` | Replaces `scheduler.score.enable_scorers` when the profile provides a non-nil list. |
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
`resource_weights` value. Plugin-only `binpack_score` does not use that map
as a construction gate and does not require a fake `resource_weights`
block. Legacy `affinity_score` keeps empty-profile compatibility: with no
Profile and `resource_weights: null` (omitted), it is not constructed; with
a Profile or a non-nil `resource_weights` map it constructs normally.
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

## Built-in Profile Examples

Leave `scheduler.profile` empty to keep the current Filter/Score config.
Setting a built-in name does **not** require a matching key under
`scheduler.profiles`.

`balanced_spread` (high-concurrency short-lived sandboxes) supplies a default
`real_time_weighted_average` block:

```yaml
scheduler:
  profile: balanced_spread
```

`template_locality_first` (repeated same-template creates) likewise supplies
safe `image_score` defaults:

```yaml
scheduler:
  profile: template_locality_first
```

`binpack_utilization` (mixed-size / long-lived) injects plugin weight 1 and
equal CPU/memory/MVM occupancy weights when its plugin block is omitted:

```yaml
scheduler:
  profile: binpack_utilization
```

These overlays are scene-oriented selector combinations. They are **not**
the same as simulator `weightsForProfile`, and they make **no** live
performance claims.

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
`enable_filters`.

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
`binpack_utilization` injects defaults; a user Profile without that block
fails fast. Explicit `weight: 0` disables Select. Negative weight is always
rejected at config load.

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

## Related open work

`external_http_score` (HTTP plugin scorer) is tracked separately in #1700 and
is not part of this runtime Profiles + binpack change set. Do not list it in
`enable_scorers` on this branch; unknown score names fail closed.

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

**Do Not**

- Claim runtime presets use the same scoring formula as the offline simulator.
- Put `plugin_conf` under `scheduler.profiles.<name>.score`.
- Invent filter or score names outside the allowed sets above.
- Equate runtime `scheduler.profiles` with simulator `weightsForProfile`.
- Treat a Profile overlay as a change to
  `PreFilter -> Filter -> Score -> PostScore`.
- Claim #1699 / #1700 is merged via this PR.

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
