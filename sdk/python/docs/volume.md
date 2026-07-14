# Persistent Volumes

`Volume` is the class-level helper for managing CubeSandbox **persistent
volumes** — e2b-compatible stores backed by a volume plugin (COS, NFS, …).
Create a volume, mount it into a sandbox via
`Sandbox.create(volume_mounts=[...])`, and its data survives sandbox restarts
and can be shared across sandboxes.

```python
from cubesandbox import Sandbox, Volume, VolumeMount
```

All `Volume` methods are **class methods** — no instance needed.

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
| `Volume.create(name=None, *, driver=None, config=None)` | `POST /volumes` | See parameter table below | `VolumeInfo` |
| `Volume.list(*, config=None)` | `GET /volumes` | — | `list[VolumeInfo]` (**`token` always empty**) |
| `Volume.get(volume_id, *, config=None)` | `GET /volumes/{id}` | `volume_id`: identifier | `VolumeInfo` (with `token`) |
| `Volume.delete(volume_id, *, config=None)` | `DELETE /volumes/{id}` | `volume_id`: identifier | `None` |

**`Volume.create` parameters**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | `str \| None` | No | Volume name. Must match `^[a-zA-Z0-9_-]+$`, ≤128 chars. Omit for a server-assigned UUID. |
| `driver` | `str \| None` | No | Volume plugin name. Currently supported: `"cos"`. Pass `None` or `""` to omit the field → backend uses its default plugin. |
| `config` | `Config \| None` | No | SDK config, overrides environment variables. |

### `create`: default plugin vs. pinned driver

Modeled on e2b, where `Volume.create(name)` takes **no** driver:

- **`create(name)`** — e2b-compatible. The request body is just
  `{"name": ...}`; CubeMaster falls back to the **first configured** volume
  plugin. Use this by default, including when several plugins are registered
  and "the first one" is acceptable.
- **`create(name, driver="cos")`** — CubeSandbox extension. Pass a non-empty
  `driver` to pin a specific plugin. Use it only when you must select among
  multiple plugins. A `None`/empty `driver` means "unspecified" and the field
  is simply not sent.

> Wire body: without a driver it sends `{"name": "<name-or-empty>"}`; with a
> non-empty `driver` it adds `{"driver": "<driver>"}`. When `name` is omitted the
> server generates a UUID and uses it as both the name and the volume ID.

### `VolumeInfo`

| Attribute | Type | Notes |
|---|---|---|
| `.volume_id` | `str` | Stable identifier (name or auto UUID). Maps from `volumeID`/`volume_id`. |
| `.name` | `str` | Display name. |
| `.token` | `str` | Plugin-issued token. Populated on `create` / `get`; **empty on `list`**. |

### `VolumeMount`

Passed to `Sandbox.create(volume_mounts=[...])`. Either the typed dataclass or a
plain dict is accepted:

```python
VolumeMount(name=<volume_id>, path="/workspace")   # typed
{"name": <volume_id>, "path": "/workspace"}         # dict — equivalent
```

`name` must be an existing `volume_id`; a dict missing `name`/`path` raises
`ValueError`.

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

### 2. Pin a specific plugin

```python
from cubesandbox import Sandbox, Volume, VolumeMount

vol = Volume.create("my-data", driver="cos")

with Sandbox.create(
    volume_mounts=[VolumeMount(name=vol.volume_id, path="/workspace")],
) as sb:
    sb.files.write("/workspace/note.txt", "persisted!")
    print(sb.files.read("/workspace/note.txt"))   # "persisted!"
```

### 3. Query and delete

```python
for v in Volume.list():            # note: v.token is "" here
    print(v.volume_id, v.name)

one = Volume.get(vol.volume_id)    # one.token is populated
Volume.delete(vol.volume_id)       # kill all mounting sandboxes first (see Notes)
```

### 4. Mount into a sandbox

```python
from cubesandbox import Sandbox, Volume, VolumeMount

vol = Volume.create("my-data", driver="cos")

with Sandbox.create(
    volume_mounts=[VolumeMount(name=vol.volume_id, path="/workspace")],
) as sb:
    sb.files.write("/workspace/note.txt", "persisted!")
    print(sb.files.read("/workspace/note.txt"))   # "persisted!"
```

### 5. Cross-sandbox data sharing

Multiple sandboxes can mount the same volume concurrently. Data is visible
to all mounters in real time.

```python
from cubesandbox import Sandbox, Volume, VolumeMount

vol = Volume.create("shared", driver="cos")
mount = [VolumeMount(name=vol.volume_id, path="/workspace")]

# Sandbox A writes data.
a = Sandbox.create(volume_mounts=mount)
a.files.write("/workspace/probe.txt", "hello from A")

# Sandbox B mounts the same volume and reads it immediately.
with Sandbox.create(volume_mounts=mount) as b:
    print(b.files.read("/workspace/probe.txt"))   # "hello from A"

# ⚠️ All mounting sandboxes must be killed before deleting the volume.
a.kill()
b.kill()  # context manager already killed it on exit; explicit for clarity
Volume.delete(vol.volume_id)
```

---

## Errors & status codes

Every `Volume` method routes non-2xx responses through the same mapping:

| HTTP status | Exception raised | Meaning |
|---|---|---|
| 2xx | — (returns normally) | success |
| 401 / 403 | `AuthenticationError` | unauthenticated / forbidden |
| 404 | `VolumeNotFoundError` | volume does not exist (`get` / `delete`) |
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
    Volume.get("does-not-exist")
except VolumeNotFoundError:
    ...                       # 404
except ApiError as e:
    print(e.status_code)      # 500
```

---

## Notes

- **Kill all mounting sandboxes before deleting a volume.** A volume can be
  mounted by multiple sandboxes concurrently. `Volume.delete()` does not
  auto-detach — if any running sandbox still holds the volume, the delete may
  fail or leak backend mounts. Always `sb.kill()` every sandbox that mounts the
  volume before calling `Volume.delete(volume_id)`.
- **`list` never returns tokens.** Tokens are only surfaced by `create` and
  `get`; call `Volume.get(volume_id)` when you need the token.
- **`name` is optional.** Omit it and the server assigns a UUID that serves as
  both `volume_id` and `name`.
