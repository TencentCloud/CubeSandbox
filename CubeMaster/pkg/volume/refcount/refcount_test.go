// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package refcount

import (
	"encoding/json"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func TestReleasedVolumeIDs(t *testing.T) {
	t.Parallel()

	require.Nil(t, ReleasedVolumeIDs(nil))
	require.Nil(t, ReleasedVolumeIDs(map[string][]byte{}))

	raw, err := json.Marshal([]Event{
		{VolumeID: "vol-a", Referenced: 0},
		{VolumeID: "vol-b", Referenced: 1},
		{VolumeID: "vol-a", Referenced: 0},
		{VolumeID: "", Referenced: 0},
	})
	require.NoError(t, err)
	got := ReleasedVolumeIDs(map[string][]byte{ExtInfoKey: raw})
	require.Equal(t, []string{"vol-a"}, got)

	require.Nil(t, ReleasedVolumeIDs(map[string][]byte{ExtInfoKey: []byte("{bad")}))
	require.Nil(t, ReleasedVolumeIDs(map[string][]byte{"other": raw}))
}

func TestApplyLastDetachBeforeCreateReport(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	ctx := t.Context()
	require.NoError(t, db.WithContext(ctx).AutoMigrate(&models.VolumeRecord{}))
	row := models.VolumeRecord{VolumeID: "reordered", Name: "reordered", InFlightCreates: 1}
	require.NoError(t, db.WithContext(ctx).Create(&row).Error)
	check := func(count int64) {
		t.Helper()
		require.NoError(t, db.WithContext(ctx).Take(&row, row.ID).Error)
		require.Equal(t, count, row.RefCount)
	}
	legacy := Event{VolumeID: row.VolumeID, Referenced: 0}
	reported := Event{VolumeID: row.VolumeID, Referenced: 0, CreateReportClaimed: true}
	apply(ctx, db, legacy)
	check(0)
	apply(ctx, db, reported)
	check(-1)
	apply(ctx, db, legacy)
	check(-1)
	apply(ctx, db, Event{VolumeID: row.VolumeID, Referenced: 1})
	check(0)
	require.NoError(t, db.WithContext(ctx).Model(&row).Update("in_flight_creates", 0).Error)
	apply(ctx, db, reported)
	check(0)
}
