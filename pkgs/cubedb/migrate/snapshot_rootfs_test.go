// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package migrate_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
)

func TestSnapshotRootfsBackfillMySQL(t *testing.T) {
	env := newMySQL(t)
	defer env.teardown()
	db := openDB(t, env.dsn)
	defer db.Close()
	testSnapshotRootfsBackfill(t, db, "mysql", testSessionLocker())
}

func TestSnapshotRootfsBackfillPostgres(t *testing.T) {
	env := newPostgres(t)
	defer env.teardown()
	db := openPGDB(t, env.dsn)
	defer db.Close()
	testSnapshotRootfsBackfill(t, db, "postgres", pgTestSessionLocker())
}

func testSnapshotRootfsBackfill(t *testing.T, db *sql.DB, dialect string, locker lock.SessionLocker) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.Run(ctx, db, dialect, locker); err != nil {
		t.Fatal(err)
	}
	if err := migrate.DownTo(ctx, db, dialect, locker, 20260904120000); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ id, request, want string }{
		{"top", `{"annotations":{"cube.master.rootfs.artifact.id":" rfs-top "},"containers":[{"image":{"annotations":{"cube.master.rootfs.artifact.id":"rfs-other"}}}]}`, "rfs-top"},
		{"image", `{"containers":[null,{"image":{"annotations":{"cube.master.rootfs.artifact.id":" "}}},{"image":{"annotations":{"cube.master.rootfs.artifact.id":" rfs-image "}}}]}`, "rfs-image"},
		{"empty", "", ""},
		{"unrelated", `{"annotations":{"other":"rfs-unrelated"}}`, ""},
	}
	insert := "INSERT INTO t_cube_snapshot (snapshot_id, request_json) VALUES (?, ?)"
	query := "SELECT rootfs_artifact_id FROM t_cube_snapshot WHERE snapshot_id = ?"
	if dialect == "postgres" {
		insert = "INSERT INTO t_cube_snapshot (snapshot_id, request_json) VALUES ($1, $2)"
		query = "SELECT rootfs_artifact_id FROM t_cube_snapshot WHERE snapshot_id = $1"
	}
	for _, c := range cases {
		if _, err := db.ExecContext(ctx, insert, c.id, c.request); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate.Run(ctx, db, dialect, locker); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		var got string
		if err := db.QueryRowContext(ctx, query, c.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: reference = %q, want %q", c.id, got, c.want)
		}
	}
	if err := migrate.Run(ctx, db, dialect, locker); err != nil {
		t.Fatal(err)
	}
}
