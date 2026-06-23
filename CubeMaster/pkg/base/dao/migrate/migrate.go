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
	"errors"
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
	// fingerprints enables the content-fingerprint defence layer. The
	// fingerprint store SQL is MySQL-specific (ON DUPLICATE KEY UPDATE,
	// information_schema, ENGINE=InnoDB), so a future dialect must provide its
	// own implementation before flipping this on.
	fingerprints bool
}

var dialectSpecs = map[string]dialectSpec{
	"mysql": {
		dialect:      database.DialectMySQL,
		rootFS:       mysqlMigrations,
		subdir:       "migrations/mysql",
		fingerprints: true,
	},
}

// newProvider creates a goose Provider wired with this package's conventions:
// verbose logging and allow-out-of-order for timestamped migrations.
func newProvider(
	dialect string,
	sqlDB *sql.DB,
	locker lock.SessionLocker,
) (*goose.Provider, fs.FS, bool, error) {
	spec, ok := dialectSpecs[dialect]
	if !ok {
		return nil, nil, false, fmt.Errorf("migrate: unknown dialect %q", dialect)
	}
	subFS, err := fs.Sub(spec.rootFS, spec.subdir)
	if err != nil {
		return nil, nil, false, fmt.Errorf("migrate: fs.Sub %q: %w", spec.subdir, err)
	}
	opts := []goose.ProviderOption{
		goose.WithVerbose(true),
		// New migrations use immutable, globally-unique UTC-timestamp version
		// numbers (see migrations/<dialect>/README.md), so a migration authored
		// earlier but merged later can legitimately have a version lower than an
		// environment's current max. Allow goose to apply such out-of-order
		// migrations instead of hard-failing startup. Safe because every
		// migration is written idempotently via the *_if_missing helpers.
		goose.WithAllowOutofOrder(true),
	}
	if locker != nil {
		opts = append(opts, goose.WithSessionLocker(locker))
	}
	provider, err := goose.NewProvider(spec.dialect, sqlDB, subFS, opts...)
	if err != nil {
		return nil, nil, false, fmt.Errorf("migrate: new provider: %w", err)
	}
	return provider, subFS, spec.fingerprints, nil
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
	provider, subFS, fpEnabled, err := newProvider(dialect, sqlDB, locker)
	if err != nil {
		return err
	}

	// Defence layer (MySQL only): detect (and loudly reject) the case where an
	// already-applied migration version's content changed on disk, which goose
	// would otherwise skip silently. Must run before goose.Up.
	//
	// Note: the preflight reads goose_db_version / the fingerprint table WITHOUT
	// holding goose's session lock (goose only takes it inside provider.Up). A
	// concurrent instance's Up()/DownTo() could therefore move the bookkeeping
	// between our read and goose's run. This is safe: InnoDB MVCC prevents dirty
	// reads, and the worst case is a false-positive (startup blocked), never a
	// false-negative (silent pass). Concurrent DownTo is an operator-only path.
	var fsFP map[int64]fileFingerprint
	if fpEnabled {
		fsFP, err = collectFSFingerprints(subFS)
		if err != nil {
			return fmt.Errorf("migrate: collect migration fingerprints: %w", err)
		}
		if err := ensureFingerprintTable(ctx, sqlDB); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		if err := preflightFingerprints(ctx, sqlDB, fsFP); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	results, upErr := provider.Up(ctx)
	// Record fingerprints for everything that actually applied — even on partial
	// failure — so a fixed re-deploy never leaves an applied version
	// unfingerprinted (which would reopen the silent-skip gap).
	if fpEnabled {
		recErr := recordFingerprints(ctx, sqlDB, fsFP, appliedResults(results, upErr))
		if recErr != nil {
			if upErr != nil {
				// Both goose and fingerprinting failed: surface both so the
				// operator sees the fingerprint gap alongside the goose error.
				return fmt.Errorf("migrate: goose up: %w; fingerprint recording: %w", upErr, recErr)
			}
			return fmt.Errorf("migrate: %w", recErr)
		}
	}
	if upErr != nil {
		return fmt.Errorf("migrate: goose up: %w", upErr)
	}
	return nil
}

// appliedResults returns the migrations goose actually applied. On partial
// failure goose returns (nil, *PartialError) with the successfully-applied
// migrations in PartialError.Applied rather than in the results slice, so we
// must unwrap it to avoid losing fingerprints for the migrations that did run.
func appliedResults(results []*goose.MigrationResult, upErr error) []*goose.MigrationResult {
	var pErr *goose.PartialError
	if errors.As(upErr, &pErr) {
		return pErr.Applied
	}
	return results
}

// DownTo rolls back migrations to (and including) the given version. It
// is intended for tests and emergency operator use; production startup
// only ever calls Run.
func DownTo(ctx context.Context, sqlDB *sql.DB, dialect string, locker lock.SessionLocker, version int64) error {
	provider, _, _, err := newProvider(dialect, sqlDB, locker)
	if err != nil {
		return err
	}
	if _, err := provider.DownTo(ctx, version); err != nil {
		return fmt.Errorf("migrate: goose down-to %d: %w", version, err)
	}
	return nil
}
