# cubecow Public API Design

- Status: Draft
- Audience: **Upper-layer business callers** of cubecow

---

## 1. Concepts

cubecow exposes only two kinds of objects to callers: **Volume** and **Snapshot**.

### 1.1 Volume

- **A Volume is the only "usable storage unit" cubecow delivers to business callers.**
- Every Volume has a user-supplied string `name` that is unique within the process; `name` is the **canonical identifier** used by every subsequent API to refer to that Volume.
- Every Volume has a logical size `size_bytes` (thin-provisioned; it is only an upper bound and does not pre-reserve physical space).
- Once created, a Volume is always associated with a **host-side block device path `device_path`** (e.g. `/dev/nvme6n1`); business callers perform block-level IO by `open(2)` + `read/write` on that path.
- A Volume is **readable and writable**.
- A Volume can be grown (never shrunk).
- A Volume can be deleted; after deletion all data on it is unrecoverable.

### 1.2 Snapshot

- **A Snapshot is a read-only copy of some Volume at a given moment.**
- Every Snapshot also has a `name` (sharing the same namespace with Volumes; duplicate names are not allowed).
- Every Snapshot carries an `origin_volume` field recording which Volume it was taken from.
- **Snapshots are flat (flatten semantics):** even when the caller asks to "take a snapshot of a snapshot", cubecow internally flattens the request into "another snapshot taken from the same original Volume"; `origin_volume` always points to the initial Volume, never to another Snapshot.
- Snapshots are **read-only**—`write` on a Snapshot is not allowed. If the caller needs a writable copy, use the cross-node `import_lvol` path (the current API layer does not directly expose an in-node "derive Volume from Snapshot" primitive).
- A Snapshot can be deleted independently without affecting its source Volume. Conversely, whether deleting a Volume that still has snapshot references is allowed is decided by the backend implementation.
- A Snapshot has an "activated" state: when activated, `device_path` is non-empty and it can be read by callers; when not activated, only the metadata exists and `device_path` is empty. Callers toggle the state on demand via `activate_volume` / `deactivate_volume`.

### 1.3 Namespace

- Volumes and Snapshots share a single process-wide namespace—once a `name` is taken (by either a Volume or a Snapshot), any subsequent create-style operation is rejected with `AlreadyExists`.
- Recommended `name` rules for callers: UTF-8, non-empty, and not starting with an underscore `_` (the underscore prefix is reserved as a namespace for each cubecow backend's internal bookkeeping; the exact set of prefixes is backend-defined).

### 1.4 Lifecycle

| State | Meaning | Entry / Exit |
|---|---|---|
| Non-existent | The namespace has no such name | Enter via `create_volume` / `create_snapshot_from_volume` / `import_lvol`; leave via `delete_volume` / `delete_snapshot` |
| Activated | Metadata exists + `device_path` is non-empty and IO-ready | Reached directly from "non-existent" via `create_volume`; reached from "not activated" via `activate_volume` |
| Not activated | Metadata exists + `device_path` is empty | Reached from "activated" via `deactivate_volume`; entered on Snapshot creation when `activate=false` |

Invariants:

- `create_volume` creates-and-activates atomically—on return the Volume is always in the "activated" state.
- `activate_volume` works uniformly for both Volumes and Snapshots; semantics are idempotent, repeated calls return the same `device_path`.
- `deactivate_volume` only tears down the device node; it **does not delete metadata**, and `activate_volume` may be called again afterwards.
- `delete_volume` / `delete_snapshot` are one-shot: regardless of whether the object is currently "activated" or "not activated", delete will automatically tear down the device (if activated) and then reclaim the metadata.

---

## 2. Object Fields

### 2.1 Volume fields

| Field | Type | Semantics |
|---|---|---|
| `name` | string | User-supplied name; also the canonical id |
| `size_bytes` | unsigned 64-bit integer | Logical size (thin-provisioned) |
| `device_path` | string | Host block device path; empty string when not activated |
| `snapshot_count` | signed 32-bit integer | Number of direct snapshots taken from this Volume |
| `created_at` | string | RFC3339 timestamp |

### 2.2 Snapshot fields

| Field | Type | Semantics |
|---|---|---|
| `name` | string | Canonical id of the Snapshot |
| `size_bytes` | unsigned 64-bit integer | Logical size |
| `device_path` | string | Host block device path; empty string when not activated |
| `origin_volume` | string | Always points to the initial Volume after flatten |
| `created_at` | string | RFC3339 timestamp |
| `export_uuid` | string | UUID after cross-node export; empty when not exported or the backend does not support export |
| `export_status` | string | Enum values: `""` / `"NONE"` / `"INPROGRESS"` / `"DONE"`; always empty when not exported or the backend does not support export |
| `deletable` | nullable boolean | Whether an exported snapshot can be safely deleted; empty when not exported or not applicable |

Note: `export_uuid` / `export_status` / `deletable` are meaningful only for Snapshots; Volumes do not have these three fields (`export_snapshot` only accepts a Snapshot as input).

### 2.3 VolumeBlockInfo fields

| Field | Type | Semantics |
|---|---|---|
| `num_blocks` | unsigned 64-bit integer | Number of blocks = `size_bytes / block_size` |
| `block_size` | unsigned 32-bit integer | Block size; currently `512` for every backend |

---

## 3. API Methods

Every API is invoked by the caller in the form "pass in a `name` plus parameters, receive back a Volume / Snapshot object or an error". Methods are grouped by function below; each row lists method name, inputs, outputs, and semantics.

### 3.1 Volume operations

| Method | Inputs | Output | Semantics |
|---|---|---|---|
| `create_volume` | `name`, `size_bytes` | Newly created Volume | Creates and immediately activates a Volume. `size_bytes` is the logical upper bound; the backend may round it up to its own minimum allocation unit. A name conflict returns `AlreadyExists`. The returned Volume's `device_path` is guaranteed to be non-empty. |
| `delete_volume` | `name` | None | Deletes a Volume. If it is currently activated, the device is torn down automatically first. If the Volume still has snapshot references, some backends reject with `PreconditionFailed`. Returns success for a non-existent name (idempotent). |
| `resize_volume` | `name`, `new_size_bytes` | `(old_size, new_size)` | Grow only. `new < old` returns `InvalidArg`; `new == old` is a no-op and returns directly. Calling on a Snapshot returns `PreconditionFailed`. |
| `get_volume_info` | `name` | Volume or Snapshot object | Queries the metadata of a Volume or a Snapshot (same method, disambiguated by `name`; returns the appropriate field set depending on the actual object kind). When `name` refers to a previously exported Snapshot and the backend supports cross-node export, `export_status` / `deletable` are refreshed in-line once; on refresh failure they degrade to empty and the method itself still succeeds. |
| `get_volume_block_info` | `name` | VolumeBlockInfo | Returns `num_blocks` and `block_size`. |
| `list_volumes` | `page_size`, `page_token` (empty on first call) | List of Volumes + next-page token + total count | Paginates all Volumes (Snapshots not included). An empty next-page token indicates the last page. **Does not refresh** exported fields. |

### 3.2 Snapshot operations

| Method | Inputs | Output | Semantics |
|---|---|---|---|
| `create_snapshot_from_volume` | `source_name`, `snapshot_name`, `activate` | Newly created Snapshot | Takes a read-only snapshot from `source_name`. `source_name` may be a Volume or another Snapshot (the latter is flattened). When `activate=true`, the new Snapshot is initially "activated" and `device_path` is non-empty; when `activate=false`, only metadata is written. A `snapshot_name` conflict returns `AlreadyExists`. |
| `delete_snapshot` | `snapshot_name` | None | Deletes a Snapshot. If it is currently activated, the device is torn down automatically first. Returns success for a non-existent name (idempotent). |
| `list_snapshots` | `volume_name`, `page_size`, `page_token` | List of Snapshots + next-page token | Paginates all Snapshots under `volume_name` (filtered by the flattened `origin_volume`). |

### 3.3 Activation state

| Method | Inputs | Output | Semantics |
|---|---|---|---|
| `activate_volume` | `name` | Volume object | Activates the block device node of a Volume or Snapshot. Idempotent: repeated calls return the same `device_path`. The returned object's `device_path` is guaranteed to be non-empty. |
| `deactivate_volume` | `name` | None | Tears down the block device node while preserving the metadata. Idempotent: returns success even for a name that is not currently activated. |

### 3.4 Cross-node export / import (optional capability)

Cross-node export / import is an optional capability that each backend decides whether to implement; backends that do not support it uniformly return `PreconditionFailed`.

| Method | Inputs | Output | Semantics |
|---|---|---|---|
| `export_snapshot` | `snapshot_name` | `export_uuid` string | Publishes a Snapshot as a cross-node recoverable object and returns an opaque `export_uuid`. Exporting the same Snapshot repeatedly returns the same UUID. Publishing is generally asynchronous; upload progress can be observed via a follow-up `get_volume_info` looking at `export_status`. |
| `import_lvol` | `lvol_name`, `export_uuid` | Newly created Volume | Materializes a new **writable Volume** (not a snapshot) on the local node from a given `export_uuid`; `lvol_name` is the name of the new local volume. The returned Volume is immediately usable and its `device_path` is non-empty. |

### 3.5 Node-level operations

| Method | Inputs | Output | Semantics |
|---|---|---|---|
| `reset_node_storage` | None | None | Destructive reset: deletes every cubecow-managed Volume and Snapshot on this node. Intended for host reinstallation. |
| `metrics` | None | Key-value map | Returns internal counters (per-API call counts, error counts, backend call counts, etc.). |

---

## 4. Error Model

All APIs use a unified error taxonomy. Every call either succeeds or returns one of the errors below.

| Error | Trigger | Recommended caller handling |
|---|---|---|
| `NotFound` | The referenced name does not exist (semantic lookups such as `get_volume_info`) | Handle as empty object |
| `AlreadyExists` | Name conflict on a create-style operation | Retry with a different name, or treat as already existing |
| `ResourceExhausted` | Backend out of capacity | Expand or clean up |
| `InvalidArg` | Illegal parameter (e.g. `resize` shrinking) | Fix the argument |
| `IoError` | Backend IO error | Usually a backend service anomaly; retryable |
| `PreconditionFailed` | Precondition not met (e.g. deleting a Volume that still has snapshot references, resizing a Snapshot, backend RPC failure) | Decide based on the message |
| `ConfigError` | Startup configuration error | Fix the configuration |

Idempotency summary:

| Idempotent API | Idempotent behavior |
|---|---|
| `delete_volume` / `delete_snapshot` | Returns success for a non-existent name |
| `activate_volume` | Repeated calls return the same `device_path` |
| `deactivate_volume` | Returns success for a name that is not activated |
| `resize_volume` (equal size) | `new == old` is a no-op and returns directly |
| `export_snapshot` | Repeated calls on the same Snapshot return the same `export_uuid` |

---

## 5. Language Bindings

cubecow provides two binding forms whose behavior is functionally identical; pick either:

- **Rust library binding**: for Rust callers; every API from §3 is exposed as a trait method. The engine is initialized with a configuration object at construction time, and all subsequent calls are dispatched against the same engine instance.
- **C ABI shared-library binding**: for C / Go / any other language that can call the C ABI; exports corresponding C functions named with a `cubecow_` prefix, one-to-one with the methods in §3.

### 5.1 C ABI general conventions

- **Return values**: `0` indicates success; negative values indicate error codes mapped one-to-one with the taxonomy in §4 (`NOT_FOUND = -1` / `ALREADY_EXISTS = -2` / `RESOURCE_EXHAUSTED = -3` / `INVALID_ARG = -4` / `IO_ERROR = -6` / `CONFIG_ERROR = -10` / `PRECONDITION_FAILED = -11`; additionally `NULL_POINTER = -12` / `INVALID_UTF8 = -13` / `PANIC = -99` are used to signal FFI-layer input errors or Rust panics).
- **Error messages**: A detailed description can be obtained via a "get last error" function (thread-local storage, valid until the next FFI call).
- **String outputs**: Every string returned by cubecow is heap-allocated by cubecow; the caller **must** hand it back through the cubecow-provided release function and must not call `free()` directly.
- **Optional output parameters**: All output parameters may be passed as NULL to indicate the output is not needed.
- **Panic safety**: Every C ABI function guarantees that a Rust panic does not propagate across the boundary; panics are uniformly converted to the `PANIC` error code.

### 5.2 Lifecycle

The C side needs 4 extra lifecycle entry points (the Rust side manages this automatically via ownership):

| Purpose | Semantics |
|---|---|
| Load configuration from a TOML file path and create an engine | On success returns an engine handle; on failure returns a null handle and details can be retrieved via the error function |
| Load configuration from a JSON string and create an engine | Same as above, with the configuration passed as a JSON string |
| "Do-not-init-logging" variants of the two entry points above | For hosts that already manage tracing / logging on their own, to avoid reinstalling a logging subscriber |
| Destroy an engine | The engine handle is invalidated immediately after the call |

### 5.3 JSON output format

For C ABI calls that "return an object or a list", the output is uniformly a JSON string whose fields correspond directly to §2:

- **Single object output** (`get_volume_info`): a JSON object; when `name` refers to a Volume the fields correspond to §2.1, when it refers to a Snapshot the fields correspond to §2.2.
- **Volume list output** (`list_volumes`): a JSON array whose elements correspond to §2.1.
- **Snapshot list output** (`list_snapshots`): a JSON array whose elements only contain `name` / `size_bytes` / `device_path` / `origin_volume` / `created_at` and **do not include** the exported-series fields (call `get_volume_info` on the specific name to obtain the full field set).
- **Metrics output**: a JSON object whose keys are metric names and whose values are 64-bit unsigned integer counters.

### 5.4 Pagination

Pagination parameters on the C ABI:

- Inputs: `page_size` (unsigned integer) + `page_token` (string; pass empty on the first call to request the first page).
- Outputs: JSON array + next-page token (empty means last page) + total count (only `list_volumes` provides this field).
