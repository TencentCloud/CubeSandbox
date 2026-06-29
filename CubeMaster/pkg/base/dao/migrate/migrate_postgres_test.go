// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/migrate"
)

const (
	pgDsnEnv       = "CUBEMASTER_DAO_TEST_POSTGRES_DSN"
	pgImage        = "postgres"
	pgImageTag     = "16-alpine"
	pgProbeTimeout = 90 * time.Second
)

type pgTestEnv struct {
	dsn        string
	teardown   func()
	usesDocker bool
}

func newPostgres(t *testing.T) *pgTestEnv {
	t.Helper()
	if dsn := os.Getenv(pgDsnEnv); dsn != "" {
		t.Logf("using external PostgreSQL from %s", pgDsnEnv)
		return &pgTestEnv{dsn: dsn, teardown: func() {}, usesDocker: false}
	}
	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Skipf("dockertest not available (%v); set %s to run this test", err, pgDsnEnv)
	}
	if err := pool.Client.Ping(); err != nil {
		t.Skipf("docker daemon not reachable (%v); set %s to run this test", err, pgDsnEnv)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: pgImage,
		Tag:        pgImageTag,
		Env: []string{
			"POSTGRES_USER=cube",
			"POSTGRES_PASSWORD=cube_pass",
			"POSTGRES_DB=cube_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Skipf("could not start postgres container (%v); set %s to skip docker", err, pgDsnEnv)
	}
	port := resource.GetPort("5432/tcp")
	dsn := fmt.Sprintf(
		"host=127.0.0.1 port=%s user=cube password=cube_pass dbname=cube_test sslmode=disable",
		port,
	)

	pool.MaxWait = pgProbeTimeout
	if err := pool.Retry(func() error {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("postgres container never became reachable: %v", err)
	}

	return &pgTestEnv{
		dsn:        dsn,
		usesDocker: true,
		teardown: func() {
			_ = pool.Purge(resource)
		},
	}
}

func openPGDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping: %v", err)
	}
	return db
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
