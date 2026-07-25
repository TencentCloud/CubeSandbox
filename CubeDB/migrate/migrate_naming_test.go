// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// historicalMaxVersion is the last version of the frozen sequential block.
// Do NOT bump this; new migrations must be timestamp-based.
const historicalMaxVersion = 10

var migrationDialects = []string{"mysql", "postgres"}

// TestMigrationFilenames enforces the migration naming policy documented in
// migrations/<dialect>/README.md. It runs without a database (no docker needed)
// so it can gate every PR in CI. Covers both mysql and postgres directories.
//
// Policy:
//   - The historical sequential block 0001..0010 is FROZEN: those exact files
//     must exist, must keep their 4-digit names, and no NEW 4-digit sequential
//     file may be added (sequential numbers get reused on rebase and cause
//     silently-skipped migrations).
//   - Every NEW migration must use a 14-digit UTC timestamp prefix:
//     YYYYMMDDhhmmss_<description>.sql.
//   - No two migration files may share the same integer version.
func TestMigrationFilenames(t *testing.T) {
	for _, dialect := range migrationDialects {
		t.Run(dialect, func(t *testing.T) {
			assertMigrationFilenames(t, filepath.Join("migrations", dialect))
		})
	}
}

func assertMigrationFilenames(t *testing.T, migrationsDir string) {
	t.Helper()

	frozen := map[int64]bool{}
	for v := int64(1); v <= historicalMaxVersion; v++ {
		frozen[v] = true
	}

	// 14-digit timestamp prefix + lowercase snake_case description.
	timestampRe := regexp.MustCompile(`^\d{14}_[a-z0-9]+(_[a-z0-9]+)*\.sql$`)
	// 4-digit sequential prefix (only the frozen block may use this shape).
	sequentialRe := regexp.MustCompile(`^(\d{4})_[a-z0-9]+(_[a-z0-9]+)*\.sql$`)

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationsDir, err)
	}

	versionToFile := map[int64]string{}
	seenFrozen := map[int64]bool{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue // README.md and other non-migration files are fine.
		}

		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Errorf("%s: missing '_' separator in filename", name)
			continue
		}
		version, perr := strconv.ParseInt(prefix, 10, 64)
		if perr != nil {
			t.Errorf("%s: version prefix %q is not numeric", name, prefix)
			continue
		}

		if other, dup := versionToFile[version]; dup {
			t.Errorf("duplicate migration version %d: %q and %q", version, other, name)
			continue
		}
		versionToFile[version] = name

		switch {
		case len(prefix) == 4:
			// Only the frozen sequential block may use the 4-digit shape.
			if !sequentialRe.MatchString(name) {
				t.Errorf("%s: invalid sequential filename shape", name)
				continue
			}
			if !frozen[version] {
				t.Errorf("%s: new 4-digit sequential migrations are forbidden; "+
					"use a 14-digit UTC timestamp prefix instead (see %s/README.md)",
					name, migrationsDir)
				continue
			}
			seenFrozen[version] = true
		case len(prefix) == 14:
			if !timestampRe.MatchString(name) {
				t.Errorf("%s: invalid timestamp filename; expected "+
					"YYYYMMDDhhmmss_<snake_case>.sql", name)
				continue
			}
			if version <= historicalMaxVersion {
				t.Errorf("%s: timestamp version %d collides with frozen block", name, version)
			}
		default:
			t.Errorf("%s: version prefix must be the 4-digit frozen block or a "+
				"14-digit UTC timestamp (got %d-digit %q)", name, len(prefix), prefix)
		}
	}

	// Every frozen migration must still be present (catches renames/deletes of
	// the historical block even if CI git-diff is bypassed).
	for v := range frozen {
		if !seenFrozen[v] {
			t.Errorf("frozen migration version %04d is missing or renamed; "+
				"the historical block 0001..%04d must never change",
				v, historicalMaxVersion)
		}
	}
}

// TestMigrationsDirHasReadme is a light guard so the naming policy stays
// discoverable next to the files it governs.
func TestMigrationsDirHasReadme(t *testing.T) {
	for _, dialect := range migrationDialects {
		t.Run(dialect, func(t *testing.T) {
			readme := filepath.Join("migrations", dialect, "README.md")
			if _, err := os.Stat(readme); err != nil {
				t.Errorf("expected migration policy doc at %s: %v", readme, err)
			}
		})
	}
}

// TestMigrationVersionParityAcrossDialects asserts that mysql and postgres
// migration directories contain the same version set and the same description
// suffix for each version. This is a cheap, docker-free gate that catches
// "forgot to port a migration" regressions (the exact class of bug that left
// t_cube_snapshot_runtime_active missing from PostgreSQL).
func TestMigrationVersionParityAcrossDialects(t *testing.T) {
	mysqlFiles := listMigrationVersionFiles(t, filepath.Join("migrations", "mysql"))
	pgFiles := listMigrationVersionFiles(t, filepath.Join("migrations", "postgres"))

	for version, mysqlName := range mysqlFiles {
		pgName, ok := pgFiles[version]
		if !ok {
			t.Errorf("version %d present in mysql (%s) but missing from postgres", version, mysqlName)
			continue
		}
		mysqlSuffix := migrationDescriptionSuffix(mysqlName)
		pgSuffix := migrationDescriptionSuffix(pgName)
		if mysqlSuffix != pgSuffix {
			t.Errorf("version %d: description suffix mismatch: mysql=%q postgres=%q",
				version, mysqlSuffix, pgSuffix)
		}
	}
	for version, pgName := range pgFiles {
		if _, ok := mysqlFiles[version]; !ok {
			t.Errorf("version %d present in postgres (%s) but missing from mysql", version, pgName)
		}
	}
}

// listMigrationVersionFiles maps integer goose version → filename for every
// .sql migration under dir.
func listMigrationVersionFiles(t *testing.T, dir string) map[int64]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			t.Fatalf("%s/%s: missing '_' separator", dir, e.Name())
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("%s/%s: version prefix %q not numeric", dir, e.Name(), prefix)
		}
		if other, dup := out[version]; dup {
			t.Fatalf("%s: duplicate version %d: %q and %q", dir, version, other, e.Name())
		}
		out[version] = e.Name()
	}
	return out
}

// migrationDescriptionSuffix returns the part after the version prefix,
// e.g. "snapshot_runtime_active_binding.sql" for
// "20260701040100_snapshot_runtime_active_binding.sql".
func migrationDescriptionSuffix(filename string) string {
	_, suffix, ok := strings.Cut(filename, "_")
	if !ok {
		return filename
	}
	return suffix
}
