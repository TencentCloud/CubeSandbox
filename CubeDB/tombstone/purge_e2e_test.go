// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package tombstone

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeDB/migrate"
	"gorm.io/gorm"
)

// mysqlE2ELocker is a minimal goose SessionLocker (MySQL GET_LOCK) so this
// package's e2e test can drive migrate.Run on its own dockertest container.
// It mirrors CubeDB/migrate's test locker.
type mysqlE2ELocker struct{ name string }

func (l mysqlE2ELocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", l.name).Scan(&got); err != nil {
		return err
	}
	if !got.Valid || got.Int64 != 1 {
		return fmt.Errorf("e2e: GET_LOCK %q failed (got=%v)", l.name, got.Int64)
	}
	return nil
}

func (l mysqlE2ELocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "DO RELEASE_LOCK(?)", l.name)
	return err
}

var _ lock.SessionLocker = mysqlE2ELocker{} // compile-time interface check

// TestE2E_MigrateThenPurgeRealSchema is the true end-to-end test for issue
// #973: it runs the FULL real migration on a fresh MySQL (so the real
// t_cube_sandbox_spec schema + the new idx_sandbox_spec_deleted_at exist),
// seeds tombstones across the retention boundary, then runs one REAL purge
// pass (advisory lock + bounded batched delete) and asserts only the
// retention-expired tombstones are hard-deleted while recent tombstones and
// live rows are untouched. This proves the migration and the purger work
// together against the production schema.
func TestE2E_MigrateThenPurgeRealSchema(t *testing.T) {
	env := newGormEnv(t, "mysql") // mysql-only: pg image is unreachable in this sandbox
	defer env.teardown()

	sqlDB, err := env.db.DB()
	if err != nil {
		t.Fatalf("get *sql.DB: %v", err)
	}
	defer sqlDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1) Apply the real schema (all migrations, incl. the soft-delete-purge index one).
	if err := migrate.Run(ctx, sqlDB, "mysql", mysqlE2ELocker{name: "tombstone_e2e_migrate_lock"}); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}

	// 2) Seed the real t_cube_sandbox_spec across the retention boundary.
	now := time.Now()
	mustSeedSpec(t, env.db, "e2e-old", now.Add(-8*24*time.Hour))    // > 7d  -> purged
	mustSeedSpec(t, env.db, "e2e-recent", now.Add(-1*24*time.Hour)) // < 7d -> kept
	mustSeedSpec(t, env.db, "e2e-live", time.Time{})                // live  -> kept

	// 3) Run one real purge pass (lock + per-table bounded batched delete).
	cfg := Config{
		Enabled:    true,
		Tables:     []string{"t_cube_sandbox_spec"},
		Retention:  7 * 24 * time.Hour,
		BatchSize:  100,
		MaxPerPass: 1000,
		LockName:   "tombstone_e2e_purge_v1",
	}.sanitized()
	runPass(ctx, env.db, cfg, slog.Default())

	// 4) Assert: only the old tombstone is gone; recent + live remain.
	if got := countSpec(t, env.db, "deleted_at IS NOT NULL"); got != 1 {
		t.Errorf("remaining tombstones = %d, want 1 (only the recent one)", got)
	}
	if got := countSpec(t, env.db, "deleted_at IS NULL"); got != 1 {
		t.Errorf("live rows = %d, want 1", got)
	}
	if got := countSpec(t, env.db, "sandbox_id='e2e-old'"); got != 0 {
		t.Errorf("old tombstone was not purged (rows=%d)", got)
	}
	if got := countSpec(t, env.db, "sandbox_id='e2e-recent'"); got != 1 {
		t.Errorf("recent tombstone should survive (rows=%d)", got)
	}
}

// mustSeedSpec inserts a t_cube_sandbox_spec row against the real (migrated)
// schema. request_json is NOT NULL without a default, so it is supplied.
// instance_type is fixed ('cubebox'); a zero deletedAt => live row.
func mustSeedSpec(t *testing.T, db *gorm.DB, sandboxID string, deletedAt time.Time) {
	t.Helper()
	if deletedAt.IsZero() {
		execSpec(t, db, "INSERT INTO t_cube_sandbox_spec (sandbox_id, instance_type, request_json, deleted_at) VALUES (?, 'cubebox', '{}', NULL)", sandboxID)
		return
	}
	execSpec(t, db, "INSERT INTO t_cube_sandbox_spec (sandbox_id, instance_type, request_json, deleted_at) VALUES (?, 'cubebox', '{}', ?)", sandboxID, deletedAt)
}

func execSpec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if res := db.Exec(query, args...); res.Error != nil {
		t.Fatalf("seed: %v", res.Error)
	}
}

func countSpec(t *testing.T, db *gorm.DB, where string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM t_cube_sandbox_spec WHERE " + where).Scan(&n).Error; err != nil {
		t.Fatalf("count(%s): %v", where, err)
	}
	return n
}
