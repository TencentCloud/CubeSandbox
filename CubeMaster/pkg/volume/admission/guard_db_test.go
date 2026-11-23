//go:build integration

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestVolumeAdmissionRealDatabases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		open  func(string) gorm.Dialector
		count string
	}{
		{"mysql", "CUBEMASTER_DAO_TEST_MYSQL_DSN", mysql.Open, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()"},
		{"postgres", "CUBEMASTER_DAO_TEST_POSTGRES_DSN", postgres.Open, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			require.NotEmpty(t, dsn, "dedicated empty SQL fixture required")
			db, err := gorm.Open(tc.open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.True(t, err == nil, "open dedicated SQL fixture failed")
			raw, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = raw.Close() })
			raw.SetMaxOpenConns(4)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			var tables int
			require.NoError(t, db.WithContext(ctx).Raw(tc.count).Scan(&tables).Error)
			require.Zero(t, tables, "refusing a nonempty database")
			require.NoError(t, migrate.Run(ctx, raw, tc.name, nil))

			for _, winner := range []string{"create", "delete"} {
				t.Run(winner+"_locks_first", func(t *testing.T) {
					testVolumeRowContention(t, ctx, db, winner)
				})
			}
			t.Run("confirmed_rollback_releases_last_node_reference", func(t *testing.T) {
				row := models.VolumeRecord{VolumeID: "shared", Name: "shared", Driver: "fixture"}
				require.NoError(t, db.Create(&row).Error)
				failed, _, err := BeginCreate(ctx, db, []string{row.VolumeID})
				require.NoError(t, err)
				success, _, err := BeginCreate(ctx, db, []string{row.VolumeID})
				require.NoError(t, err)
				require.NoError(t, success.Finish(ctx, map[string][]byte{
					refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":1}]`),
				}, false))
				// The successful sandbox left first, while the failed Create
				// still held the last local reference.
				_, err = BeginDelete(ctx, db, row.ID)
				require.ErrorIs(t, err, ErrBusy)
				require.NoError(t, failed.Finish(ctx, map[string][]byte{
					refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":0}]`),
				}, true))
				settled, err := BeginDelete(ctx, db, row.ID)
				require.NoError(t, err)
				require.Zero(t, settled.RefCount)
				require.Zero(t, settled.InFlightCreates)
			})
			t.Run("rollback_arrives_before_claimed_create_report", func(t *testing.T) {
				testRollbackBeforeCreateReport(t, db)
			})
		})
	}
}

func testVolumeRowContention(t *testing.T, parent context.Context, db *gorm.DB, winner string) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	row := models.VolumeRecord{VolumeID: "first-" + winner, Name: "first-" + winner, Driver: "fixture"}
	require.NoError(t, db.Create(&row).Error)
	type marker struct{}
	firstWrite, unblock, otherQuery := make(chan struct{}), make(chan struct{}), make(chan struct{})
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("pause_winner", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(marker{}) == "winner" && tx.Error == nil && tx.RowsAffected == 1 {
			close(firstWrite)
			select {
			case <-unblock:
			case <-ctx.Done():
				tx.AddError(ctx.Err())
			}
		}
	}))
	defer db.Callback().Update().Remove("pause_winner")
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("observe_loser", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(marker{}) == "loser" {
			close(otherQuery)
		}
	}))
	defer db.Callback().Query().Remove("observe_loser")

	create := func(ctx context.Context) error {
		_, _, err := BeginCreate(ctx, db, []string{row.VolumeID})
		return err
	}
	del := func(ctx context.Context) error {
		_, err := BeginDelete(ctx, db, row.ID)
		return err
	}
	first, other := create, del
	if winner == "delete" {
		first, other = del, create
	}
	firstResult, otherResult := make(chan error, 1), make(chan error, 1)
	go func() { firstResult <- first(context.WithValue(ctx, marker{}, "winner")) }()
	select {
	case <-firstWrite:
	case err := <-firstResult:
		t.Fatalf("winner returned before its successful SQL write: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { otherResult <- other(context.WithValue(ctx, marker{}, "loser")) }()
	select {
	case <-otherQuery:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-otherResult:
		close(unblock)
		<-firstResult
		t.Fatalf("second transaction escaped the uncommitted row lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(unblock)
	require.NoError(t, <-firstResult)
	require.ErrorIs(t, <-otherResult, ErrBusy)
	var after models.VolumeRecord
	require.NoError(t, db.Take(&after, row.ID).Error)
	require.Zero(t, after.RefCount)
	if winner == "create" {
		require.EqualValues(t, 1, after.InFlightCreates)
		require.False(t, after.Deleting)
	} else {
		require.Zero(t, after.InFlightCreates)
		require.True(t, after.Deleting)
	}
}
