# cubecow

A Copy-On-Write storage engine built on xfs-reflink (FICLONE), shipped as a Rust library crate that provides thin-provisioned volume management and O(1) snapshot/clone capabilities.

## Overview

cubecow is a library crate that exposes a uniform interface through the `Engine` trait. It currently ships with a single backend:

- **xfs-reflink (`ReflinkEngine`)** — uses regular files as volumes and snapshots inside a directory on a FICLONE-capable filesystem (XFS with reflink=1 / Btrfs / OCFS2 / …), and relies on the kernel `FICLONE` ioctl for O(1) metadata snapshots.


## Key Features

| Feature | Description |
|---------|-------------|
| **O(1) snapshots** | Powered by the kernel `FICLONE` ioctl: extents are shared rather than byte-copied |
| **Flat snapshot model** | Every snapshot is an independent file; deleting one snapshot never affects any other |
| **O(1) clones** | Taking a snapshot of a snapshot is also a FICLONE — cost depends only on the source file's extent count |
| **Simple crash recovery** | Volumes and snapshots *are* files; on restart the in-memory index is rebuilt by scanning `<root_dir>/volumes/`. No standalone ledger required |
| **Backend-agnostic API** | All callers go through `dyn Engine`; adding a new backend does not require any change to the FFI / SDK callers |

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│                Public API layer (lib.rs)                  │
│   Engine trait + Volume / Snapshot / VolumeBlockInfo /   │
│           initialize / initialize_without_logging       │
├──────────────────────────────────────────────────────────┤
│                C FFI layer (ffi.rs)                       │
│   #[no_mangle] extern "C" fn cubecow_*(...)              │
│   → header file include/cubecow.h                         │
├──────────────────────────────────────────────────────────┤
│              Backend implementation (engine/)             │
│   engine::reflink::ReflinkEngine                          │
│       ├── name_index: RwLock<HashMap<String, VolumeInfo>> │
│       ├── volumes_dir: <root_dir>/volumes/                │
│       └── direct FICLONE / unlink / fsync_dir calls       │
├──────────────────────────────────────────────────────────┤
│              Infrastructure (pkg/)                        │
│   errors / logger / metrics                               │
└──────────────────────────────────────────────────────────┘
```

## On-disk layout for volumes and snapshots

```
<root_dir>/                         # AppConfig.backend.reflink.root_dir
└── volumes/
    ├── <vol-A>/
    │   ├── <vol-A>          # main volume file, FICLONE source
    │   ├── <snap-A1>        # snapshot derived from vol-A
    │   └── <snap-A2>        # snapshot derived from vol-A
    └── <vol-B>/
        ├── <vol-B>
        └── <snap-B1>
```

Every snapshot is "flattened" into the directory of its ultimate origin volume; deleting a snapshot is a single `unlink` and never touches any other snapshot's extents.

## Build

### Prerequisites

- Rust 1.93+ (installation via `rustup` is recommended)
- A FICLONE-capable filesystem to host `root_dir` (production setups should use `mkfs.xfs -m reflink=1,crc=1`)

### Compile

```bash
# Debug
cargo build

# Release (produces target/release/libcubecow.{so,a})
cargo build --release
```

### Unit tests / benchmarks

```bash
# Unit tests (no root required; nothing is actually mkfs'd or mounted)
cargo test --lib

# End-to-end reflink performance benchmark (requires root + xfsprogs + losetup)
sudo cargo bench --bench reflink_ops
```

## Configuration

Both TOML and JSON are supported; the JSON form is consumed directly by the FFI entry points `cubecow_init_*_from_json` as a string. The schema is intentionally minimal:

### TOML

```toml
[log]
level = "info,h2=warn,hyper=warn"
format = "compact"               # "json" | "compact" | "pretty"
file = "/var/log/cubecow.log"    # optional; logs go to stdout when unset
rotation = "daily"               # "daily" | "hourly" | "never"

[backend]
kind = "reflink"                 # the only backend currently supported

[backend.reflink]
root_dir = "/var/lib/cubecow/reflink"
```

### JSON

```json
{
  "log": {
    "level": "info",
    "format": "compact"
  },
  "backend": {
    "kind": "reflink",
    "reflink": { "root_dir": "/var/lib/cubecow/reflink" }
  }
}
```

`root_dir` must be an absolute path whose mount point supports FICLONE. During `initialize` the engine performs a one-shot FICLONE probe; if it fails, startup is rejected outright.

## Rust SDK usage

```rust
use cubecow::{config::AppConfig, Engine};

let config = AppConfig::load("/etc/cubecow.toml")?;
let engine: Box<dyn Engine> = cubecow::initialize(config)?;

// Create a volume
let vol = engine.create_volume("my-vol", 10 * 1024 * 1024 * 1024)?;
println!("device path: {}", vol.device_path);

// Create a snapshot (metadata-only; do not activate immediately)
let snap = engine.create_snapshot("my-vol", "my-snap", false)?;

// List snapshots
let (snaps, _next) = engine.list_snapshots("my-vol", 10, None);
```

## C FFI / Go integration

The C header lives at [include/cubecow.h](include/cubecow.h). The `cubecow_` function prefix is preserved to keep ABI compatibility. A typical call flow:

```c
#include "cubecow.h"

CubecowEngineHandle eng = cubecow_init_from_json(json_blob);
if (!eng) {
    fprintf(stderr, "init failed: %s\n", cubecow_last_error());
    return 1;
}

char* dev = NULL;
int rc = cubecow_create_volume(eng, "my-vol", 10ULL << 30, &dev);
if (rc == COW_OK) {
    printf("created at %s\n", dev);
    cubecow_free_string(dev);
}
cubecow_shutdown(eng);
```

A Go example lives at [examples/go-test/](examples/go-test/) and uses cgo to link against `-lcubecow`:

```bash
# 1. Build the Rust shared lib
cargo build --release

# 2. Build & run the Go test binary
cd examples/go-test
make run
```