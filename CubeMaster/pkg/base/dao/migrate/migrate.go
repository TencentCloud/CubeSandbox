// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package migrate runs version-controlled schema migrations against the
// shared CubeMaster database. It wraps github.com/pressly/goose/v3 with
// two extras the design calls out:
//
//  1. An outer cluster-wide lock — the lock.SessionLocker handed in by the
//     driver — that serialises whole goose.Up() runs across instances.
//
//  2. An inner per-file lock asserted from inside every .sql migration via
//     a CALL cubemaster_acquire_migration_lock(name, timeout) at the top
//     and a SELECT RELEASE_LOCK at the bottom. The helper procedure is
//     defined once in the baseline migration; it SIGNALs SQLSTATE 45000
//     when GET_LOCK times out or returns NULL so goose aborts cleanly.
//
// New engines plug in by adding a sibling migrations/<dialect>/ folder
// and an entry in dialectSpecs below.
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/mysql/*.sql
var mysqlMigrations embed.FS

// dialectSpec wires a goose dialect to its embedded migrations FS.
type dialectSpec struct {
	dialect database.Dialect
	rootFS  fs.FS
	subdir  string
}

var dialectSpecs = map[string]dialectSpec{
	"mysql": {
		dialect: database.DialectMySQL,
		rootFS:  mysqlMigrations,
		subdir:  "migrations/mysql",
	},
}

// Run applies every pending migration for the given dialect. The caller
// is responsible for opening sqlDB; this function never closes it.
//
// Run is idempotent: if the database is already at HEAD it returns nil
// without touching the schema, so it is safe to call on every process
// start.
//
// When locker is non-nil, the goose Provider takes a cluster-wide lock
// for the duration of the run (outer layer). Per-file inner locks are
// asserted by the SQL migrations themselves via the helper procedure
// established in the baseline migration.
func Run(ctx context.Context, sqlDB *sql.DB, dialect string, locker lock.SessionLocker) error {
	spec, ok := dialectSpecs[dialect]
	if !ok {
		return fmt.Errorf("migrate: unknown dialect %q", dialect)
	}
	subFS, err := fs.Sub(spec.rootFS, spec.subdir)
	if err != nil {
		return fmt.Errorf("migrate: fs.Sub %q: %w", spec.subdir, err)
	}
	opts := []goose.ProviderOption{
		goose.WithVerbose(true),
	}
	if locker != nil {
		opts = append(opts, goose.WithSessionLocker(locker))
	}
	provider, err := goose.NewProvider(spec.dialect, sqlDB, subFS, opts...)
	if err != nil {
		return fmt.Errorf("migrate: new provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate: goose up: %w", err)
	}
	return nil
}

// DownTo rolls back migrations to (and including) the given version. It
// is intended for tests and emergency operator use; production startup
// only ever calls Run.
func DownTo(ctx context.Context, sqlDB *sql.DB, dialect string, locker lock.SessionLocker, version int64) error {
	spec, ok := dialectSpecs[dialect]
	if !ok {
		return fmt.Errorf("migrate: unknown dialect %q", dialect)
	}
	subFS, err := fs.Sub(spec.rootFS, spec.subdir)
	if err != nil {
		return fmt.Errorf("migrate: fs.Sub %q: %w", spec.subdir, err)
	}
	opts := []goose.ProviderOption{goose.WithVerbose(true)}
	if locker != nil {
		opts = append(opts, goose.WithSessionLocker(locker))
	}
	provider, err := goose.NewProvider(spec.dialect, sqlDB, subFS, opts...)
	if err != nil {
		return fmt.Errorf("migrate: new provider: %w", err)
	}
	if _, err := provider.DownTo(ctx, version); err != nil {
		return fmt.Errorf("migrate: goose down-to %d: %w", version, err)
	}
	return nil
}
