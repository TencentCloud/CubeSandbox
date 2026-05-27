# CubeShim Extended API (shimapi)

CubeShim exposes an **extended control plane** on top of the standard containerd Shim v2 API.
The extension uses containerd's `UpdateContainer` RPC, where the caller passes arbitrary
`annotations` to trigger custom actions inside the shim.

## How It Works

All shimapi actions are delivered via the **containerd Shim v2 `UpdateContainer` RPC**
([spec](https://github.com/containerd/containerd/blob/main/core/runtime/v2/README.md)).
The caller populates the `annotations` map on `UpdateContainerRequest`; CubeShim reads
`cube.shimapi.update.action` and dispatches to the matching handler.

```
caller (CubeAPI / CubeMaster)
    │
    │  UpdateContainerRequest { id: <sandbox_id>, annotations: { ... } }
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

| Action | Feature Doc | Description |
|--------|-------------|-------------|
| `RollbackSnapshot` | [rollback-snapshot.md](./rollback-snapshot.md) | Roll back a running sandbox VM to a previously taken snapshot |

## Adding New Features

Create a new Markdown file in this directory named after the feature
(e.g. `live-resize.md`), following the structure of existing docs:

1. **Overview** — what the feature does
2. **Annotation reference** — all annotation keys, types, required/optional
3. **Payload schemas** — JSON struct definitions with field-level descriptions
4. **Examples** — minimal and full JSON examples
5. **Error cases** — expected error messages and their causes
