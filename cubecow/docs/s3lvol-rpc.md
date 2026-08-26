# s3lvol / RCOW JSON-RPC Server Design and Interface

- Status: Draft
- Audience: **Implementers and operators of the s3lvol / RCOW service**
- Positioning: s3lvol is a local daemon that exposes three families of primitives over a Unix Domain Socket using JSON-RPC — **lvol (writable logical volumes) + snapshot (read-only snapshots) + cross-node export/import**. The data plane is built on SPDK + NVMe-oF and exposes block devices to the host kernel; the metadata / cold-data backend is COS object storage.

---

## 1. Concepts

s3lvol exposes only three kinds of objects:

### 1.1 lvol (Logical Volume)

- Writable logical volume; created by `rcow_create_lvol`.
- Each lvol has a globally unique string `lvol_name` and a GiB-granularity capacity `size_gib`.
- An lvol is purely a "logical object"; it **does not** directly appear as a host device. To obtain a `/dev/nvmeXnY` you must additionally call `rcow_active_bdev`.
- Supports resize (`rcow_resize_lvol`, grow only, no shrink).
- Supports deletion (`rcow_delete_lvol`); if the lvol is still active the client first calls `rcow_deactive_bdev`.

### 1.2 snapshot

- A read-only copy of an lvol at a specific point in time; created from an lvol via `rcow_create_snapshot`.
- snapshot and lvol share **the same global flat namespace**: a `snapshot_name` must not collide with any `lvol_name`.
- A snapshot is read-only. To derive a writable copy from a snapshot, use `rcow_create_clone`.
- A snapshot can be deleted (via `rcow_delete_lvol`, the same RPC, distinguished by name).
- A snapshot can be activated as a host block device (`rcow_active_bdev` also accepts a snapshot name); once activated it is read-only.

### 1.3 clone

- A **writable lvol** derived from a snapshot; created by `rcow_create_clone`.
- Once created, the clone is **data-independent** from its source snapshot — deleting the clone does not affect the snapshot, and subsequent writes to the clone must not contaminate the snapshot.
- On the server side, a clone behaves identically to an lvol; every RPC that is valid on an lvol is also valid on a clone.

### 1.4 Namespace

- lvol / snapshot / clone **share** a single global flat string namespace. Any create-family operation that collides must fail with a message containing the keyword `already exists`.
- Client-reserved prefixes (the server should be aware of them but **should not** apply special handling):
  - `__cbc_tmpclone_<uuid>`: intermediate clone name used by cubecow when flattening snapshots. Its lifecycle is managed explicitly by the client; the server **must not** garbage-collect it automatically.
  - `__cbc_probe_<uuid>`: startup liveness probe placeholder used by cubecow. The server will receive a `rcow_deactive_bdev` for such a name and should respond with a `not found`-class error and produce no side effects.

### 1.5 Data plane (bdev)

- An lvol / snapshot / clone is only exposed to the host kernel as an NVMe-oF namespace (`/dev/nvmeXnY`) after `rcow_active_bdev`.
- `rcow_deactive_bdev` only tears down that namespace; it **does not** delete the underlying object. The object can be re-activated later with `rcow_active_bdev`.
- `rcow_get_bdev` returns the `device_path` of an already-activated object; when the object is not active it must return a `not found`-class error.

### 1.6 Cross-node (export / import)

- `rcow_export_snapshot` publishes a snapshot to shared COS and returns an opaque `export_uuid`. The upload is **asynchronous** and may still be in flight when the RPC returns.
- `rcow_get_snapshot_status` queries upload progress and whether the snapshot is "safe to delete" (i.e. whether any importer on another node still references it).
- `rcow_import_lvol` materializes a new writable lvol on this node from an `export_uuid`. The client always passes `"decouple": true`, which requires the server to complete decoupling within this RPC — the returned lvol must be immediately usable, and the client will not issue any follow-up decouple RPC.

---

## 2. Transport and Protocol

### 2.1 Transport

| Item | Value |
|---|---|
| Socket type | Unix Domain Stream (`AF_UNIX` + `SOCK_STREAM`) |
| Default path | `/var/run/s3lvol.sock` |
| Long-lived connection | Yes; the client maintains a single long-lived connection and will auto-reconnect once on `BrokenPipe / ConnectionReset` |
| Concurrency | Requests on the same connection are answered strictly in send order (the client does not match by `id`, it reads responses sequentially) |
| Frame format | **Single-line JSON terminated by `\n`** |
| Default client read/write timeout | 10s |

**Server behavior requirements:**

1. Accept connections initiated by the uid running the cubecow client; the socket must be readable immediately after `accept`.
2. Support an arbitrary number of request/response exchanges on the same connection.
3. **Must not** proactively close idle connections when there is no RPC activity (the client reconnects frequently).
4. Responses on the same connection **must** be returned in the order the requests were sent; no out-of-order pipelining.
5. The server is allowed to `accept` multiple connections in a multi-threaded fashion.

### 2.2 Protocol: JSON-RPC 2.0 (subset)

**Request**:

```json
{ "jsonrpc": "2.0", "method": "rcow_xxx", "params": <object>, "id": <u64> }
```

- `jsonrpc` is always `"2.0"`.
- `method` is restricted to the whitelist defined in §3.
- `params` **is always a JSON object** (positional arrays are not used).
- `id` is monotonically increasing; the server should just echo it back in the response — its semantic can be ignored.

**Response (success)**:

```json
{ "jsonrpc": "2.0", "id": <u64>, "result": <object> }
```

**Response (protocol-level failure)**:

```json
{ "jsonrpc": "2.0", "id": <u64>, "error": { "message": "<human string>", "code": <optional int> } }
```

The client currently only reads `error.message`; `code` is reserved and unused.

### 2.3 Framing rules

1. One request = one compact single-line JSON followed by `\n`; **no** bare newlines inside the payload.
2. One response = one compact single-line JSON followed by `\n`; same rule.
3. All strings are UTF-8.
4. Unrecognized fields may appear in responses and the client will ignore them; requests will never contain fields not defined in §3.

### 2.4 Business-level response convention (important)

Aside from protocol-level `error`, **every** RPC's successful `result` must be an object that **must** contain:

| Field | Type | Semantics |
|---|---|---|
| `bool_value` | bool | `true` = business success; `false` = business failure |
| `string_value` | string | On success: a name / uuid / or **nested JSON string** (see §2.5); on failure: the error message |

Client decoding logic:

```
if resp.error         → protocol-level error
elif bool_value=false → treat string_value as an error message and map by keyword
else                  → read string_value from result for further parsing
```

**Therefore, every response the server produces must at least provide `result.bool_value`** — a missing field is treated by the client as an ambiguous "neither success nor error" state and is turned into a failure.

### 2.5 Nested-JSON `string_value`

For the following 3 methods, `string_value` in a successful response is a **JSON-encoded string** (the server builds a nested JSON, serializes it into a string, and wraps it inside the outer `result`). The client performs a second `json.parse`:

- `rcow_active_bdev`
- `rcow_get_bdev`
- `rcow_get_snapshot_status`

For these 3 methods the server **must** guarantee that the nested JSON is a compact, valid JSON (no bare newlines, no unescaped quotes).

---

## 3. RPC Method Contract

s3lvol exposes **11** RPC methods to cubecow. For each method, the input, successful response, idempotency requirement, and error-keyword requirement are specified.

### 3.1 `rcow_create_lvol`

Create a writable lvol.

**Request**

```json
{ "lvol_name": "vol-A", "size_gib": 20 }
```

| Field | Type | Required | Constraint |
|---|---|---|---|
| `lvol_name` | string | ✅ | UTF-8; globally unique in the namespace |
| `size_gib` | number | ✅ | positive integer GiB |

**Success**

```json
{ "bool_value": true, "string_value": "vol-A" }
```

**Requirements:**

- **Must be idempotent**: a second call with the same name must return a `string_value` containing the keyword `"already exists"` (or `"already exist"`) and **must not** damage the data produced by the first call.
- After success the lvol is in the "created, not active" state; the client will immediately follow up with `rcow_active_bdev`.

### 3.2 `rcow_create_snapshot`

Derive a read-only snapshot from an lvol.

**Request**

```json
{ "lvol_name": "vol-A", "snapshot_name": "snap-1" }
```

| Field | Type | Required | Constraint |
|---|---|---|---|
| `lvol_name` | string | ✅ | The source must be a **writable lvol** (the client will not pass a snapshot name here) |
| `snapshot_name` | string | ✅ | Globally unique in the namespace |

**Success**

```json
{ "bool_value": true, "string_value": "snap-1" }
```

**Requirements:**

- Idempotent: name collision returns `"already exists"`.
- The source lvol may be active when the snapshot is taken (the client will not deactivate it beforehand).

### 3.3 `rcow_create_clone`

Derive a writable clone from a snapshot.

**Request**

```json
{ "snapshot_name": "snap-1", "clone_name": "vol-B" }
```

| Field | Type | Required | Constraint |
|---|---|---|---|
| `snapshot_name` | string | ✅ | Must be an existing snapshot |
| `clone_name` | string | ✅ | Globally unique in the namespace; when the request originates from cubecow it is always prefixed with `__cbc_tmpclone_` |

**Success**

```json
{ "bool_value": true, "string_value": "vol-B" }
```

**Requirements:**

- The clone must be **fully data-independent** from the source snapshot — a subsequent `rcow_delete_lvol(clone)` or a write to the clone must not affect the snapshot.
- Idempotent: name collision returns `"already exists"`.
- The server **must not** garbage-collect clones based on the `__cbc_tmpclone_` prefix; the lifecycle is managed explicitly by the client.

### 3.4 `rcow_delete_lvol`

Delete an lvol / snapshot / clone.

**Request**

```json
{ "lvol_name": "vol-A" }
```

**Success**

```json
{ "bool_value": true, "string_value": "vol-A" }
```

**Requirements:**

- **Unified interface**: `lvol_name` may refer to an lvol / snapshot / clone; the server determines the kind by name. The client does not distinguish at the protocol layer.
- **Must be idempotent**: for a non-existent name, the server **must** return an error message containing the keyword `"not found"` or `"no such"`. The client treats such an error as an idempotent success.
- **Integrity check**: if the lvol is still referenced by some snapshot / clone, the server may reject the request and return a descriptive error. The error message **must not** also contain the keyword `"not found"` (that would be mis-interpreted as an idempotent success by the client).
- Deletion **must not** be undoable.

### 3.5 `rcow_resize_lvol`

Grow an lvol.

**Request**

```json
{ "lvol_name": "vol-A", "size_gib": 40 }
```

**Success**

```json
{ "bool_value": true, "string_value": "vol-A" }
```

**Requirements:**

- **Grow only**; the client will not issue a shrink request.
- **Must not apply to a snapshot**: if `lvol_name` refers to a snapshot, return an error.
- For an identity resize (new size == current size), the server may return either success, or an error whose message contains the keyword `"new size"` — the client handles both.

### 3.6 `rcow_export_snapshot`

Publish a snapshot to COS.

**Request**

```json
{ "snapshot_name": "snap-1" }
```

**Success**

```json
{ "bool_value": true, "string_value": "a60fc264-d69a-4b59-b8c4-32e37cf6647f" }
```

`string_value` is the opaque `export_uuid`; the client will persist it in its local metadata as the credential for subsequent `rcow_get_snapshot_status` / `rcow_import_lvol` calls.

**Requirements:**

- Only meaningful for a snapshot; the server should return an error when called on an lvol.
- **Stability**: exporting the same snapshot multiple times **must return the same `export_uuid`** — the client assumes the uuid is stable across the snapshot's lifetime.
- The COS upload is asynchronous: this RPC merely registers metadata; the data may still be uploading when the RPC returns.

### 3.7 `rcow_import_lvol`

Materialize a new writable lvol on this node from an `export_uuid`.

**Request**

```json
{ "lvol_name": "restore-A", "export_uuid": "a60fc264-d69a-4b59-b8c4-32e37cf6647f", "decouple": true }
```

| Field | Type | Required | Constraint |
|---|---|---|---|
| `lvol_name` | string | ✅ | Name of the new local lvol; globally unique in the namespace |
| `export_uuid` | string | ✅ | Produced by `rcow_export_snapshot` |
| `decouple` | bool | ✅ | The client always sends `true`; the server is required to complete decoupling within this RPC and return an lvol that is fully self-contained |

**Success**

```json
{ "bool_value": true, "string_value": "restore-A" }
```

**Requirements:**

- The returned lvol **must be self-contained**: the client will **not** call `rcow_decouple_lvol` after import. The server must either synchronously finish decoupling within this RPC, or support transparent on-demand fetching from COS.
- `lvol_name` shares the same namespace with the other create-family RPCs; collision returns `"already exists"`.
- Idempotent: a repeated import with the same `(lvol_name, export_uuid)` returns `"already exists"`.

### 3.8 `rcow_deactive_bdev`

Take down the kernel-side block-device node of an already-activated object.

**Request**

```json
{ "device_name": "vol-A" }
```

**Success**

```json
{ "bool_value": true, "string_value": "vol-A" }
```

**Requirements:**

- **Must be idempotent**: for a `device_name` that is not currently active, return success, or return an error whose message contains the keyword `"not found"`. **Do not** return any other hard error — the client invokes this RPC in a best-effort manner during delete / reset paths, and a hard error would disrupt the subsequent flow.
- Only tears down the NVMe-oF namespace; **does not** delete the underlying object.
- The server will also be called with `__cbc_probe_<uuid>` names (client liveness probes); it should treat those as "not found".

### 3.9 `rcow_active_bdev`

Activate an object and expose it to the kernel as an NVMe-oF namespace.

**Request**

```json
{ "device_name": "vol-A" }
```

**Success** (`string_value` is a **nested JSON string**)

```json
{
  "bool_value": true,
  "string_value": "{\"device_name\":\"vol-A\",\"uuid\":\"0c1649d6-8497-4918-9391-1eedafffe280\",\"nqn\":\"nqn.2026-08.io.spdk:rcow-06\",\"subsys\":6,\"nsid\":1,\"already_active\":true}"
}
```

Nested fields:

| Field | Type | Description |
|---|---|---|
| `device_name` | string | Echo of the request |
| `uuid` | string | Device UUID |
| `nqn` | string | NVMe subsystem NQN |
| `subsys` | number | Subsystem number |
| `nsid` | number | Namespace id |
| `already_active` | bool | `true` = already active, this call is an idempotent replay |

**Requirements:**

- **Applies uniformly to lvol / snapshot / clone** — the server does not differentiate the underlying object kind.
- **Must be idempotent**: a repeated activation returns `already_active: true`, not an error.
- This RPC **does not return `device_path`**; the client will follow up with `rcow_get_bdev` to obtain `/dev/nvmeXnY`.
- The nested JSON must be compact and single-line.

### 3.10 `rcow_get_bdev`

Query the `device_path` of an already-activated object.

**Request**

```json
{ "device_name": "vol-A" }
```

**Success** (`string_value` is a **nested JSON string**)

```json
{
  "bool_value": true,
  "string_value": "{\"device_name\":\"vol-A\",\"uuid\":\"0c1649d6-...\",\"nqn\":\"nqn.2026-08.io.spdk:rcow-06\",\"subsys\":6,\"nsid\":1,\"device_path\":\"/dev/nvme6n1\"}"
}
```

Nested fields (the client only cares about `device_path`):

| Field | Type | Description |
|---|---|---|
| `device_name` | string | Echo of the request |
| `uuid` | string | Device UUID |
| `nqn` | string | NVMe subsystem NQN |
| `subsys` | number | Subsystem number |
| `nsid` | number | Namespace id |
| `device_path` | string | Real host-side block device path that must actually exist (e.g. `/dev/nvme6n1`) |

**Requirements:**

- Only an **already-activated** object should return success; when not active, return an error whose message contains the keyword `"not found"`.
- `device_path` must be a real host-side path that can be `open(2)`ed.
- The nested JSON must be compact and single-line.

### 3.11 `rcow_get_snapshot_status`

Query the upload progress of an exported snapshot and whether it is "safe to delete".

**Request**

```json
{ "export_uuid": "a60fc264-d69a-4b59-b8c4-32e37cf6647f" }
```

**Success** (`string_value` is a **nested JSON string**)

```json
{
  "bool_value": true,
  "string_value": "{\"export_status\":\"INPROGRESS\",\"deletable\":\"NO\"}"
}
```

Nested fields:

| Field              | Value | Description |
|-----------------|---|---|
| `export_status` | `"NONE"` / `"INPROGRESS"` / `"DONE"` | Upload progress to COS |
| `deletable`     | `"YES"` / `"NO"` | Whether this snapshot is currently safe to delete; `"NO"` when an importer on another node still holds a reference |

**Requirements:**

- Only meaningful for `export_uuid`s that have been exported; an invalid uuid should return an error — the client will best-effort swallow the error and leave the status fields empty.
- **Must not mutate server state**: a pure query, no side effects such as triggering an upload or cleanup.
- The client polls the same `export_uuid` at a high rate (once per business-side `get_volume_info` call); the server should respond in O(1) or O(log N).

---

## 4. Idempotency Matrix (Hard Contract)

The following idempotency semantics are the cornerstone of cubecow client crash recovery. **A missing one drives the client into an inconsistent state:**

| RPC | A second call with identical parameters must return |
|---|---|
| `rcow_create_lvol` | `"already exists"` |
| `rcow_create_snapshot` | `"already exists"` |
| `rcow_create_clone` | `"already exists"` |
| `rcow_import_lvol` | `"already exists"` |
| `rcow_delete_lvol` | Contains `"not found"` or `"no such"` |
| `rcow_deactive_bdev` | Success; or contains `"not found"` |
| `rcow_active_bdev` | Success with `already_active: true` in the nested JSON (**not** an error) |
| `rcow_resize_lvol` (identity) | Success; or contains `"new size"` |
| `rcow_export_snapshot` | The same `export_uuid` |
| `rcow_get_bdev` | Idempotent (pure query) |
| `rcow_get_snapshot_status` | Idempotent (pure query) |

---

## 5. Mapping cubecow API to RPC Calls

| cubecow API | Rust `Engine` method | C FFI function | RPC call sequence | Notes |
|---|---|---|---|---|
| **Create + activate volume** | `create_volume` | `cubecow_create_volume` | `rcow_create_lvol` → `rcow_active_bdev` → `rcow_get_bdev`  [+ rollback: `rcow_deactive_bdev` → `rcow_delete_lvol`] | Create implies activate; any failed step triggers rollback |
| Delete volume | `delete_volume` | `cubecow_delete_volume` | `rcow_deactive_bdev` → `rcow_delete_lvol` | deactivate is best-effort |
| Resize | `resize_volume` | `cubecow_resize_volume` | `rcow_resize_lvol` | When `new == old`, no RPC is issued |
| Get volume info | `get_volume_info` | `cubecow_get_volume_info` | If the entry is a snapshot carrying `export_uuid`: `rcow_get_snapshot_status` × 1; otherwise no RPC | Since v0.2 the FFI returns JSON (includes export_uuid/export_status/deletable) |
| Get volume block info | `get_volume_block_info` | `cubecow_get_volume_block_info` | None | Shares `get_volume_info` logic but **skips** the status RPC |
| List volumes | `list_volumes` | `cubecow_list_volumes` | None | Pure in-memory read of `name_index` |
| Create snapshot (source = Volume) | `create_snapshot_from_volume` | `cubecow_create_snapshot_from_volume` | `rcow_create_snapshot` [+ if `activate=true`: `rcow_active_bdev` → `rcow_get_bdev`] | |
| Create snapshot (source = Snapshot) | `create_snapshot_from_volume` | `cubecow_create_snapshot_from_volume` | `rcow_create_clone` → `rcow_create_snapshot` → `rcow_delete_lvol` [+ if `activate=true`: `rcow_active_bdev` → `rcow_get_bdev`] | Flatten via intermediate; tmp clone uses `__cbc_tmpclone_<uuid>` prefix |
| Delete snapshot | `delete_snapshot` | `cubecow_delete_snapshot` | `rcow_deactive_bdev`(snap) → `rcow_delete_lvol`(snap) | deactivate best-effort |
| List snapshots | `list_snapshots` | `cubecow_list_snapshots` | None | Pure in-memory read of `name_index` |
| Activate (volume or snapshot) | `activate_volume` | `cubecow_activate_volume` | `rcow_active_bdev` → `rcow_get_bdev` | v0.2 rule B |
| Deactivate | `deactivate_volume` | `cubecow_deactivate_volume` | `rcow_deactive_bdev` | Only tears down, does not delete |
| Export snapshot | `S3Export::export_snapshot` (trait extension) | `cubecow_s3_export_snapshot` (S3-specific prefix) | `rcow_export_snapshot` | The returned uuid is persisted in the index |
| Import volume | `S3Export::import_lvol` (trait extension) | `cubecow_s3_import_lvol` | `rcow_import_lvol` [+ rollback: `rcow_delete_lvol`] | Rollback only occurs when `persist` fails |
| Reset node | `reset_node_storage` | `cubecow_reset_node_storage` | Per entry: `rcow_deactive_bdev` → `rcow_delete_lvol`; order Snapshot → Volume | deactivate best-effort |
| Metrics | `metrics` | `cubecow_get_metrics` | None | Reads local counters only |
| **Startup initialization** | `S3Engine::initialize` | Triggered by `cubecow_init` | `rcow_deactive_bdev` × 1 (`__cbc_probe_*`, liveness probe) [+ per residual tmp clone: `rcow_delete_lvol`] | The probe expects a `not found` keyword |

