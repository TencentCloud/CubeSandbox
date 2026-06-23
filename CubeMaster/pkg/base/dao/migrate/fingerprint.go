// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

// This file implements the "content fingerprint" defence layer described in
// migrations/mysql/README.md.
//
// goose identifies a migration solely by the integer version parsed from its
// filename. Once a version integer is recorded in goose_db_version, goose never
// re-reads that file and never compares its content. That makes the following
// failure mode SILENT: a developer reuses an integer version (e.g. via a rebase
// rename) for a different file, and the new content is skipped forever.
//
// The fingerprint table records the sha256 of every migration that goose has
// ACTUALLY applied. On every start, we verify that the on-disk content of each
// currently-applied & previously-fingerprinted version still matches. A
// mismatch turns the silent skip into a loud, actionable startup error.
//
// We deliberately do NOT backfill fingerprints for versions that were applied
// before this feature existed: we have no record of what was truly applied, so
// recording the current on-disk content would give false confidence and could
// mask an environment that is already in the bad state.
//
// All SQL below is MySQL-specific (ON DUPLICATE KEY UPDATE, information_schema,
// DATABASE(), ENGINE=InnoDB). It is gated behind dialectSpec.fingerprints.
// BEFORE adding a second dialect, extract a fingerprintStore interface
// (ensureTable / loadStored / currentlyApplied / recordOne) and provide a
// per-dialect implementation rather than branching inside these functions.

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
	// fingerprintTable stores one row per migration version that goose has
	// applied, together with the sha256 of the file content at apply time.
	fingerprintTable = "t_cubemaster_migration_fingerprint"

	// gooseVersionTable is goose's own bookkeeping table. We only read it.
	gooseVersionTable = "goose_db_version"

	// skipFingerprintEnv lets an operator bypass the content check when they
	// intentionally need to change an already-applied migration (rare; an
	// applied migration is supposed to be immutable). Any non-empty value
	// disables the preflight check (recording still happens).
	skipFingerprintEnv = "CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK"
)

// ErrFingerprintMismatch is returned by the preflight check when the on-disk
// content of an already-applied migration differs from the recorded fingerprint.
var ErrFingerprintMismatch = errors.New("migration fingerprint check failed")

// fileFingerprint is the on-disk view of a single migration file.
type fileFingerprint struct {
	version int64
	source  string // base filename, e.g. "0001_baseline_v0_2_2.sql"
	sum     string // hex-encoded sha256 of the raw file bytes
}

// collectFSFingerprints enumerates the embedded migrations FS and returns a map
// of version -> fileFingerprint. Files without a valid numeric version component
// are ignored (mirroring goose's own collection rules).
//
// File discovery deliberately uses the SAME fs.Glob("*.sql") pattern goose uses
// in collectFilesystemSources (provider_collect.go), not fs.ReadDir. Keeping the
// two enumerations identical guarantees we fingerprint exactly the set of files
// goose can apply, so goose can never apply a version we failed to collect.
func collectFSFingerprints(subFS fs.FS) (map[int64]fileFingerprint, error) {
	matches, err := fs.Glob(subFS, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	out := make(map[int64]fileFingerprint, len(matches))
	for _, name := range matches {
		version, verr := goose.NumericComponent(name)
		if verr != nil {
			// Not a versioned migration file; ignore like goose does.
			continue
		}
		if existing, ok := out[version]; ok {
			// Mirror goose's duplicate-version guard so the failure mode is a
			// clear error rather than a silently-dropped migration.
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

// ensureFingerprintTable idempotently creates the fingerprint bookkeeping
// table. Created outside goose (like goose creates goose_db_version) so it is
// available for the preflight that runs before goose.Up.
func ensureFingerprintTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+fingerprintTable+` (
  version bigint NOT NULL,
  sha256 char(64) NOT NULL,
  source varchar(255) NOT NULL DEFAULT '',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return fmt.Errorf("ensure %s: %w", fingerprintTable, err)
	}
	return nil
}

// storedFingerprint is a row from the fingerprint table.
type storedFingerprint struct {
	sum    string
	source string
}

// loadStoredFingerprints returns the recorded fingerprints keyed by version.
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

// currentlyAppliedVersions returns the set of versions whose most recent
// goose_db_version row marks them as applied. If goose has not created its
// version table yet (fresh database), it returns an empty set with no error.
func currentlyAppliedVersions(ctx context.Context, db *sql.DB) (map[int64]bool, error) {
	exists, err := tableExists(ctx, db, gooseVersionTable)
	if err != nil {
		return nil, err
	}
	if !exists {
		return map[int64]bool{}, nil
	}
	// For each version_id keep only the latest row (max id) and require it to
	// be applied. This mirrors goose's own "latest row wins" semantics, so a
	// version that was applied and later rolled back (Down) is NOT reported as
	// applied here.
	rows, err := db.QueryContext(ctx, `
SELECT g.version_id
FROM `+gooseVersionTable+` g
JOIN (
  SELECT version_id, MAX(id) AS max_id
  FROM `+gooseVersionTable+`
  GROUP BY version_id
) latest ON g.id = latest.max_id
WHERE g.is_applied = 1`)
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

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check table %q exists: %w", name, err)
	}
	return n > 0, nil
}

// preflightFingerprints fails fast if any currently-applied, previously-
// fingerprinted version has different content on disk than what was recorded
// when it was applied. This is the loud replacement for goose's silent skip.
//
// Versions that are NOT currently applied are intentionally skipped: this lets
// the operator remediation runbook (delete a goose_db_version row to force a
// re-apply) work without tripping the check.
func preflightFingerprints(
	ctx context.Context,
	db *sql.DB,
	fsFP map[int64]fileFingerprint,
) error {
	if v := os.Getenv(skipFingerprintEnv); v != "" {
		return nil
	}
	applied, err := currentlyAppliedVersions(ctx, db)
	if err != nil {
		return err
	}
	stored, err := loadStoredFingerprints(ctx, db)
	if err != nil {
		return err
	}

	// Operational visibility: surface applied versions that have no fingerprint
	// baseline (typically migrations applied before this feature existed, or one
	// that lost its fingerprint to a partial-failure gap). They are NOT
	// protected against silent content changes.
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
			// A previously-applied migration file vanished from the tree. That
			// is itself a dangerous change (renames/deletes of applied
			// migrations are forbidden) and worth surfacing.
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

// logUnprotectedVersions emits a single concise line listing currently-applied
// versions that have no stored fingerprint and are therefore not covered by the
// content-drift check. Empty input is a no-op (fresh database, or every applied
// version is already fingerprinted).
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

// recordFingerprints upserts the fingerprint for every version that goose
// reported as successfully applied in this run. We only record what was
// actually applied (no backfill of historical versions).
func recordFingerprints(
	ctx context.Context,
	db *sql.DB,
	fsFP map[int64]fileFingerprint,
	results []*goose.MigrationResult,
) error {
	for _, r := range results {
		if r == nil || r.Error != nil || r.Source == nil {
			continue
		}
		ff, ok := fsFP[r.Source.Version]
		if !ok {
			// Should not happen: file discovery is aligned with goose
			// (fs.Glob), so any version goose applied must be collectable. If
			// it ever does happen, do NOT stay silent — that version would lose
			// content-drift protection, defeating this defence layer. We log
			// loudly rather than failing startup of an otherwise-successful
			// migration run.
			log.Printf("migrate: WARNING: applied migration version %d (%q) "+
				"has no on-disk fingerprint source; it will NOT be protected "+
				"against silent content changes",
				r.Source.Version, r.Source.Path)
			continue
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO `+fingerprintTable+` (version, sha256, source)
			 VALUES (?, ?, ?)
			 ON DUPLICATE KEY UPDATE sha256 = VALUES(sha256), source = VALUES(source)`,
			ff.version, ff.sum, ff.source,
		); err != nil {
			return fmt.Errorf("record fingerprint for version %d: %w", ff.version, err)
		}
	}
	return nil
}
