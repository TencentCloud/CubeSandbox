// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestReconcileExpiredSnapshotsSelectsExpiredReadyOrFailed pins the selection
// predicate: only READY/FAILED rows with a non-NULL expires_at at or before
// now may be retired. The repo has no in-memory driver, so the query runs on
// a DryRun gorm.DB and an after-query callback captures the exact WHERE
// clause (see TestGetTemplateByAliasFiltersByKindExcludesSnapshots).
func TestReconcileExpiredSnapshotsSelectsExpiredReadyOrFailed(t *testing.T) {
	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	require.NoError(t, err)
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)

	var capturedSQL string
	var capturedVars []any
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("capture_expiry_query", func(tx *gorm.DB) {
		capturedSQL = tx.Statement.SQL.String()
		capturedVars = append(capturedVars, tx.Statement.Vars...)
	}))

	old := store.db
	store.db = db
	t.Cleanup(func() { store.db = old })

	before := time.Now()
	require.NoError(t, reconcileExpiredSnapshots(context.Background()))
	after := time.Now()

	require.Contains(t, capturedSQL, "status IN")
	require.Contains(t, capturedSQL, "expires_at IS NOT NULL")
	require.Contains(t, capturedSQL, "expires_at <=")

	var statuses []string
	var expiryBound time.Time
	for _, v := range capturedVars {
		switch val := v.(type) {
		case string:
			statuses = append(statuses, val)
		case time.Time:
			expiryBound = val
		}
	}
	require.ElementsMatch(t, []string{StatusReady, StatusFailed}, statuses)
	require.False(t, expiryBound.Before(before), "expires_at cutoff %v predates call start %v", expiryBound, before)
	require.False(t, expiryBound.After(after), "expires_at cutoff %v postdates call end %v", expiryBound, after)
}

// TestReconcileExpiredSnapshotsExpiresEverySelectedRow verifies the loop hands
// each selected row to expireSnapshotRecord and joins per-row failures instead
// of aborting the pass.
func TestReconcileExpiredSnapshotsExpiresEverySelectedRow(t *testing.T) {
	rows := []models.SnapshotRecord{
		{SnapshotID: "snap-a", Status: StatusReady},
		{SnapshotID: "snap-b", Status: StatusFailed},
	}

	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	require.NoError(t, err)
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)
	require.NoError(t, db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		if dest, ok := tx.Statement.Dest.(*[]models.SnapshotRecord); ok {
			*dest = append([]models.SnapshotRecord(nil), rows...)
		}
	}))

	old := store.db
	store.db = db
	t.Cleanup(func() { store.db = old })

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	var expired []string
	patches.ApplyFunc(expireSnapshotRecord, func(_ context.Context, snapshotID string, _ time.Time) error {
		expired = append(expired, snapshotID)
		if snapshotID == "snap-a" {
			return errors.New("expire snap-a failed")
		}
		return nil
	})

	err = reconcileExpiredSnapshots(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "expire snap-a failed")
	require.ElementsMatch(t, []string{"snap-a", "snap-b"}, expired)
}

// TestExpireSnapshotRecordGuards covers the per-record early returns in
// expireSnapshotRecord: only READY/FAILED rows whose expires_at is set and at
// or before now are tombstoned.
func TestExpireSnapshotRecordGuards(t *testing.T) {
	now := time.Now()
	expiredAt := now.Add(-time.Minute)
	futureAt := now.Add(time.Hour)

	cases := []struct {
		name      string
		status    string
		expiresAt *time.Time
		tombstone bool
	}{
		{name: "expired READY is tombstoned", status: StatusReady, expiresAt: &expiredAt, tombstone: true},
		{name: "expired FAILED is tombstoned", status: StatusFailed, expiresAt: &expiredAt, tombstone: true},
		{name: "expired CREATING is untouched", status: StatusCreating, expiresAt: &expiredAt},
		{name: "expired DELETING is untouched", status: StatusDeleting, expiresAt: &expiredAt},
		{name: "expired DELETED is untouched", status: StatusDeleted, expiresAt: &expiredAt},
		{name: "READY without TTL is untouched", status: StatusReady, expiresAt: nil},
		{name: "READY with future expiry is untouched", status: StatusReady, expiresAt: &futureAt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := store.db
			store.db = &gorm.DB{}
			t.Cleanup(func() { store.db = old })

			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			patches.ApplyFunc(getSnapshotRecord, func(context.Context, string) (*models.SnapshotRecord, error) {
				return &models.SnapshotRecord{SnapshotID: "snap-1", Status: tc.status, ExpiresAt: tc.expiresAt}, nil
			})
			tombstoned := false
			patches.ApplyFunc(tombstoneSnapshotRecord, func(_ context.Context, snapshotID string) error {
				tombstoned = true
				return nil
			})

			require.NoError(t, expireSnapshotRecord(context.Background(), "snap-1", now))
			require.Equal(t, tc.tombstone, tombstoned)
		})
	}
}
