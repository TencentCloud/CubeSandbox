// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
)

// Historical t_component_warehouse from 20260813120000 before that file was
// rewritten in-place from rel_path to object_key (see 49379cf8).
const legacyWarehouseMySQL = `CREATE TABLE IF NOT EXISTS t_component_warehouse (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  arch varchar(16) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL,
  source varchar(32) NOT NULL DEFAULT '',
  source_ref varchar(256) NOT NULL DEFAULT '',
  rel_path varchar(512) NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  checksum varchar(128) NOT NULL DEFAULT '',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_wh_arch_comp_ver (arch, component, version),
  KEY idx_wh_component (component, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

const legacyWarehousePostgres = `CREATE TABLE IF NOT EXISTS t_component_warehouse (
  id bigserial NOT NULL,
  arch varchar(16) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL,
  source varchar(32) NOT NULL DEFAULT '',
  source_ref varchar(256) NOT NULL DEFAULT '',
  rel_path varchar(512) NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  checksum varchar(128) NOT NULL DEFAULT '',
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT uk_wh_arch_comp_ver UNIQUE (arch, component, version)
)`

// Current 20260813120000 CREATE TABLE IF NOT EXISTS body. Rewriting this
// file does not alter an existing rel_path table.
const headWarehouseCreateMySQL = `CREATE TABLE IF NOT EXISTS t_component_warehouse (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  arch varchar(16) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL,
  source varchar(32) NOT NULL DEFAULT '',
  source_ref varchar(256) NOT NULL DEFAULT '',
  object_key varchar(512) NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  checksum varchar(128) NOT NULL DEFAULT '',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_wh_arch_comp_ver (arch, component, version),
  KEY idx_wh_component (component, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

const headWarehouseCreatePostgres = `CREATE TABLE IF NOT EXISTS t_component_warehouse (
  id bigserial NOT NULL,
  arch varchar(16) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL,
  source varchar(32) NOT NULL DEFAULT '',
  source_ref varchar(256) NOT NULL DEFAULT '',
  object_key varchar(512) NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  checksum varchar(128) NOT NULL DEFAULT '',
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT uk_wh_arch_comp_ver UNIQUE (arch, component, version)
)`

const legacyWarehouseRelPath = "blobs/amd64/cube-shim/v0.6.0/component.tar.gz"

const seedLegacyWarehouseMySQL = `INSERT INTO t_component_warehouse
	(arch, component, version, source, source_ref, rel_path, size_bytes, checksum)
	VALUES ('amd64', 'cube-shim', 'v0.6.0', 'github', 'TencentCloud/CubeSandbox', ?, 100, 'sha256:a')`

const seedLegacyWarehousePostgres = `INSERT INTO t_component_warehouse
	(arch, component, version, source, source_ref, rel_path, size_bytes, checksum)
	VALUES ('amd64', 'cube-shim', 'v0.6.0', 'github', 'TencentCloud/CubeSandbox', $1, 100, 'sha256:a')`

// TestWarehouseCreateIfNotExistsDoesNotRewriteRelPath is the negative gate:
// executing today's 20260813 CREATE TABLE IF NOT EXISTS against a leftover
// rel_path table must leave the old columns in place (MySQL 1054 / PG 42703).
func TestWarehouseCreateIfNotExistsDoesNotRewriteRelPath(t *testing.T) {
	t.Run("mysql", func(t *testing.T) {
		env := newMySQL(t)
		defer env.teardown()
		db := openDB(t, env.dsn)
		defer db.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		mustExec(t, ctx, db, "create legacy warehouse", legacyWarehouseMySQL)
		mustExec(t, ctx, db, "head CREATE IF NOT EXISTS", headWarehouseCreateMySQL)
		assertWarehouseStillLegacy(t, tableColumns(ctx, t, db, "t_component_warehouse"))
		assertObjectKeySelectFails(t, ctx, db)
	})
	t.Run("postgres", func(t *testing.T) {
		env := newPostgres(t)
		defer env.teardown()
		db := openPGDB(t, env.dsn)
		defer db.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		mustExec(t, ctx, db, "create legacy warehouse", legacyWarehousePostgres)
		mustExec(t, ctx, db, "head CREATE IF NOT EXISTS", headWarehouseCreatePostgres)
		assertWarehouseStillLegacy(t, pgTableColumns(ctx, t, db, "t_component_warehouse"))
		assertObjectKeySelectFails(t, ctx, db)
	})
}

// TestRun_UpgradeWarehouseRelPath is the existing-data path: leftover
// rel_path rows must survive migrate.Run as object_key, and HEAD schema
// must no longer expose rel_path.
func TestRun_UpgradeWarehouseRelPath(t *testing.T) {
	t.Run("mysql", func(t *testing.T) {
		env := newMySQL(t)
		defer env.teardown()
		db := openDB(t, env.dsn)
		defer db.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		mustExec(t, ctx, db, "create legacy warehouse", legacyWarehouseMySQL)
		mustExec(t, ctx, db, "seed rel_path row", seedLegacyWarehouseMySQL, legacyWarehouseRelPath)
		if err := migrate.Run(ctx, db, "mysql", testSessionLocker()); err != nil {
			t.Fatalf("migrate.Run: %v", err)
		}
		assertWarehouseRelPathCopied(t, ctx, db)
		assertHeadSchema(t, db)
	})
	t.Run("postgres", func(t *testing.T) {
		env := newPostgres(t)
		defer env.teardown()
		db := openPGDB(t, env.dsn)
		defer db.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		mustExec(t, ctx, db, "create legacy warehouse", legacyWarehousePostgres)
		mustExec(t, ctx, db, "seed rel_path row", seedLegacyWarehousePostgres, legacyWarehouseRelPath)
		if err := migrate.Run(ctx, db, "postgres", pgTestSessionLocker()); err != nil {
			t.Fatalf("migrate.Run: %v", err)
		}
		assertWarehouseRelPathCopied(t, ctx, db)
		assertPGHeadSchema(t, db)
	})
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, what, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func assertWarehouseStillLegacy(t *testing.T, cols map[string]bool) {
	t.Helper()
	if !cols["rel_path"] {
		t.Fatalf("expected leftover rel_path column, have %v", sortedKeys(cols))
	}
	if cols["object_key"] {
		t.Fatalf("CREATE IF NOT EXISTS must not add object_key to an existing table")
	}
}

func assertObjectKeySelectFails(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var dummy string
	err := db.QueryRowContext(ctx, `SELECT object_key FROM t_component_warehouse LIMIT 1`).Scan(&dummy)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected missing-column error, got %v", err)
	}
	var myerr *mysql.MySQLError
	var pgerr *pgconn.PgError
	switch {
	case errors.As(err, &myerr) && myerr.Number == 1054:
		return
	case errors.As(err, &pgerr) && pgerr.Code == "42703":
		return
	default:
		t.Fatalf("expected MySQL 1054 or PostgreSQL 42703, got %v", err)
	}
}

func assertWarehouseRelPathCopied(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var key string
	q := `SELECT object_key FROM t_component_warehouse WHERE arch = 'amd64' AND component = 'cube-shim' AND version = 'v0.6.0'`
	if err := db.QueryRowContext(ctx, q).Scan(&key); err != nil {
		t.Fatalf("select upgraded object_key: %v", err)
	}
	if key != legacyWarehouseRelPath {
		t.Fatalf("object_key = %q, want copied rel_path %q", key, legacyWarehouseRelPath)
	}
	var dummy string
	err := db.QueryRowContext(ctx, `SELECT rel_path FROM t_component_warehouse LIMIT 1`).Scan(&dummy)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rel_path must be dropped after upgrade, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "rel_path") {
		t.Fatalf("expected missing rel_path error, got %v", err)
	}
}
