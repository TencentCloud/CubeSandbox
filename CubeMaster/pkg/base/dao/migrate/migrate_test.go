// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/migrate"
)

// dsnEnv lets a dev/CI machine point the test at an existing MySQL when
// Docker is not available. The test always wins over the env var when
// dockertest succeeds, since that produces a clean, scoped instance.
const (
	dsnEnv          = "CUBEMASTER_DAO_TEST_MYSQL_DSN"
	mysqlImage      = "mysql"
	mysqlImageTag   = "8.0"
	probeTimeout    = 90 * time.Second
	probeRetryEvery = 1 * time.Second
)

type testEnv struct {
	dsn        string
	teardown   func()
	usesDocker bool
}

// newMySQL spins up a throwaway MySQL via dockertest, or returns the DSN
// from $CUBEMASTER_DAO_TEST_MYSQL_DSN, or skips the test. Skipping is the
// correct CI behaviour when the runner has neither: a green-by-default
// "test never ran" beats a red "I couldn't find a database".
func newMySQL(t *testing.T) *testEnv {
	t.Helper()
	if dsn := os.Getenv(dsnEnv); dsn != "" {
		t.Logf("using external MySQL from %s", dsnEnv)
		return &testEnv{dsn: dsn, teardown: func() {}, usesDocker: false}
	}
	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Skipf("dockertest not available (%v); set %s to run this test", err, dsnEnv)
	}
	if err := pool.Client.Ping(); err != nil {
		t.Skipf("docker daemon not reachable (%v); set %s to run this test", err, dsnEnv)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: mysqlImage,
		Tag:        mysqlImageTag,
		Env: []string{
			"MYSQL_ROOT_PASSWORD=root",
			"MYSQL_DATABASE=cube_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Skipf("could not start mysql container (%v); set %s to skip docker", err, dsnEnv)
	}
	port := resource.GetPort("3306/tcp")
	// DSN parameters intentionally mirror pkg/base/dao/driver/mysql.buildDSN so
	// the test path exercises the same connection-level options as production
	// (notably: multiStatements is NOT enabled). If a future migration relies
	// on multi-statement Exec, fix the migration rather than papering over it
	// here.
	dsn := fmt.Sprintf(
		"root:root@tcp(127.0.0.1:%s)/cube_test?charset=utf8&parseTime=true&loc=Local&timeout=5s&readTimeout=5s&writeTimeout=5s",
		port,
	)

	pool.MaxWait = probeTimeout
	if err := pool.Retry(func() error {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("mysql container never became reachable: %v", err)
	}

	return &testEnv{
		dsn:        dsn,
		usesDocker: true,
		teardown: func() {
			_ = pool.Purge(resource)
		},
	}
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping: %v", err)
	}
	return db
}

// testSessionLocker stamps every Run with a clearly-named global lock so
// production and tests do not accidentally collide on the same name.
func testSessionLocker() lock.SessionLocker {
	return &mysqlTestLocker{name: "cubemaster_dao_migrate_test_global", timeout: 30}
}

type mysqlTestLocker struct {
	name    string
	timeout int
}

func (l *mysqlTestLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", l.name, l.timeout).Scan(&got); err != nil {
		return err
	}
	if !got.Valid || got.Int64 != 1 {
		return fmt.Errorf("failed to acquire test lock %q (got=%v valid=%v)", l.name, got.Int64, got.Valid)
	}
	return nil
}

func (l *mysqlTestLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "DO RELEASE_LOCK(?)", l.name)
	return err
}

// TestRun_Fresh validates the empty-database path: every migration runs
// from scratch and lands on the HEAD schema.
func TestRun_Fresh(t *testing.T) {
	env := newMySQL(t)
	defer env.teardown()
	db := openDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.Run(ctx, db, "mysql", testSessionLocker()); err != nil {
		t.Fatalf("migrate.Run (fresh): %v", err)
	}
	assertHeadSchema(t, db)
}

// TestRun_UpgradeFromV022 is the upgrade-path test: manually create
// the v0.2.2 baseline (no goose_db_version yet),
// seed legacy rows, then run migrate.Run. This proves the data normalize
// step is honoured before the UNIQUE (request_id, operation) index is
// added — exactly the path that breaks for real 0.2.2 users.
func TestRun_UpgradeFromV022(t *testing.T) {
	env := newMySQL(t)
	defer env.teardown()
	db := openDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Build the v0.2.2 schema by directly running the baseline file. We
	// extract a minimal subset that matters for the legacy rows: the
	// template_image_job table. (The rest of the baseline is exercised
	// by TestRun_Fresh.)
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS t_cube_template_image_job (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  job_id varchar(128) NOT NULL,
  template_id varchar(128) NOT NULL DEFAULT '',
  request_id varchar(128) NOT NULL DEFAULT '',
  attempt_no int NOT NULL DEFAULT 1,
  retry_of_job_id varchar(128) NOT NULL DEFAULT '',
  operation varchar(32) NOT NULL DEFAULT '',
  redo_mode varchar(32) NOT NULL DEFAULT '',
  redo_scope_json mediumtext,
  resume_phase varchar(64) NOT NULL DEFAULT '',
  node_id varchar(128) NOT NULL DEFAULT '',
  node_ip varchar(256) NOT NULL DEFAULT '',
  snapshot_path varchar(1024) NOT NULL DEFAULT '',
  artifact_id varchar(128) NOT NULL DEFAULT '',
  template_spec_fingerprint varchar(128) NOT NULL DEFAULT '',
  source_image_ref varchar(1024) NOT NULL DEFAULT '',
  source_image_digest varchar(256) NOT NULL DEFAULT '',
  writable_layer_size varchar(64) NOT NULL DEFAULT '',
  instance_type varchar(64) NOT NULL DEFAULT '',
  network_type varchar(64) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT '',
  phase varchar(64) NOT NULL DEFAULT '',
  progress int NOT NULL DEFAULT 0,
  error_message text,
  expected_node_count int NOT NULL DEFAULT 0,
  ready_node_count int NOT NULL DEFAULT 0,
  failed_node_count int NOT NULL DEFAULT 0,
  template_status varchar(32) NOT NULL DEFAULT '',
  artifact_status varchar(32) NOT NULL DEFAULT '',
  request_json mediumtext,
  result_json mediumtext,
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at datetime DEFAULT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY idx_template_image_job_id (job_id),
  UNIQUE KEY idx_template_image_template_attempt (template_id,attempt_no),
  KEY idx_template_image_request_id (request_id),
  KEY idx_template_image_status (status),
  KEY idx_template_image_template_status (template_id,status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		t.Fatalf("create v0.2.2 template_image_job: %v", err)
	}

	// Seed legacy rows: same empty request_id, different job_id. The Go-
	// side normalizer used to set request_id = 'legacy-' + job_id, which
	// is exactly what migration 0001 step (C) replicates in SQL.
	// Five legacy rows with empty request_id and operation, one per
	// branch of the CASE in 0002 step (C.2):
	//   - job-a / job-c : CREATE (source ref, no retry, no node)
	//   - job-b         : COMMIT (node, no source ref)
	//   - job-d         : REDO   (source ref AND retry_of_job_id set)
	//   - job-e         : LEGACY (everything empty -> fallback ELSE branch)
	// Distinct (template_id, attempt_no) so the existing v0.2.2 UNIQUE
	// index does not reject the seed inserts.
	if _, err := db.ExecContext(ctx, `INSERT INTO t_cube_template_image_job
		(job_id, template_id, attempt_no, source_image_ref, node_id, retry_of_job_id)
		VALUES
		  ('job-a', 'tpl-1', 1, 'registry.io/img@sha256:aaa', '',       ''),
		  ('job-b', 'tpl-1', 2, '',                          'node-1', ''),
		  ('job-c', 'tpl-2', 1, 'registry.io/img@sha256:aaa', '',       ''),
		  ('job-d', 'tpl-2', 2, 'registry.io/img@sha256:bbb', '',       'job-a'),
		  ('job-e', 'tpl-3', 1, '',                          '',       '')`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}

	// Now run the full migration suite (baseline 0000 will no-op on
	// the existing table thanks to IF NOT EXISTS, then 0001 will add
	// the new columns/indexes and run the normalize step).
	if err := migrate.Run(ctx, db, "mysql", testSessionLocker()); err != nil {
		t.Fatalf("migrate.Run (upgrade): %v", err)
	}

	// Assert the data normalize ran: each legacy row got a unique
	// request_id matching 'legacy-' + job_id.
	rows, err := db.QueryContext(ctx,
		`SELECT job_id, request_id, operation FROM t_cube_template_image_job ORDER BY job_id`)
	if err != nil {
		t.Fatalf("select normalized rows: %v", err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var jobID, requestID, op string
		if err := rows.Scan(&jobID, &requestID, &op); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[jobID] = [2]string{requestID, op}
	}
	want := map[string][2]string{
		"job-a": {"legacy-job-a", "CREATE"}, // source ref present, no retry
		"job-b": {"legacy-job-b", "COMMIT"}, // node id present, no source ref
		"job-c": {"legacy-job-c", "CREATE"},
		"job-d": {"legacy-job-d", "REDO"},   // source ref + retry_of_job_id
		"job-e": {"legacy-job-e", "LEGACY"}, // ELSE branch fallback
	}
	for jobID, expected := range want {
		actual, ok := got[jobID]
		if !ok {
			t.Errorf("missing row for %s", jobID)
			continue
		}
		if actual != expected {
			t.Errorf("%s: got %v, want %v", jobID, actual, expected)
		}
	}

	// Assert the UNIQUE (request_id, operation) index is now enforceable.
	if _, err := db.ExecContext(ctx, `INSERT INTO t_cube_template_image_job
		(job_id, template_id, request_id, operation, source_image_ref)
		VALUES ('dup-1', 'tpl-3', 'legacy-job-a', 'CREATE', 'x')`); err == nil {
		t.Errorf("expected UNIQUE (request_id, operation) violation, got nil")
	}

	assertHeadSchema(t, db)
}

func TestRun_RerunsAfterVersionMissingAtHeadSchema(t *testing.T) {
	env := newMySQL(t)
	defer env.teardown()
	db := openDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.Run(ctx, db, "mysql", testSessionLocker()); err != nil {
		t.Fatalf("initial migrate.Run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id = 2`); err != nil {
		t.Fatalf("delete version 2: %v", err)
	}
	if err := migrate.Run(ctx, db, "mysql", testSessionLocker()); err != nil {
		t.Fatalf("rerun migrate.Run at HEAD schema without version 2: %v", err)
	}
	assertHeadSchema(t, db)
}

func TestRun_ContinuesFromPartialV022ToHeadDDL(t *testing.T) {
	env := newMySQL(t)
	defer env.teardown()
	db := openDB(t, env.dsn)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.Run(ctx, db, "mysql", testSessionLocker()); err != nil {
		t.Fatalf("initial migrate.Run: %v", err)
	}
	if err := migrate.DownTo(ctx, db, "mysql", testSessionLocker(), 1); err != nil {
		t.Fatalf("migrate.DownTo(1): %v", err)
	}

	partialDDL := []string{
		`ALTER TABLE t_cube_template_image_job ADD COLUMN sandbox_id varchar(128) NOT NULL DEFAULT '' COMMENT 'sandbox id for snapshot operations' AFTER request_id`,
		`ALTER TABLE t_cube_template_image_job ADD INDEX idx_template_image_sandbox_status (sandbox_id, status)`,
		`ALTER TABLE t_cube_template_definition ADD COLUMN kind varchar(32) NOT NULL DEFAULT 'template' COMMENT 'template kind' AFTER status`,
		`ALTER TABLE t_cube_template_definition ADD INDEX idx_template_kind_status (kind, status)`,
		`ALTER TABLE t_cube_template_replica DROP COLUMN snapshot_path`,
		`CREATE TABLE IF NOT EXISTS t_cube_sandbox_spec (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			sandbox_id varchar(128) NOT NULL COMMENT 'sandbox id',
			template_id varchar(128) NOT NULL DEFAULT '' COMMENT 'base template id at create time',
			instance_type varchar(64) NOT NULL DEFAULT '' COMMENT 'instance type',
			network_type varchar(64) NOT NULL DEFAULT '' COMMENT 'network type',
			host_id varchar(128) NOT NULL DEFAULT '' COMMENT 'host id where sandbox runs',
			host_ip varchar(64) NOT NULL DEFAULT '' COMMENT 'host ip where sandbox runs',
			request_json mediumtext NOT NULL COMMENT 'canonical create request json',
			backfilled tinyint(1) NOT NULL DEFAULT 0 COMMENT 'whether spec was reconstructed from base template (override-lossy)',
			created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at datetime DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY idx_sandbox_spec_sandbox_id (sandbox_id),
			KEY idx_sandbox_spec_template_id (template_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
	}
	for _, stmt := range partialDDL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("apply partial DDL %q: %v", stmt, err)
		}
	}

	if err := migrate.Run(ctx, db, "mysql", testSessionLocker()); err != nil {
		t.Fatalf("migrate.Run from partial DDL: %v", err)
	}
	assertHeadSchema(t, db)
}

// assertHeadSchema verifies a set of HEAD-only signals: new tables exist,
// new columns exist, deprecated columns are gone, expected indexes are
// present. The intent is to catch drift between the SQL migrations and
// the Go models without coupling the test to every column name.
func assertHeadSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type expect struct {
		table   string
		columns []string // must all exist
		absent  []string // must NOT exist
		indexes []string // must all exist (by index name)
	}
	cases := []expect{
		{
			table: "t_cube_template_definition",
			columns: []string{
				"kind", "origin_sandbox_id", "origin_node_id",
				"display_name", "storage_backend", "retain",
				"rootfs_size_bytes_at_snapshot",
			},
			indexes: []string{
				"idx_template_kind_status",
				"idx_snapshot_origin_sandbox",
				"idx_snapshot_origin_node",
				"idx_template_storage_backend",
			},
		},
		{
			table:   "t_cube_template_image_job",
			columns: []string{"sandbox_id", "resource_type", "resource_id"},
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
				"rootfs_kind", "memory_kind", "rootfs_dev",
				"memory_dev", "meta_dir", "build_rootfs_vol",
			},
		},
		{
			table:   "t_cube_sandbox_spec",
			columns: []string{"sandbox_id", "request_json", "backfilled"},
		},
		{
			table:   "t_cube_snapshot_runtime_ref",
			columns: []string{"snapshot_id", "binding_type", "sandbox_gen"},
		},
	}
	for _, c := range cases {
		cols := tableColumns(ctx, t, db, c.table)
		for _, want := range c.columns {
			if !cols[want] {
				t.Errorf("%s: missing column %q (have: %s)", c.table, want, strings.Join(sortedKeys(cols), ","))
			}
		}
		for _, gone := range c.absent {
			if cols[gone] {
				t.Errorf("%s: deprecated column %q still exists", c.table, gone)
			}
		}
		idx := tableIndexes(ctx, t, db, c.table)
		for _, want := range c.indexes {
			if !idx[want] {
				t.Errorf("%s: missing index %q (have: %s)", c.table, want, strings.Join(sortedKeys(idx), ","))
			}
		}
	}
}

func tableColumns(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
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

func tableIndexes(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT INDEX_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
