// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	_ "github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao/driver/postgres"
)

func TestSnapshotReadsPostgreSQLAddColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Docker-backed PostgreSQL test in short mode")
	}
	// This regression requires a disposable Docker database for dao.Open.
	// CUBEMASTER_DAO_TEST_POSTGRES_DSN applies to the other PostgreSQL fixtures.
	env, cfg := startPGDockerContainer(t)
	t.Cleanup(env.teardown)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := dao.Open(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, dao.Close()) })
	require.NoError(t, dao.Migrate(ctx))
	pool := dao.SQL()
	pool.SetMaxOpenConns(2)

	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })

	// Keeping the DDL connection checked out leaves only one connection for the reader.
	ddl, err := pool.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ddl.Close() })

	schemaName := fmt.Sprintf("test_snapshot_add_column_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, err := ddl.ExecContext(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
		assert.NoError(t, err)
	})
	_, err = ddl.ExecContext(ctx, "CREATE SCHEMA "+schemaName)
	require.NoError(t, err)
	tableName := schemaName + ".t_cube_snapshot"
	_, err = ddl.ExecContext(ctx, "CREATE TABLE "+tableName+" (LIKE t_cube_snapshot)")
	require.NoError(t, err)
	_, err = pool.ExecContext(ctx, "SET search_path TO "+schemaName)
	require.NoError(t, err)

	var readerPID, ddlPID int
	require.NoError(t, pool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&readerPID))
	require.NoError(t, ddl.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&ddlPID))
	require.NotEqual(t, readerPID, ddlPID)

	want := models.SnapshotRecord{SnapshotID: "snap-test-add-column", Status: StatusReady, Mode: "full"}
	want.ID = 1
	require.NoError(t, db.WithContext(ctx).Create(&want).Error)
	get, err := getSnapshotRecord(ctx, want.SnapshotID)
	require.NoError(t, err)
	require.Equal(t, want.Status, get.Status)
	require.Equal(t, want.Mode, get.Mode)
	list, err := listSnapshotRecords(ctx)
	require.NoError(t, err)
	require.Contains(t, list, *get)

	_, err = ddl.ExecContext(ctx, "ALTER TABLE "+tableName+" ADD COLUMN test_snapshot_extra_column text")
	require.NoError(t, err)

	getAfter, err := getSnapshotRecord(ctx, want.SnapshotID)
	if assert.NoError(t, err, "first get after ALTER TABLE") {
		assert.Equal(t, get, getAfter)
	}
	listAfter, err := listSnapshotRecords(ctx)
	if assert.NoError(t, err, "first list after ALTER TABLE") {
		assert.Contains(t, listAfter, *get)
	}
	var finalPID int
	require.NoError(t, pool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&finalPID))
	require.Equal(t, readerPID, finalPID)
}
