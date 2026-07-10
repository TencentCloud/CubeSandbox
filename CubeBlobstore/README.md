# CubeBlobstore

Storage-side utilities for CubeSandbox. Currently ships one binary,
`cube-snapshot` — a chunked, deduplicated, zstd-compressed archiver that
saves a set of local directories to an S3/COS bucket and restores them
back. Future tools go under `cmd/<name>/` in this same module.

`cube-snapshot` is the archive/migration layer for cubecow snapshots.
It is deliberately storage-backend-agnostic: it does not know or care
what produced the input directories. The caller (Cubelet) decides which
directories to hand it, based on where the snapshot's data physically
lives (see [Snapshot layout contract](#snapshot-layout-contract)).

## On-COS object layout

Content-addressed. Every chunk's key is derived from its plaintext
SHA256, so identical chunks across files (or across snapshots that
share the same underlying data) collapse to a single stored object:

```
<uuid>/manifest.json
<uuid>/chunks/<chunk-plaintext-sha256>
```

The manifest records, per file: the original absolute path, size, mode,
mtime, whole-file SHA256, and an ordered run-length-encoded list of
extents. Each `data` extent carries the per-chunk plaintext SHA256s and
`stored_size`s used to fetch chunks from `<uuid>/chunks/` and reassemble
the file on restore.

## Wire-level properties

- Fixed-size chunks (`--chunk-size`, default 4 MiB). All-zero chunks
  are not uploaded; the manifest records them as zero extents.
  Adjacent same-kind chunks merge into run-length extents.
- Each non-zero chunk is independently zstd-compressed (one frame per
  chunk). The compressed form is kept only when it saves at least 5%;
  otherwise the plaintext is stored. Restore distinguishes the two by
  comparing `stored_size` against the plaintext chunk length
  (`stored == plain` ⇒ plaintext).
- Per-chunk plaintext SHA256 + per-file SHA256 in the manifest; both
  are verified on restore.
- `manifest.json` is uploaded **last** as a transaction marker: a
  `<uuid>` is only considered present once its manifest exists. Restore
  and list refuse to operate on a `<uuid>` without a manifest. If a
  save fails before the manifest lands, the partially uploaded chunks
  under `<uuid>/chunks/` are orphans; `rm --uuid <id> --force` cleans
  them up by prefix.
- Restore recreates sparse files (truncate to size + skip zero runs).

## Snapshot layout contract

`cube-snapshot` is used to archive **cubecow snapshots**. A cubecow
snapshot has two logical parts:

1. **Metadata** — memory ranges description, VM state JSON, snapshot
   spec, and other small structured files produced by the hypervisor
   into a local snapshot directory. This part is
   **backend-independent**: it is always local files on the node that
   took the snapshot, and it never lives inside the cubecow storage
   backend itself.

2. **Bulk data** — the actual rootfs and memory contents. Where this
   physically lives depends on the cubecow backend in use:
   - **Node-local backends** (e.g. filesystem-based): bulk data lands
     in one or more files on the local node.
   - **Shared-storage backends**: bulk data lives inside a shared
     cluster storage system that any node can attach to; the node that
     took the snapshot holds no exclusive copy of it.

The archive contract mirrors this split. Each `--dir` passed to
`cube-snapshot save` is one such logical part:

| Backend kind      | `--dir` count | Contents                        |
|-------------------|---------------|---------------------------------|
| node-local        | 2             | metadata dir, bulk dir          |
| shared-storage    | 1             | metadata dir only               |

For shared-storage backends the bulk data is not archived by
`cube-snapshot` at all; it is expected to be preserved by the
underlying cluster storage system (replication, mirroring, multi-site,
etc.), and to be re-attached on restore by name rather than
re-materialised from an archive.

### Caller responsibilities

The tool itself does not inspect directory contents; it faithfully
archives whatever files it is given. Two contracts are therefore
enforced on the caller (Cubelet), not on `cube-snapshot`:

- **Metadata dir must be portable.** Do not place files whose contents
  reference host-local paths or device nodes (for example, a file
  recording `/dev/<something>` that names a node-local attachment).
  Such files are meaningful only on the originating node; on restore
  the corresponding attach step must recompute them from scratch, not
  read them from the archive.

- **Bulk dir must contain only the raw snapshot payload** — one file
  per cubecow snapshot object, named so that restore can map each
  file back to its cubecow snapshot on the target node.

### Restore behavior

Files are always restored to the absolute paths recorded in the
manifest. The `--dir` flags at save time only determine what gets
archived; they do not appear at restore time and are not used to
"relocate" files. Cross-node restores therefore require the target
node to have the same directory layout as the source node (or to
perform any relocation itself, outside the tool).

## Build

Layout (multi-binary module):

```
CubeBlobstore/
├── go.mod / go.sum
├── Makefile              # build / fmt / test / clean
├── cmd/cube-snapshot/    # the cube-snapshot CLI (main.go)
└── pkg/version/          # build metadata injected via -ldflags
```

Local build (produces a static, pure-Go binary at `bin/cube-snapshot`):

```sh
export GOPROXY=https://goproxy.cn,direct   # or your internal goproxy
make build           # -> bin/cube-snapshot
# or directly:
CGO_ENABLED=0 go build -trimpath -o bin/cube-snapshot ./cmd/cube-snapshot
```

Via the CubeSandbox root build (inside the builder image):

```sh
make cube-snapshot   # -> _output/bin/cube-snapshot
```

`cube-snapshot --version` prints the version/commit/build-time injected
by the Makefile ldflags.

## Usage

```
cube-snapshot save    --uuid <id> --dir <path> [--dir <path> ...]
                      [--chunk-size <bytes>] [--parallel <N>]
                      [--zstd-level <N>] [--no-compress] [--overwrite]
cube-snapshot restore --uuid <id> [--parallel <N>] [--verify] [--fsync]
                      [--save-manifest-to <local_path>]
cube-snapshot ls      [--uuid <id> | --prefix <p>] [--verify]
cube-snapshot rm      --uuid <id> [--parallel <N>] [--force]
```

Connection options are shared by all subcommands and resolved with
precedence **CLI > env > config file**:

- `--bucket --region --endpoint --secret-id --secret-key`
- env `COS_SECRET_ID` / `COS_SECRET_KEY`
- `--config <path>` (or `/data/cubelet/cos.cfg` when present); reads
  `[cos_config]` keys `secretid`, `secretkey`, `region`, `cos_endpoint`,
  `cos_bucket_name`.

## cos.cfg format

`cube-snapshot` reads connection settings from a cubelet-style INI file.
The path is picked in this order:

1. `--config <path>` — required to exist; missing/unreadable is an error.
2. `/data/cubelet/cos.cfg` — used automatically when present; silently
   skipped otherwise.

Only the `[cos_config]` section is consumed; any other section is ignored.

### Recognized keys

Each setting accepts one of two equivalent names (for compatibility with
existing cubelet configs). CLI flags and environment variables still take
precedence over the file.

| Purpose            | Primary key       | Alias        | Matching CLI flag | Matching env       |
|--------------------|-------------------|--------------|-------------------|--------------------|
| Access key ID      | `secretid`        | `secret_id`  | `--secret-id`     | `COS_SECRET_ID`    |
| Access key secret  | `secretkey`       | `secret_key` | `--secret-key`    | `COS_SECRET_KEY`   |
| Region             | `region`          | —            | `--region`        | —                  |
| Endpoint URL       | `cos_endpoint`    | `endpoint`   | `--endpoint`      | —                  |
| Bucket name        | `cos_bucket_name` | `bucket`     | `--bucket`        | —                  |

Empty values are ignored (they do not clear a previously set value).
Duplicate keys within the section: the last non-empty occurrence wins.

### Syntax rules

- **Comments**: lines starting with `#` or `;` are ignored. Inline
  comments (`key = val # note`) are **not** supported — the `#`/`;` would
  become part of the value.
- **Quoting**: values may be wrapped in single (`'`) or double (`"`)
  quotes; `#` and `;` inside quotes are literal. An unclosed quote is
  read to end of line.
- **Whitespace**: leading/trailing whitespace around keys and values is
  trimmed; internal whitespace in unquoted values is preserved.
- **Case-sensitive**: keys are matched literally in lower case.

### Example

```ini
# /data/cubelet/cos.cfg
[cos_config]
secretid        = AKID_your_access_key_id
secretkey       = your_access_key_secret
region          = ap-guangzhou
cos_endpoint    = https://cos.ap-guangzhou.myqcloud.com
cos_bucket_name = my-snapshots-1250000000
```

Equivalent using the alias names:

```ini
[cos_config]
secret_id  = AKID_your_access_key_id
secret_key = your_access_key_secret
region     = ap-guangzhou
endpoint   = https://cos.ap-guangzhou.myqcloud.com
bucket     = my-snapshots-1250000000
```

### Recommended file permissions

The file holds long-lived credentials; restrict it to the invoking user:

```sh
sudo install -o root -g root -m 0600 cos.cfg /data/cubelet/cos.cfg
```

