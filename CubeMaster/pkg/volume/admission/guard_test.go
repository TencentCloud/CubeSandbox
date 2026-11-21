// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"gorm.io/gorm"
)

func TestCreateAndDeleteShareVolumeRow(t *testing.T) {
	ctx := t.Context()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "volumes.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VolumeRecord{}))
	row := models.VolumeRecord{VolumeID: "shared", Name: "shared"}
	require.NoError(t, db.Create(&row).Error)

	a, _, err := BeginCreate(ctx, db, []string{"shared"})
	require.NoError(t, err)
	b, _, err := BeginCreate(ctx, db, []string{"shared"})
	require.NoError(t, err)
	_, err = BeginDelete(ctx, db, row.ID)
	require.ErrorIs(t, err, ErrBusy)
	require.NoError(t, a.Finish(ctx, nil, false))
	require.Error(t, a.Finish(ctx, nil, false))
	_, err = BeginDelete(ctx, db, row.ID)
	require.ErrorIs(t, err, ErrBusy)
	require.NoError(t, b.Finish(ctx, map[string][]byte{
		refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":1}]`),
	}, false))
	_, err = BeginDelete(ctx, db, row.ID)
	require.ErrorIs(t, err, ErrBusy)
	require.NoError(t, db.Model(&row).UpdateColumn("refcount", 0).Error)
	_, err = BeginDelete(ctx, db, row.ID)
	require.NoError(t, err)
	_, _, err = BeginCreate(ctx, db, []string{"shared"})
	require.ErrorIs(t, err, ErrBusy)
}

func TestCreateHandoffRollbackLeavesBothVolumeGuards(t *testing.T) {
	ctx := t.Context()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "volumes.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VolumeRecord{}))
	volumes := []models.VolumeRecord{
		{VolumeID: "first", Name: "first"},
		{VolumeID: "second", Name: "second"},
	}
	require.NoError(t, db.Create(&volumes).Error)
	guard, _, err := BeginCreate(ctx, db, []string{"second", "first"})
	require.NoError(t, err)
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("fail_second_handoff", func(tx *gorm.DB) {
		if row, ok := tx.Statement.Model.(*models.VolumeRecord); ok && row.VolumeID == "second" {
			tx.AddError(errors.New("handoff rolled back"))
		}
	}))
	err = guard.Finish(ctx, map[string][]byte{
		refcount.ExtInfoKey: []byte(`[{"volume_id":"first","referenced":1}]`),
	}, false)
	require.ErrorContains(t, err, "handoff rolled back")
	require.NoError(t, db.Callback().Update().Remove("fail_second_handoff"))
	for _, initial := range volumes {
		var row models.VolumeRecord
		require.NoError(t, db.Take(&row, initial.ID).Error)
		require.EqualValues(t, 1, row.InFlightCreates)
		require.Zero(t, row.RefCount)
		_, err := BeginDelete(ctx, db, row.ID)
		require.ErrorIs(t, err, ErrBusy)
	}
}

func TestConfirmedPluginVolumeRollbackSettlesCreate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		refCount int64
		reports  map[string][]byte
		busy     bool
	}{
		{name: "unreported first mount"},
		{name: "last holder of reported mount", refCount: 1, reports: map[string][]byte{
			refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":0}]`),
		}},
		{name: "missing matching reference", reports: map[string][]byte{
			refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":0}]`),
		}, busy: true},
		{name: "claimed but no other in-flight create", reports: map[string][]byte{
			refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":0,"create_report_claimed":true}]`),
		}, busy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "volumes.db")), &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&models.VolumeRecord{}))
			row := models.VolumeRecord{VolumeID: "shared", Name: "shared", RefCount: tc.refCount}
			require.NoError(t, db.Create(&row).Error)
			guard, _, err := BeginCreate(ctx, db, []string{"shared"})
			require.NoError(t, err)

			err = guard.Finish(ctx, tc.reports, true)
			if tc.busy {
				require.ErrorIs(t, err, ErrBusy)
				_, err := BeginDelete(ctx, db, row.ID)
				require.ErrorIs(t, err, ErrBusy)
				return
			}
			require.NoError(t, err)
			deleted, err := BeginDelete(ctx, db, row.ID)
			require.NoError(t, err)
			require.Zero(t, deleted.RefCount)
			require.Zero(t, deleted.InFlightCreates)
		})
	}
}

func TestRollbackCanSettleBeforeClaimedCreateReport(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "volumes.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VolumeRecord{}))
	testRollbackBeforeCreateReport(t, db)
}

func testRollbackBeforeCreateReport(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := t.Context()
	record := models.VolumeRecord{VolumeID: "reordered", Name: "reordered", Driver: "fixture"}
	require.NoError(t, db.WithContext(ctx).Create(&record).Error)
	a, _, err := BeginCreate(ctx, db, []string{record.VolumeID})
	require.NoError(t, err)
	b, _, err := BeginCreate(ctx, db, []string{record.VolumeID})
	require.NoError(t, err)

	// The successful holder left before the failed holder's last detach.
	// The last detach receipt arrived at Master before the success receipt.
	early := map[string][]byte{refcount.ExtInfoKey: []byte(`[{"volume_id":"reordered","referenced":0,"create_report_claimed":true}]`)}
	require.NoError(t, a.Finish(ctx, early, true))
	require.NoError(t, db.WithContext(ctx).Take(&record, record.ID).Error)
	require.EqualValues(t, -1, record.RefCount)
	require.EqualValues(t, 1, record.InFlightCreates, "only the failed Create's reservation was released")
	_, err = BeginDelete(ctx, db, record.ID)
	require.ErrorIs(t, err, ErrBusy)

	plus := map[string][]byte{refcount.ExtInfoKey: []byte(`[{"volume_id":"reordered","referenced":1}]`)}
	require.NoError(t, b.Finish(ctx, plus, false))
	settled, err := BeginDelete(ctx, db, record.ID)
	require.NoError(t, err)
	require.Zero(t, settled.RefCount)
	require.Zero(t, settled.InFlightCreates)
}
