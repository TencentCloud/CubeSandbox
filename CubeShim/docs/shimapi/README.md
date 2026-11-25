# CubeShim Extended API (shimapi)

CubeShim exposes an **extended control plane** on top of the standard containerd Shim v2 API.
The extension uses containerd's Task `Update` RPC, where the caller passes arbitrary
`annotations` to trigger custom actions inside the shim.

## How It Works

All shimapi actions are delivered via the **containerd Shim v2 Task `Update` RPC**
([spec](https://github.com/containerd/containerd/blob/main/docs/runtime-v2.md)).
The caller populates the `annotations` map on `UpdateTaskRequest`; CubeShim reads
`cube.shimapi.update.action` and dispatches to the matching handler.

```
caller (CubeAPI / CubeMaster)
    │
    │  UpdateTaskRequest { id: <sandbox_id>, annotations: { ... } }
    │  (containerd Shim v2 / ttrpc)
    ▼
CubeShim  →  update_ext::update_route()
    │
    ├── reads  cube.shimapi.update.action
    └── dispatches to the matching action handler
```

## Annotation Keys (common)

| Key | Required | Description |
|-----|----------|-------------|
| `cube.shimapi.update.action` | Yes | Name of the action to perform (see below) |

Action-specific annotation keys are documented per feature.

## Supported Actions

The router and the capability declaration are generated from the same table
(`CubeShim/shim/src/capabilities.rs`), and a unit test fails if one lists an action
the other does not.

| Action | Capability | Description |
|--------|-----------|-------------|
| `RollbackSnapshot` | `update.rollback_snapshot` | Roll back a running sandbox VM to a previously taken snapshot. See [rollback-snapshot.md](./rollback-snapshot.md). |
| `PauseToSnapshot` | `update.pause_to_snapshot` | Pause the VM into a Cubelet-provided snapshot destination (`cube.shimapi.update.pause.snapshot_config`). |
| `CheckPauseAllowed` | `update.check_pause_allowed` | Cheap open-exec preflight before Pause: same busy refusal, no freeze, no artifact. |
| `SnapshotCapture` | `update.snapshot_capture` (`+ .fs_only`) | Capture memory and rootfs inside one freeze window (`cube.shimapi.update.snapshot.capture_config`). |
| `SnapshotResume` | `update.snapshot_capture` | Release the freeze window Cubelet is holding (`cube.shimapi.update.snapshot.id`). |

## Capability declaration

Cubelet used to decide what a shim could do by comparing its version against a
hard-coded boundary, by matching the wording of an error string, or by inspecting
the artifact afterwards. All three describe something other than the process that
will answer the request: a sandbox can be pinned to an older artifact while the
live toolbox points at a newer one, and a vendor-suffixed tag has no ordering at
all.

CubeShim therefore **declares** what it can do, and Cubelet reads that
declaration. There is one published file and one frozen fallback:

| Priority | Source | Path | What it describes |
|---|---|---|---|
| 1 | Artifact file | `<component root>/capabilities.json` | The released build, next to `version` and beside `bin/`. Generated at image build time. |
| 2 | Historical table | (Cubelet source) | Releases older than this mechanism. The only place in Cubelet that compares component versions, and it is frozen. |
| — | Nothing known | — | Cubelet keeps its pre-mechanism behaviour. Unknown is never read as "cannot". |

Nothing is written at runtime and nothing is probed per sandbox: everything this
component can do is fixed when it is built, and the build is already recorded in
the versioned component directory the sandbox started from. The same two-level
rule applies to every component that declares capabilities — the guest agent
publishes its own `capabilities.json` the same way, so "can this sandbox freeze a
filesystem" is answered by the agent artifact rather than by asking a live agent.

### Document schema

```json
{
  "schema": 1,
  "component": "cube-shim",
  "version": "v0.8.0",
  "capabilities": {
    "update.snapshot_capture": 1,
    "update.snapshot_capture.fs_only": 1,
    "update.check_pause_allowed": 1,
    "update.rollback_snapshot": 1,
    "update.pause_to_snapshot": 1,
    "errors.coded": 1
  }
}
```

Rules that keep the format honest:

1. **A name means one thing forever.** An incompatible behaviour change gets a new
   name instead of a higher level on the old one, so a consumer that only knows
   "level >= 1" is never quietly handed something different.
2. **Unknown names are ignored.** A consumer must not fail on a capability it does
   not recognise, so a newer shim works with an older Cubelet.
3. **A missing file means "declares nothing", not "supports nothing".** Consumers
   that gate on a capability fail closed, but consumers choosing a *fallback* path
   must keep the behaviour they had before the mechanism existed.
4. **Every component declares only what its own build knows.** A component never
   reports something about another component, and nothing is discovered at
   runtime: "can the guest agent freeze a filesystem" is published by the agent
   artifact, not inferred by the shim. That keeps one question answered in one
   place, and keeps a sandbox's create path free of probes.
5. **A version the old release table cannot order is "unknown", not "old".**
   Builds that predate this mechanism publish no file, so Cubelet falls back to a
   frozen table keyed by version. That table only answers when the order against a
   boundary is provable. An internal build number (`0.8.0-1027-1`, whose
   prerelease is a build counter) or a vendor suffix at the boundary's own base
   version has no defined order against `-rcN`, so it resolves to "unknown" — the
   consumer then keeps its pre-mechanism behaviour instead of being told "too
   old". Answering "too old" there refused filesystem-only captures for builds
   that support them; answering "new enough" would send captures through an action
   the build may not have.

### The guest agent's declaration

The agent's ability to freeze a filesystem is a property of the agent build, not of
the sandbox: the RPC surface is fixed at compile time and the endpoint allowlist is
all-permissive unless a config file says otherwise, which nothing in this repository
sets. `cube-agent` therefore publishes its own declaration with the same schema:

```json
{
  "schema": 1,
  "component": "cube-agent",
  "version": "v0.8.0",
  "capabilities": {
    "guest.fs_freeze": 1
  }
}
```

Cubelet reads it from the agent version directory the sandbox was created with. A
filesystem-only capture is refused up front only when a component positively reports
that it cannot help — the shim lacking the capture mode, or the agent lacking the
freeze RPCs. A version that predates the declaration falls back to a frozen table;
if that table cannot order the version, the answer is "unknown" and the capture is
attempted, with the freeze RPC remaining the authority.

### Generating the artifact file

The released file is produced by running the binary that was just built, so the
file can never describe a different build:

```bash
containerd-shim-cube-rs --print-capabilities > capabilities.json
cube-agent --print-capabilities > capabilities.json
```

`CubeShim/Dockerfile`, `deploy/one-click/build-vm-assets.sh` and
`deploy/one-click/build-agent-ext4.sh` all do this, and each validates the result
with `CubeShim/scripts/check-component-capabilities.py` before the artifact leaves
the build: an unreadable declaration degrades silently at runtime, so the build has
to be the one that rejects it.

## Coded errors

A shim that declares `errors.coded` prefixes every routable failure with a stable
identifier and a colon:

```
shim_error_unknown_action: unknown update ext action: Foo
shim_error_pause_busy: sandbox pause forbidding, terminate exec tasks first
```

The code is an **addition, never a replacement**. The human message keeps the
pre-capability wording byte-for-byte so a consumer that predates the code (an older
CubeMaster matching `pause forbidding`) keeps working across a rolling upgrade in
either direction. A consumer that does understand codes must judge by the code and
must not read the prose.

## Adding New Features

Create a new Markdown file in this directory named after the feature
(e.g. `live-resize.md`), following the structure of existing docs:

1. **Overview** — what the feature does
2. **Annotation reference** — all annotation keys, types, required/optional
3. **Payload schemas** — JSON struct definitions with field-level descriptions
4. **Examples** — minimal and full JSON examples
5. **Error cases** — expected error messages and their causes (with the coded
   prefix when the shim declares `errors.coded`)

Then add the action and its capability to `UPDATE_ACTIONS` in
`CubeShim/shim/src/capabilities.rs`. The consistency test fails until both sides
agree, which is the point: an action shipped without a declaration is unreachable
through Cubelet's gate, and a declaration without an action is a promise the shim
cannot keep.
