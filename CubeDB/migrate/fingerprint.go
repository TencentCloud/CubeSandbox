// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

// Content fingerprints: goose keys migrations by version int only, so reused
// versions are silent skips. We store sha256 of what was actually applied and
// fail startup on drift. No backfill for pre-feature applied versions (would
// invent a false baseline). Dialect DDL/DML: fingerprintStore via dialectSpec.store.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
)

const (
	fingerprintTable  = "t_cubemaster_migration_fingerprint"
	gooseVersionTable = "goose_db_version"

	// Non-empty value skips the preflight content check (recording still runs).
	skipFingerprintEnv = "CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK"
)

var ErrFingerprintMismatch = errors.New("migration fingerprint check failed")

type fileFingerprint struct {
	version int64
	source  string
	sum     string
}

// Same fs.Glob("*.sql") + NumericComponent rules as goose; duplicate versions error.
func collectFSFingerprints(subFS fs.FS) (map[int64]fileFingerprint, error) {
	matches, err := fs.Glob(subFS, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	out := make(map[int64]fileFingerprint, len(matches))
	for _, name := range matches {
		version, verr := goose.NumericComponent(name)
		if verr != nil {
			continue
		}
		if existing, ok := out[version]; ok {
			return nil, fmt.Errorf(
				"duplicate migration version %d on disk: %q and %q",
				version, existing.source, name,
			)
		}
		b, rerr := fs.ReadFile(subFS, name)
		if rerr != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, rerr)
		}
		sum := sha256.Sum256(b)
		out[version] = fileFingerprint{
			version: version,
			source:  name,
			sum:     hex.EncodeToString(sum[:]),
		}
	}
	return out, nil
}

type storedFingerprint struct {
	sum    string
	source string
}

func loadStoredFingerprints(ctx context.Context, db *sql.DB) (map[int64]storedFingerprint, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT version, sha256, source FROM `+fingerprintTable)
	if err != nil {
		return nil, fmt.Errorf("load fingerprints: %w", err)
	}
	defer rows.Close()
	out := map[int64]storedFingerprint{}
	for rows.Next() {
		var v int64
		var sum, source string
		if err := rows.Scan(&v, &sum, &source); err != nil {
			return nil, fmt.Errorf("scan fingerprint: %w", err)
		}
		out[v] = storedFingerprint{sum: sum, source: source}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fingerprints: %w", err)
	}
	return out, nil
}

// Latest goose_db_version row per version_id wins; rolled-back versions are
// omitted. Missing table → empty. WHERE g.is_applied works for MySQL tinyint
// and PG bool.
func currentlyAppliedVersions(
	ctx context.Context,
	db *sql.DB,
	tableExists func(context.Context, *sql.DB, string) (bool, error),
) (map[int64]bool, error) {
	exists, err := tableExists(ctx, db, gooseVersionTable)
	if err != nil {
		return nil, err
	}
	if !exists {
		return map[int64]bool{}, nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT g.version_id
FROM `+gooseVersionTable+` g
JOIN (
  SELECT version_id, MAX(id) AS max_id
  FROM `+gooseVersionTable+`
  GROUP BY version_id
) latest ON g.id = latest.max_id
WHERE g.is_applied`)
	if err != nil {
		return nil, fmt.Errorf("list applied versions: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan applied version: %w", err)
		}
		out[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied versions: %w", err)
	}
	return out, nil
}

func logUnprotectedVersions(applied map[int64]bool, stored map[int64]storedFingerprint) {
	var unprotected []int64
	for v := range applied {
		if _, ok := stored[v]; !ok {
			unprotected = append(unprotected, v)
		}
	}
	if len(unprotected) == 0 {
		return
	}
	sort.Slice(unprotected, func(i, j int) bool { return unprotected[i] < unprotected[j] })
	strs := make([]string, 0, len(unprotected))
	for _, v := range unprotected {
		strs = append(strs, strconv.FormatInt(v, 10))
	}
	log.Printf("migrate: %d applied migration version(s) have no fingerprint "+
		"baseline and are NOT content-checked (e.g. applied before fingerprinting "+
		"existed): %s",
		len(unprotected), strings.Join(strs, ","))
}

// Fail if an applied+fingerprinted version's on-disk sha256 drifted.
// Unapplied stored rows are ignored (remediation: delete goose_db_version row).
func preflightFingerprints(
	ctx context.Context,
	db *sql.DB,
	fsFP map[int64]fileFingerprint,
	store fingerprintStore,
) error {
	if v := os.Getenv(skipFingerprintEnv); v != "" {
		return nil
	}
	applied, err := store.CurrentlyApplied(ctx, db)
	if err != nil {
		return err
	}
	stored, err := store.LoadStored(ctx, db)
	if err != nil {
		return err
	}

	logUnprotectedVersions(applied, stored)

	if len(stored) == 0 {
		return nil
	}

	var mismatches []string
	for version, sf := range stored {
		if !applied[version] {
			continue
		}
		ff, ok := fsFP[version]
		if !ok {
			mismatches = append(mismatches, fmt.Sprintf(
				"version %d: applied file %q is missing from the migrations tree",
				version, sf.source,
			))
			continue
		}
		if ff.sum != sf.sum {
			mismatches = append(mismatches, fmt.Sprintf(
				"version %d: content changed since it was applied "+
					"(recorded %q sha256=%s, on-disk %q sha256=%s)",
				version, sf.source, sf.sum, ff.source, ff.sum,
			))
		}
	}
	if len(mismatches) == 0 {
		return nil
	}
	sort.Strings(mismatches)
	return fmt.Errorf(
		"%w: an already-applied migration "+
			"version was modified or reused, which goose would otherwise skip "+
			"SILENTLY. Never edit/rename/reuse an applied migration; add a new "+
			"timestamped migration instead. To bypass intentionally, set %s=1.\n  - %s",
		ErrFingerprintMismatch, skipFingerprintEnv, strings.Join(mismatches, "\n  - "),
	)
}

func recordFingerprints(
	ctx context.Context,
	db *sql.DB,
	fsFP map[int64]fileFingerprint,
	results []*goose.MigrationResult,
	store fingerprintStore,
) error {
	for _, r := range results {
		if r == nil || r.Error != nil || r.Source == nil {
			continue
		}
		ff, ok := fsFP[r.Source.Version]
		if !ok {
			log.Printf("migrate: WARNING: applied migration version %d (%q) "+
				"has no on-disk fingerprint source; it will NOT be protected "+
				"against silent content changes",
				r.Source.Version, r.Source.Path)
			continue
		}
		if err := store.RecordOne(ctx, db, ff); err != nil {
			return err
		}
	}
	return nil
}
