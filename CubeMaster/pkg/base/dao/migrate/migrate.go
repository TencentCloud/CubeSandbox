// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package migrate wraps goose with (1) a driver SessionLocker for whole Up
// runs and (2) per-file locks inside SQL (baseline helper). New engines add
// migrations/<dialect>/ and an entry in dialectSpecs.
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

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

// nil store disables the fingerprint defence layer for that dialect.
type dialectSpec struct {
	dialect database.Dialect
	rootFS  fs.FS
	subdir  string
	store   fingerprintStore
}

var dialectSpecs = map[string]dialectSpec{
	"mysql": {
		dialect: database.DialectMySQL,
		rootFS:  mysqlMigrations,
		subdir:  "migrations/mysql",
		store:   &mysqlFingerprintStore{},
	},
	"postgres": {
		dialect: database.DialectPostgres,
		rootFS:  postgresMigrations,
		subdir:  "migrations/postgres",
		store:   &postgresFingerprintStore{},
	},
}

func newProvider(
	dialect string,
	sqlDB *sql.DB,
	locker lock.SessionLocker,
) (*goose.Provider, fs.FS, fingerprintStore, error) {
	spec, ok := dialectSpecs[dialect]
	if !ok {
		return nil, nil, nil, fmt.Errorf("migrate: unknown dialect %q", dialect)
	}
	subFS, err := fs.Sub(spec.rootFS, spec.subdir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("migrate: fs.Sub %q: %w", spec.subdir, err)
	}
	opts := []goose.ProviderOption{
		goose.WithVerbose(true),
		// Timestamp versions can land out of numeric order after merge;
		// migrations are idempotent via *_if_missing.
		goose.WithAllowOutofOrder(true),
	}
	if locker != nil {
		opts = append(opts, goose.WithSessionLocker(locker))
	}
	provider, err := goose.NewProvider(spec.dialect, sqlDB, subFS, opts...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("migrate: new provider: %w", err)
	}
	return provider, subFS, spec.store, nil
}

// Run applies pending migrations. Caller owns sqlDB. Idempotent at HEAD.
func Run(ctx context.Context, sqlDB *sql.DB, dialect string, locker lock.SessionLocker) error {
	provider, subFS, fpStore, err := newProvider(dialect, sqlDB, locker)
	if err != nil {
		return err
	}

	// Preflight runs without goose's session lock; false-positive startup
	// fail is OK, silent false-negative is not.
	var fsFP map[int64]fileFingerprint
	if fpStore != nil {
		fsFP, err = collectFSFingerprints(subFS)
		if err != nil {
			return fmt.Errorf("migrate: collect migration fingerprints: %w", err)
		}
		if err := fpStore.EnsureTable(ctx, sqlDB); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		if err := preflightFingerprints(ctx, sqlDB, fsFP, fpStore); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	results, upErr := provider.Up(ctx)
	// Record even on partial Up failure so applied versions are not left unfingerprinted.
	if fpStore != nil {
		recErr := recordFingerprints(ctx, sqlDB, fsFP, appliedResults(results, upErr), fpStore)
		if recErr != nil {
			if upErr != nil {
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

// On PartialError, goose puts successful applies in PartialError.Applied, not results.
func appliedResults(results []*goose.MigrationResult, upErr error) []*goose.MigrationResult {
	var pErr *goose.PartialError
	if errors.As(upErr, &pErr) {
		return pErr.Applied
	}
	return results
}

// DownTo is for tests and emergency operator use; production startup only calls Run.
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
