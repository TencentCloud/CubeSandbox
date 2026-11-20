# Persistent Volumes

`Volume` is the class-level helper for managing CubeSandbox **persistent
volumes** — e2b-compatible stores backed by a volume plugin (COS, NFS, …).
Create a volume, mount it into a sandbox via
`Sandbox.create(volume_mounts={...})` (e2b-compatible mapping), and its data
survives sandbox restarts and can be shared across sandboxes.

```python
from cubesandbox import Sandbox, Volume, VolumeMount
```

Management calls remain class methods: `create` and `connect` return a live
`Volume` instance; `Volume.list()` returns `VolumeInfo` entries and
`Volume.get_info(id)` returns `VolumeAndToken`; `destroy` returns a `bool`. Instance file operations now follow the E2B
Volume API: `vol.read_file`, `vol.write_file`, `vol.list(path)`,
`vol.get_info(path)`, `vol.make_dir`, `vol.exists`, `vol.update_metadata`,
and `vol.remove`.

**Implementation status:** Python Volume file methods use the management API to obtain short-lived volume-scoped credentials, then access S3-compatible storage directly with `boto3`; install `cubesandbox[s3]` (or the local `.[s3]` extra). The SDK does not create a helper sandbox or proxy file bytes through CubeMaster. The credential Hook is not yet implemented by the stock S3/COS example plugins, so deployment must provide a Hook that vends credentials scoped to the volume prefix and the agreed TTL.

---

## Configuration

Volume management and short-lived storage-credential issuance use the management plane (`CUBE_API_URL`). Direct S3-compatible file operations use the `endpoint`, `region`, bucket and addressing style returned with the credentials; they do not require `CUBE_VOLUME_API_URL` or `CUBE_PROXY_NODE_IP`. The latter remains for sandbox `sb.files.*` operations on a mounted volume.

| Environment Variable | Required | Default | Used by |
|---|:---:|---|---|
| `CUBE_API_URL` / `E2B_API_URL` | override | bound Config default | volume CRUD and credential issuance |
| `CUBE_API_KEY` / `E2B_API_KEY` | optional | none | sent only to the management API |
| `.[s3]` extra | required for direct object access | — | installs the existing `boto3` S3 client dependency |
| `CUBE_VOLUME_API_URL` / `E2B_VOLUME_API_URL` | legacy gateway only | none | explicit content-token HTTP mode |
| `CUBE_PROXY_NODE_IP` | remote mount access | none | `sb.files.*` on the mounted volume |
| `CUBE_TEMPLATE_ID` | for mount | none | `Sandbox.create(...)` |

Bind `Config(api_url=..., api_key=...)` via `Volume.create/connect(config=...)`. On the first instance file operation, the SDK requests `POST /volumes/{id}/content-token`, caches the S3 credentials in memory, and refreshes them when less than two minutes remain. The management API key is never sent to the object-storage endpoint. The response must include a trusted HTTPS endpoint, bucket, region, `addressing_style`, fixed volume prefix and short-lived session credentials. An explicitly supplied content `token=` selects the legacy HTTP gateway mode instead. The driver Hook is responsible for authorizing the subject and limiting permissions to the volume.

---

## API reference

| Method | HTTP | Parameters | Returns |
|---|---|---|---|
| `Volume.create(name=None, *, driver=None, config=None, **opts)` | `POST /volumes` | See parameter table below | `Volume` |
| `Volume.create_with_private_data(name=None, *, private_data, driver=None, config=None, **opts)` | `POST /v2/volumes` | Opaque string input; Cube/SDK ≥ 0.8.0 | `Volume` |
| `Volume.connect(volume_id, *, config=None)` | `GET /volumes/{id}` | `volume_id`: identifier | `Volume` |
| `Volume.list(*, config=None, **opts)` | `GET /volumes` | E2B management options | `list[VolumeInfo]` (ID and name only) |
| `Volume.get_info(volume_id, *, config=None, **opts)` | `GET /volumes/{id}` | `volume_id`: identifier | `VolumeAndToken` (token and optional domain) |
| `Volume.destroy(volume_id, *, config=None)` | `DELETE /volumes/{id}` | `volume_id`: identifier | `bool` (`True` on success, `False` on 404) |

**`Volume.create` parameters**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | `str \| None` | No | Volume name. Non-empty names must match `^[a-zA-Z0-9_-]+$`, ≤128 chars. Omitted → the server generates a UUID used as both name and ID. |
| `driver` | `str \| None` | No | Volume plugin name (e.g. `"cos"`). Pass `None` or `""` to omit the field → backend uses its default plugin. |
| `config` | `Config \| None` | No | SDK config, overrides environment variables. |

### `create`

Modeled on e2b, where `Volume.create(name)` takes no driver:

- **`create(name)`** — e2b-compatible. The request body is just
  `{"name": ...}`; the backend uses its **first configured** volume plugin.
- **`create(name, driver="cos")`** — CubeSandbox extension. Pass a non-empty
  `driver` to pin a specific plugin. Use it only when you must select among
  multiple plugins.

> When `name` is omitted, the server generates a UUID and uses it as both the
> volume name and the volume ID.

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
| `.token` | `str` | Present on Volume / VolumeAndToken, absent on list VolumeInfo entries. |
| `.domain` | `str \| None` | Optional domain on VolumeAndToken, obtained from the management response. |

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
for v in Volume.list():            # ID and name only; no token field
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

## Direct content API and E2B comparison

The pinned E2B reference is `e2b-dev/E2B@8cc9ed1e`, checked 2026-11-02. The
exact management/content routes, success statuses, options, return shapes and
remaining incompatibilities are documented in the direct-content section of
[volume.zh.md](volume.zh.md). This is not a claim of live E2B interoperability;
the existing compatibility suite's version list covers a separate sandbox API surface.

The Python synchronous SDK implements the direct-content client and short-lived
content-token flow. Node.js currently supports volume management APIs only; it
does not expose these direct file operations. Content 404 is path-not-found
regardless of body shape. AsyncVolume is outside this implementation. Python
wall-clock transfer deadlines and full differential error validation remain
unverified.

| Instance operation | Direct S3 operation | Return / limitation |
|---|---|---|
| `vol.read_file(path, format="text")` | `GetObject` | UTF-8 `str` |
| `vol.read_file(path, format="bytes")` | `GetObject` | raw `bytes` |
| `vol.read_file(path, format="stream")` | streamed `GetObject` | lazy `Iterator[bytes]` |
| `vol.write_file(path, data, ...)` | `PutObject` | accepts text, bytes and readable file objects |
| `vol.list(path, depth=None)` | `ListObjectsV2` | current-level objects and common prefixes |
| `vol.get_info(path)` / `vol.exists(path)` | `HeadObject` / prefix check | object metadata is an approximation |
| `vol.make_dir(path, ...)` | zero-byte directory marker | S3 does not have native directories |
| `vol.remove(path)` | `DeleteObject` | does not recursively remove a directory |
| `vol.update_metadata(path, ...)` | — | unsupported in direct S3 mode |

`Volume.list()` / `Volume.get_info(volume_id)` keep their management meaning.
Instance calls require an absolute path **within the volume**; NUL, backslashes
and dot traversal segments are rejected. The object key is the server-selected
credential prefix plus the path without its leading slash. Client validation
supplements, but never replaces, the Hook's prefix-scoped cloud policy.

`mode`, `uid` and `gid` may be recorded as object metadata, but they do not
provide POSIX permissions. Direct S3 access does not implement atomic rename,
symlinks, locks, recursive directory deletion or JuiceFS/NFS semantics. Pass an
explicit content `token=` to keep using the legacy HTTP content gateway.

```python
import os
from contextlib import closing
from cubesandbox import Config, Volume

config = Config(api_url=os.environ["CUBE_API_URL"], api_key=os.environ["CUBE_API_KEY"])
vol = Volume.connect(os.environ["CUBE_VOLUME_ID"], config=config)
path = "/sdk-example-unique.txt"
vol.write_file(path, "hello S3", force=True)
assert vol.read_file(path) == "hello S3"
with closing(vol.read_file(path, format="stream")) as chunks:
    assert b"".join(chunks) == b"hello S3"
vol.remove(path)
```

### Testing without starting a sandbox

From `sdk/python`, use the local tests and loopback E2E harness first:

```sh
python3 -m pip install -e '.[dev,s3]'
python3 -m pytest -q tests/test_volume.py tests/test_volume_content.py tests/test_volume_s3.py
python3 -m pytest -q tests/test_volume_direct_harness.py
```

The live legacy-gateway suite remains in `test_volume_direct_e2e.py`. Direct S3
read/write is covered by the SDK compatibility E2E case:

```sh
cd ../..
SDK_E2E_DIRECT_S3_VOLUME_ID=<existing-volume> python3 -m pytest \
  tests/e2e/sdk_compat/cases/volume/test_s3_direct_python.py --run-e2e --sdk-e2e-backends=cubesandbox
```

Set `CUBE_API_URL`, `CUBE_API_KEY`, and an existing volume ID whose driver has
a credential hook. The test writes a unique object and removes only that object;
it does not create/connect a sandbox or destroy the volume. The `e2b` backend
automatically skips this CubeSandbox Volume Plugin extension.

### Server integration status and remaining work

The direct S3 request flow is:

```text
SDK --management API key--> CubeAPI /volumes/{id}/content-token
    --> CubeMaster --> driver VolumeContentService.IssueCredentials
SDK --short-lived scoped S3 credentials--> object-storage endpoint
```

File bytes bypass CubeAPI, CubeMaster and Cubelet. The repository contains a
credential protocol and server-side validation, but the stock S3/COS plugins do
not yet implement the issuing Hook; deployments must provide one. `content_enabled`
must be enabled for an RPC driver. Hook authorization must validate the caller's
ownership of the volume and restrict cloud policy to its prefix; do not treat
SDK path validation as an authorization boundary. The SDK direct API is for
S3-compatible object stores and does not promise POSIX-equivalent behavior for
JuiceFS/NFS-backed volumes.

The explicit-token legacy HTTP gateway remains supported separately. Its 404
mapping and metadata response behavior apply only to that mode, not to direct S3.

## Errors & status codes

Volume management uses the mapping below. Direct methods raise
`VolumePathNotFoundException` (alias `VolumePathNotFoundError`) on every 404,
`AuthenticationException` on 401, `RateLimitException` on 429,
`ServiceBusyException` on 503, and `VolumeException` for other HTTP failures (including 403).
Redirects are errors, not credential-forwarding fallbacks. `exists()` suppresses
only path-not-found; authorization, transport and other server errors propagate.


| HTTP status | Exception raised | Meaning |
|---|---|---|
| 2xx | — (returns normally) | success |
| 401 | `AuthenticationException` (alias `AuthenticationError`) | unauthenticated |
| 429 | `RateLimitException` | rate limited |
| 503 | `ServiceBusyException` | service unavailable |
| 404 | `VolumeNotFoundException` for class get_info/connect; path exception for content; destroy returns False | resource missing |
| any other non-2xx (400 / 403 / 405 / 409 / 500 …) | `VolumeException` (also a Cube `ApiError`) | forbidden, bad params, conflict, backend error |

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
- **`name` is required by the Python create signature.** Pass an explicit name.
- **Read-only is per attachment.** Use `VolumeMount(volume, read_only=True)` at sandbox creation time. Creating the `Volume` itself remains unchanged.
