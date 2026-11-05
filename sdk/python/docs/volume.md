# Persistent Volumes

`Volume` is the class-level helper for managing CubeSandbox **persistent
volumes** — e2b-compatible stores backed by a volume plugin (COS, NFS, …).
Create a volume, mount it into a sandbox via
`Sandbox.create(volume_mounts={...})` (e2b-compatible mapping), and its data
survives sandbox restarts and can be shared across sandboxes.

```python
from cubesandbox import Sandbox, Volume, VolumeMount
```

All `Volume` methods are **class methods** — no instance needed. Mirroring
e2b, `create` and `connect` return a live `Volume` **instance** (carrying
`volume_id` / `name` / `token`), while `list` / `get_info` return plain
`VolumeInfo` data and `destroy` returns a `bool`.

---

## Configuration

Volume management calls only hit the management plane (`CUBE_API_URL`).
Reading/writing files *inside* a mounted volume goes through the data plane, so
`CUBE_PROXY_NODE_IP` is also required when running outside the CubeProxy node.

| Environment Variable | Required | Default | Used by |
|---|:---:|---|---|
| `CUBE_API_URL` | ✅ | `http://127.0.0.1:3000` | all `Volume.*` calls |
| `CUBE_PROXY_NODE_IP` | remote | — | `sb.files.*` on the mounted volume |
| `CUBE_TEMPLATE_ID` | for mount | — | `Sandbox.create(...)` |

You can also pass an explicit `Config` to every method via `config=`.

---

## API reference

| Method | HTTP | Parameters | Returns |
|---|---|---|---|
| `Volume.create(name=None, *, driver=None, config=None)` | `POST /volumes` | See parameter table below | `Volume` |
| `Volume.create_with_private_data(name=None, *, private_data, driver=None, config=None)` | `POST /v2/volumes` | Opaque string input; Cube/SDK ≥ 0.8.0 | `Volume` |
| `Volume.connect(volume_id, *, config=None)` | `GET /volumes/{id}` | `volume_id`: identifier | `Volume` |
| `Volume.list(*, config=None)` | `GET /volumes` | — | `list[VolumeInfo]` (**`token` always empty**) |
| `Volume.get_info(volume_id, *, config=None)` | `GET /volumes/{id}` | `volume_id`: identifier | `VolumeInfo` (with `token`) |
| `Volume.destroy(volume_id, *, config=None)` | `DELETE /volumes/{id}` | `volume_id`: identifier | `bool` (`True` on success, `False` on 404) |

**`Volume.create` parameters**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | `str \| None` | No | Volume name. Must match `^[a-zA-Z0-9_-]+$`, ≤128 chars. Omit for a server-assigned UUID. |
| `driver` | `str \| None` | No | Volume plugin name (e.g. `"cos"`). Pass `None` or `""` to omit the field → backend uses its default plugin. |
| `config` | `Config \| None` | No | SDK config, overrides environment variables. |

### `create`

Modeled on e2b, where `Volume.create(name)` takes no driver:

- **`create(name)`** — e2b-compatible. The request body is just
  `{"name": ...}`; the backend uses its **first configured** volume plugin.
- **`create(name, driver="cos")`** — CubeSandbox extension. Pass a non-empty
  `driver` to pin a specific plugin. Use it only when you must select among
  multiple plugins.

> When `name` is omitted the server generates a UUID and uses it as both the
> name and the volume ID.

### Plugin input

For plugin input, use `Volume.create_with_private_data("my-data", driver="my-storage", private_data='{"cred_ref":"account-7"}')`.
It requires CubeAPI, every serving CubeMaster, and Cubelet at version 0.8.0 or
newer, a plugin that supports this protocol, and the Python SDK at version
0.8.0 or newer. Existing `Volume.create` calls keep their
original definition and route. `private_data` must be valid UTF-8 of at most
1024 bytes, contain no NUL, and contain no characters outside the BMP. Use
credential references, never plaintext secrets. Empty strings are forwarded;
API versions without `/v2/volumes` raise an error without a legacy fallback.
Validation checks the decoded string once: ASCII text such as the literal
`\uD83D\uDE00` remains opaque and is not parsed again. Query, delete and mount
the returned `Volume` through the existing APIs.

Upgrade order: check or upgrade the Controller/Node plugins for Create input A
and Attach/Destroy state B, upgrade **all CubeMasters serving volume operations**,
then deploy the new CubeAPI and begin using this method. A new API connected to
an old Master can return 201 while silently dropping A; the old Master also omits
B on Destroy. The SDK does not detect this mixed-version deployment. Keep compatible
Masters serving existing A/B-dependent volumes during any rollback. See the
[plugin upgrade guide](../../../docs/guide/volume-plugin.md#private_data-upgrade-order).

### `VolumeInfo`

| Attribute | Type | Notes |
|---|---|---|
| `.volume_id` | `str` | Stable identifier (name or auto UUID). Maps from `volumeID`/`volume_id`. |
| `.name` | `str` | Display name. |
| `.token` | `str` | Plugin-issued token. Populated on `create` / `get_info`; **empty on `list`**. |

### Mounting and per-sandbox access modes

Pass a dict mapping mount paths to volumes (e2b-compatible):

```python
Sandbox.create(volume_mounts={"/workspace": vol})
Sandbox.create(volume_mounts={"/workspace": "vol-xxx"})
```

Each value can be a `Volume`, `VolumeInfo`, or bare `volume_id` string and must resolve to an existing volume. Existing e2b-style values remain read-write.

To make one sandbox attachment read-only, wrap its value in `VolumeMount`:

```python
from cubesandbox import Sandbox, VolumeMount

Sandbox.create(
    volume_mounts={
        "/dataset": VolumeMount(vol, read_only=True),
        "/workspace": workspace_vol,
    }
)
```

`read_only` only mounts that attachment as read-only. It prevents the current sandbox from writing or modifying files through that mount, but does not create an immutable snapshot. The same Volume can be attached with different access modes in different sandboxes, for example read-write in sandbox A and read-only in sandbox B.

---

## Examples

### 1. Create

```python
from cubesandbox import Volume

vol = Volume.create("my-data")     # with a name
print(vol.volume_id, vol.name, vol.token)

vol = Volume.create()              # omit name → server-assigned UUID
print(vol.volume_id)               # auto-generated UUID
```

### 2. Mount into a sandbox

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="cos")

with Sandbox.create(
    volume_mounts={"/workspace": vol},
) as sb:
    sb.files.write("/workspace/note.txt", "persisted!")
    print(sb.files.read("/workspace/note.txt"))   # "persisted!"
```

### 3. Query and destroy

```python
for v in Volume.list():            # note: v.token is "" here
    print(v.volume_id, v.name)

one = Volume.get_info(vol.volume_id)  # one.token is populated
vol = Volume.connect(vol.volume_id)   # -> live Volume instance
Volume.destroy(vol.volume_id)         # kill all mounting sandboxes first (see Notes)
```

### 4. Cross-sandbox data sharing

Multiple sandboxes can mount the same volume concurrently. Data is visible
to all mounters in real time.

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("shared", driver="cos")
mount = {"/workspace": vol}

# Sandbox A writes data.
a = Sandbox.create(volume_mounts=mount)
a.files.write("/workspace/probe.txt", "hello from A")

# Sandbox B mounts the same volume and reads it immediately.
with Sandbox.create(volume_mounts=mount) as b:
    print(b.files.read("/workspace/probe.txt"))   # "hello from A"

# ⚠️ All mounting sandboxes must be killed before deleting the volume.
a.kill()
b.kill()  # context manager already killed it on exit; explicit for clarity
Volume.destroy(vol.volume_id)
```

---

## Errors & status codes

Every `Volume` method routes non-2xx responses through the same mapping:

| HTTP status | Exception raised | Meaning |
|---|---|---|
| 2xx | — (returns normally) | success |
| 401 / 403 | `AuthenticationError` | unauthenticated / forbidden |
| 404 | `VolumeNotFoundError` | volume does not exist (`get_info` / `connect` / `destroy`) |
| any other non-2xx (400 / 405 / 409 / 500 …) | `ApiError` | bad params, name conflict, backend error |

Client-side validation raises **before** any network call:

| Condition | Exception |
|---|---|
| `name` violates `^[a-zA-Z0-9_-]+$` or > 128 chars | `ValueError` |
| mount dict missing `name` / `path` | `ValueError` |

All API exceptions derive from `CubeSandboxError` and expose `.status_code`:

```python
from cubesandbox import Volume, VolumeNotFoundError, ApiError

try:
    Volume.get_info("does-not-exist")
except VolumeNotFoundError:
    ...                       # 404
except ApiError as e:
    print(e.status_code)      # 500
```

---

## Notes

- **Kill all mounting sandboxes before deleting a volume.** A volume can be
  mounted by multiple sandboxes concurrently. `Volume.destroy()` does not
  auto-detach — if any running sandbox still holds the volume, the delete may
  fail or leak backend mounts. Always `sb.kill()` every sandbox that mounts the
  volume before calling `Volume.destroy(volume_id)`.
- **`list` never returns tokens.** Tokens are only surfaced by `create` and
  `get_info`; call `Volume.get_info(volume_id)` when you need the token.
- **`name` is optional.** Omit it and the server assigns a UUID that serves as
  both `volume_id` and `name`.
- **Read-only is per attachment.** Use `VolumeMount(volume, read_only=True)` at sandbox creation time. Creating the `Volume` itself remains unchanged.
