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

// TestMigrationFilenames enforces the migration naming policy documented in
// migrations/mysql/README.md. It runs without a database (no docker needed) so
// it can gate every PR in CI.
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
	// Path is relative to the package directory (Go runs `go test` with the cwd
	// set to the package dir). In CI this is invoked as
	// `working-directory: CubeMaster` + `go test ./pkg/base/dao/migrate/`.
	const migrationsDir = "migrations/mysql"

	// historicalMaxVersion is the last version of the frozen sequential block.
	// Do NOT bump this; new migrations must be timestamp-based.
	const historicalMaxVersion = 10

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
	readme := filepath.Join("migrations", "mysql", "README.md")
	if _, err := os.Stat(readme); err != nil {
		t.Errorf("expected migration policy doc at %s: %v", readme, err)
	}
}
