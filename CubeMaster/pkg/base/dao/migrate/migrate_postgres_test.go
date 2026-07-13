// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/migrate"
)

// newPostgres / openPGDB are thin aliases over the shared dockertest fixture.
func newPostgres(t *testing.T) *dbTestEnv { return newPostgresEnv(t) }
func openPGDB(t *testing.T, dsn string) *sql.DB {
	return openPostgresDB(t, dsn)
}

// pgTestSessionLocker uses pg_try_advisory_lock with a test-specific key.
func pgTestSessionLocker() lock.SessionLocker {
	return &pgTestLocker{id: 999999999, timeout: 30}
}

type pgTestLocker struct {
	id      int64
	timeout int
}

func (l *pgTestLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	deadline := time.Now().Add(time.Duration(l.timeout) * time.Second)
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", l.id).Scan(&acquired); err != nil {
			return err
		}
		if acquired {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pg test lock %d: timeout", l.id)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (l *pgTestLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", l.id)
	return err
}

// TestPostgres_Run_Fresh validates the empty-database path for PostgreSQL.
func TestPostgres_Run_Fresh(t *testing.T) {
	env := newPostgres(t)
	defer env.teardown()
	db := openPGDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("migrate.Run (fresh postgres): %v", err)
	}
	assertPGHeadSchema(t, db)
}

// TestPostgres_Run_Idempotent verifies re-running on an already-migrated DB is a no-op.
func TestPostgres_Run_Idempotent(t *testing.T) {
	env := newPostgres(t)
	defer env.teardown()
	db := openPGDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("first migrate.Run: %v", err)
	}
	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("second migrate.Run (idempotent): %v", err)
	}
	assertPGHeadSchema(t, db)
}

func assertPGHeadSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type expect struct {
		table   string
		columns []string
		absent  []string
		indexes []string
	}
	cases := []expect{
		{
			table: "t_cube_template_definition",
			columns: []string{
				"kind", "origin_sandbox_id", "origin_node_id",
				"display_name", "storage_backend", "retain",
				"rootfs_size_bytes_at_snapshot", "rootfs_artifact_id",
			},
			indexes: []string{
				"idx_template_kind_status",
				"idx_snapshot_origin_sandbox",
				"idx_snapshot_origin_node",
				"idx_template_storage_backend",
				"idx_template_definition_rootfs_artifact",
			},
		},
		{
			table:   "t_cube_template_image_job",
			columns: []string{"sandbox_id", "resource_type", "resource_id", "pull_total_bytes", "pull_speed_bps"},
			indexes: []string{
				"idx_template_image_sandbox_status",
				"idx_template_image_resource_status",
				"idx_template_image_request_operation",
			},
		},
		{
			table: "t_cube_template_replica",
			absent: []string{
				"snapshot_path", "rootfs_vol", "memory_vol",
			},
			columns: []string{"guest_image_version", "compat_status"},
		},
		{
			table:   "t_cube_sandbox_spec",
			columns: []string{"sandbox_id", "request_json", "backfilled"},
		},
		{
			table:   "t_cube_snapshot_runtime_ref",
			columns: []string{"snapshot_id", "binding_type", "sandbox_gen"},
		},
		{
			table:   "t_cube_node_component_version",
			columns: []string{"node_id", "component", "version"},
		},
		{
			table:   "t_agenthub_instance",
			columns: []string{"agent_id", "persistence_mode", "rootfs_source_type"},
		},
		{
			table:   "t_cube_artifact_node_placement",
			columns: []string{"artifact_id", "node_id", "node_ip"},
		},
		{
			table: "t_cube_snapshot_runtime_active",
			columns: []string{
				"sandbox_id", "binding_type", "snapshot_id",
				"node_id", "node_ip", "memory_vol", "rootfs_vol", "sandbox_gen",
			},
			indexes: []string{
				"idx_snapshot_runtime_active_snapshot",
				"idx_snapshot_runtime_active_node",
				"idx_snapshot_runtime_active_node_ip",
			},
		},
	}
	for _, c := range cases {
		cols := pgTableColumns(ctx, t, db, c.table)
		for _, want := range c.columns {
			if !cols[want] {
				t.Errorf("%s: missing column %q (have: %s)", c.table, want, strings.Join(pgSortedKeys(cols), ","))
			}
		}
		for _, gone := range c.absent {
			if cols[gone] {
				t.Errorf("%s: deprecated column %q still exists", c.table, gone)
			}
		}
		idx := pgTableIndexes(ctx, t, db, c.table)
		for _, want := range c.indexes {
			if !idx[want] {
				t.Errorf("%s: missing index %q (have: %s)", c.table, want, strings.Join(pgSortedKeys(idx), ","))
			}
		}
	}
}

func pgTableColumns(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("select columns for %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[name] = true
	}
	if len(out) == 0 {
		t.Errorf("table %q has no columns (does it exist?)", table)
	}
	return out
}

func pgTableIndexes(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT indexname FROM pg_indexes
		  WHERE schemaname = current_schema() AND tablename = $1`, table)
	if err != nil {
		t.Fatalf("select indexes for %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		out[name] = true
	}
	return out
}

func pgSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestPostgres_FingerprintDetectsContentDrift proves the content-fingerprint
// defence works on PostgreSQL: after a clean migrate, tampering with a recorded
// fingerprint makes the next Run fail loudly.
func TestPostgres_FingerprintDetectsContentDrift(t *testing.T) {
	env := newPostgres(t)
	defer env.teardown()
	db := openPGDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("initial migrate.Run: %v", err)
	}

	// Fingerprint table must have been populated.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_cubemaster_migration_fingerprint`).Scan(&n); err != nil {
		t.Fatalf("count fingerprints: %v", err)
	}
	if n == 0 {
		t.Fatal("expected fingerprints to be recorded after fresh migrate")
	}

	// Corrupt a stored fingerprint to simulate content drift.
	res, err := db.ExecContext(ctx,
		`UPDATE t_cubemaster_migration_fingerprint SET sha256 = $1 WHERE version = 2`,
		"0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("corrupt fingerprint: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if affected != 1 {
		t.Fatalf("expected to corrupt exactly 1 fingerprint row, got %d", affected)
	}

	// Next Run must fail loudly.
	err = migrate.Run(ctx, db, "postgres", pgTestSessionLocker())
	if err == nil {
		t.Fatal("expected fingerprint mismatch error, got nil")
	}
	if !errors.Is(err, migrate.ErrFingerprintMismatch) {
		t.Fatalf("expected ErrFingerprintMismatch, got: %v", err)
	}

	// The escape hatch lets an operator bypass the check.
	t.Setenv("CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK", "1")
	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("migrate.Run with skip env should succeed: %v", err)
	}
}

// TestPostgres_Upgrade_RuntimeActiveBinding proves the 20260701040100
// migration's data-normalize + backfill path on PostgreSQL:
//  1. migrate to HEAD then roll back just before the active-binding migration
//  2. seed ACTIVE runtime_ref rows with empty binding_type and duplicates
//  3. re-run migrate and assert dedup + projection into runtime_active
//  4. re-run again to prove the migration is idempotent
func TestPostgres_Upgrade_RuntimeActiveBinding(t *testing.T) {
	env := newPostgres(t)
	defer env.teardown()
	db := openPGDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("initial migrate.Run: %v", err)
	}

	// Roll back to the migration immediately before runtime_active so we can
	// seed historical ACTIVE rows and exercise the Up path with real data.
	// 20260624121500 = template_definition_rootfs_artifact_id (last before
	// 20260701040100_snapshot_runtime_active_binding).
	const priorVersion int64 = 20260624121500
	if err := migrate.DownTo(ctx, db, "postgres", pgTestSessionLocker(), priorVersion); err != nil {
		t.Fatalf("DownTo(%d): %v", priorVersion, err)
	}

	// Confirm the active table was dropped by Down.
	var activeExists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM information_schema.tables
		    WHERE table_schema = current_schema()
		      AND table_name = 't_cube_snapshot_runtime_active'
		 )`).Scan(&activeExists); err != nil {
		t.Fatalf("check active table: %v", err)
	}
	if activeExists {
		t.Fatal("expected t_cube_snapshot_runtime_active to be dropped after DownTo")
	}

	// Seed: two ACTIVE rows sharing (sandbox_id='', binding_type='') that will
	// both normalize to (sb-dup, memory_backing); one already-correct ACTIVE;
	// one empty-binding singleton. Distinct snapshot_ids so we can assert which
	// duplicate survived (MAX(id) wins).
	if _, err := db.ExecContext(ctx, `INSERT INTO t_cube_snapshot_runtime_ref
		(snapshot_id, sandbox_id, node_id, binding_type, status, sandbox_gen)
		VALUES
		  ('snap-dup-old', 'sb-dup', 'node-1', '',              'ACTIVE', 1),
		  ('snap-dup-new', 'sb-dup', 'node-1', '',              'ACTIVE', 2),
		  ('snap-ok',      'sb-ok',  'node-2', 'memory_backing','ACTIVE', 1),
		  ('snap-empty',   'sb-e',   'node-3', '',              'ACTIVE', 1)`); err != nil {
		t.Fatalf("seed ACTIVE runtime_ref rows: %v", err)
	}

	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("migrate.Run after seed: %v", err)
	}

	// Dedup: exactly one ACTIVE left for sb-dup, and it must be snap-dup-new
	// (higher id). The older duplicate must be RELEASED.
	var dupActiveSnap, dupActiveStatus string
	if err := db.QueryRowContext(ctx,
		`SELECT snapshot_id, status FROM t_cube_snapshot_runtime_ref
		  WHERE sandbox_id = 'sb-dup' AND snapshot_id = 'snap-dup-new'`).
		Scan(&dupActiveSnap, &dupActiveStatus); err != nil {
		t.Fatalf("query snap-dup-new: %v", err)
	}
	if dupActiveStatus != "ACTIVE" {
		t.Errorf("snap-dup-new status=%q, want ACTIVE", dupActiveStatus)
	}
	var dupOldStatus, dupOldErr string
	if err := db.QueryRowContext(ctx,
		`SELECT status, COALESCE(last_error, '') FROM t_cube_snapshot_runtime_ref
		  WHERE sandbox_id = 'sb-dup' AND snapshot_id = 'snap-dup-old'`).
		Scan(&dupOldStatus, &dupOldErr); err != nil {
		t.Fatalf("query snap-dup-old: %v", err)
	}
	if dupOldStatus != "RELEASED" {
		t.Errorf("snap-dup-old status=%q, want RELEASED", dupOldStatus)
	}
	if !strings.Contains(dupOldErr, "deduplicated") {
		t.Errorf("snap-dup-old last_error=%q, want deduplicated marker", dupOldErr)
	}

	// Active projection: three rows (sb-dup, sb-ok, sb-e), binding_type filled.
	type activeRow struct {
		sandboxID, bindingType, snapshotID string
	}
	rows, err := db.QueryContext(ctx,
		`SELECT sandbox_id, binding_type, snapshot_id
		   FROM t_cube_snapshot_runtime_active
		  ORDER BY sandbox_id`)
	if err != nil {
		t.Fatalf("select runtime_active: %v", err)
	}
	defer rows.Close()
	var got []activeRow
	for rows.Next() {
		var r activeRow
		if err := rows.Scan(&r.sandboxID, &r.bindingType, &r.snapshotID); err != nil {
			t.Fatalf("scan active: %v", err)
		}
		got = append(got, r)
	}
	want := []activeRow{
		{"sb-dup", "memory_backing", "snap-dup-new"},
		{"sb-e", "memory_backing", "snap-empty"},
		{"sb-ok", "memory_backing", "snap-ok"},
	}
	if len(got) != len(want) {
		t.Fatalf("runtime_active row count=%d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("runtime_active[%d]=%v, want %v", i, got[i], want[i])
		}
	}

	assertPGHeadSchema(t, db)

	// Idempotent re-run must not fail or duplicate.
	if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	var activeCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_cube_snapshot_runtime_active`).Scan(&activeCount); err != nil {
		t.Fatalf("count active after re-run: %v", err)
	}
	if activeCount != 3 {
		t.Errorf("after idempotent re-run: active count=%d, want 3", activeCount)
	}
}
