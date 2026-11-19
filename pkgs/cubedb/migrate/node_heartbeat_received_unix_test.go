// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
)

func TestHeartbeatReceivedUnixUpgradeKeepsLegacyRowsUnknown(t *testing.T) {
	const previousVersion int64 = 20261112071208
	for _, tc := range []struct {
		name   string
		env    func(*testing.T) *dbTestEnv
		open   func(*testing.T, string) *sql.DB
		locker func() lock.SessionLocker
	}{
		{"mysql", newMySQL, openDB, testSessionLocker},
		{"postgres", newPostgres, openPGDB, pgTestSessionLocker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env(t)
			defer env.teardown()
			db := tc.open(t, env.dsn)
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if err := migrate.Run(ctx, db, tc.name, tc.locker()); err != nil {
				t.Fatalf("initial migrate: %v", err)
			}
			if err := migrate.DownTo(ctx, db, tc.name, tc.locker(), previousVersion); err != nil {
				t.Fatalf("roll back receive-time migration: %v", err)
			}
			insert := "INSERT INTO t_cube_node_status (node_id, heartbeat_unix, healthy) VALUES (?, ?, ?)"
			if tc.name == "postgres" {
				insert = "INSERT INTO t_cube_node_status (node_id, heartbeat_unix, healthy) VALUES ($1, $2, $3)"
			}
			if _, err := db.ExecContext(ctx, insert, "legacy-node", int64(123), true); err != nil {
				t.Fatalf("seed legacy status: %v", err)
			}
			for i := range 2 {
				if err := migrate.Run(ctx, db, tc.name, tc.locker()); err != nil {
					t.Fatalf("upgrade run %d: %v", i+1, err)
				}
			}
			var heartbeat, received int64
			query := "SELECT heartbeat_unix, heartbeat_received_unix FROM t_cube_node_status WHERE node_id = ?"
			if tc.name == "postgres" {
				query = "SELECT heartbeat_unix, heartbeat_received_unix FROM t_cube_node_status WHERE node_id = $1"
			}
			if err := db.QueryRowContext(ctx, query, "legacy-node").Scan(&heartbeat, &received); err != nil {
				t.Fatalf("read upgraded status: %v", err)
			}
			if heartbeat != 123 || received != 0 {
				t.Fatalf("legacy row changed unexpectedly: heartbeat=%d received=%d", heartbeat, received)
			}
		})
	}
}
