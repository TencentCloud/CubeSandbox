# cubedb

Shared database migration and data-access package for the CubeSandbox
platform. Used by both [CubeMaster](../CubeMaster) and
[CubeOps](../CubeOps).

## What's Included

### `migrate/` — Schema Migration Engine

Wraps [goose v3](https://github.com/pressly/goose) with two additional
defence layers:

1. **Content-fingerprint tamper detection** — Records the SHA-256 of every
   migration file at apply time. On startup, verifies that already-applied
   migration files haven't been modified. Turns goose's silent skip into a
   loud startup error.

2. **Cluster-wide session lock** — Uses MySQL `GET_LOCK` to serialise
   migration runs across multiple instances starting simultaneously.

**Idempotent**: `Run()` returns `nil` when the database is already at HEAD,
so it's safe to call on every process start. Any service (CubeMaster or
CubeOps) can start first — the second one simply finds the schema already
up-to-date.

### `dao/` — Connection Pool & Driver Registry

Provides a thin, driver-agnostic data-access facade:

- `Open(ctx, cfg)` — Opens a `*gorm.DB` connection (GORM wraps the raw
  `*sql.DB` so goose can run migrations on the same pool)
- `Migrate(ctx)` — Runs pending migrations
- `Default()` — Returns the global `*gorm.DB` handle
- `SQL()` — Returns the raw `*sql.DB` (for tests / migration package)
- `HealthCheck(ctx, timeout)` — Quick connectivity ping

### `dao/driver/mysql/` — MySQL Driver

Blank-import from `main.go` to register the MySQL driver:

```go
import (
    "github.com/tencentcloud/CubeSandbox/cubedb/dao"
    _ "github.com/tencentcloud/CubeSandbox/cubedb/dao/driver/mysql"
)
```

## Usage

```go
package main

import (
    "context"
    "fmt"

    "github.com/tencentcloud/CubeSandbox/cubedb/dao"
    _ "github.com/tencentcloud/CubeSandbox/cubedb/dao/driver/mysql"
)

func initDB(ctx context.Context, cfg dao.Config) error {
    _, err := dao.Open(ctx, cfg)
    if err != nil {
        return fmt.Errorf("open database: %w", err)
    }
    if err := dao.Migrate(ctx); err != nil {
        return fmt.Errorf("schema migration failed: %w", err)
    }
    return nil
}
```

## Adding a New Migration

1. Create a new SQL file in `migrate/migrations/mysql/` with a timestamp
   prefix (e.g. `20260710120000_add_new_table.sql`).
2. Write the migration using goose `-- +goose Up` / `-- +goose Down` annotations.
3. Both CubeMaster and CubeOps will automatically apply it on next restart.

**Never edit an already-applied migration file** — the fingerprint layer
will reject it. Always add a new migration instead.

## Migration Files

Currently 14 migration files covering:
- Baseline schema (v0.2.2)
- AgentHub instances, settings, templates, snapshots
- Node component versions
- Cube egress
- Template replica compatibility
- OpenClaw persistence fields
- Template image pull progress
- Artifact node placement
- Snapshot runtime active binding
