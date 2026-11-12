// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxspec

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSpecStore(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "spec.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&models.SandboxSpec{}))
	require.NoError(t, database.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_sandbox_spec_sandbox_id ON t_cube_sandbox_spec (sandbox_id)").Error)

	dbMu.Lock()
	orig := db
	db = database
	dbMu.Unlock()
	t.Cleanup(func() {
		dbMu.Lock()
		db = orig
		dbMu.Unlock()
	})
	return database
}

func putSpec(t *testing.T, id, label string) {
	t.Helper()
	require.NoError(t, Put(context.Background(), id, &sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Labels:       map[string]string{"app": label},
	}, PutOptions{}))
}

func TestGetManyReturnsExistingAndSkipsMissing(t *testing.T) {
	setupSpecStore(t)
	putSpec(t, "sb-a", "a")
	putSpec(t, "sb-b", "b")

	got, err := GetMany(context.Background(), []string{"sb-a", "sb-missing", "sb-b"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "a", got["sb-a"].Labels["app"])
	require.Equal(t, "b", got["sb-b"].Labels["app"])

	one, err := Get(context.Background(), "sb-a")
	require.NoError(t, err)
	require.Equal(t, got["sb-a"].Labels, one.Labels)
}

func TestGetManyBatches(t *testing.T) {
	setupSpecStore(t)
	previous := getManyBatchSize
	getManyBatchSize = 2
	t.Cleanup(func() { getManyBatchSize = previous })

	var ids []string
	for i := 0; i < 5; i++ {
		id := "sb-batch-" + string(rune('a'+i))
		ids = append(ids, id)
		putSpec(t, id, id)
	}
	got, err := GetMany(context.Background(), ids)
	require.NoError(t, err)
	require.Len(t, got, 5)
	for _, id := range ids {
		require.Equal(t, id, got[id].Labels["app"])
	}
}

func TestGetManySkipsUndecodableRow(t *testing.T) {
	db := setupSpecStore(t)
	putSpec(t, "sb-good", "good")
	require.NoError(t, db.Table(constants.SandboxSpecTableName).Create(&models.SandboxSpec{
		SandboxID:   "sb-bad",
		RequestJSON: "{",
	}).Error)

	got, err := GetMany(context.Background(), []string{"sb-good", "sb-bad"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "good", got["sb-good"].Labels["app"])
}

func TestGetManyEmptyInput(t *testing.T) {
	setupSpecStore(t)
	got, err := GetMany(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, got)

	got, err = GetMany(context.Background(), []string{"", "  "})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestGetManyDedupesIDs(t *testing.T) {
	setupSpecStore(t)
	putSpec(t, "sb-dup", "dup")
	got, err := GetMany(context.Background(), []string{" sb-dup ", "sb-dup"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "dup", got["sb-dup"].Labels["app"])
}

func TestGetManyNotReady(t *testing.T) {
	dbMu.Lock()
	orig := db
	db = nil
	dbMu.Unlock()
	t.Cleanup(func() {
		dbMu.Lock()
		db = orig
		dbMu.Unlock()
	})

	_, err := GetMany(context.Background(), []string{"sb"})
	require.ErrorIs(t, err, ErrSandboxSpecStoreNotReady)
}

func TestGetManyFillsBackendFromColumn(t *testing.T) {
	db := setupSpecStore(t)
	require.NoError(t, db.Table(constants.SandboxSpecTableName).Create(&models.SandboxSpec{
		SandboxID:   "sb-backend",
		RequestJSON: `{"instance_type":"cubebox","labels":{}}`,
		Backend:     constants.SnapshotBackendXFS,
	}).Error)

	got, err := GetMany(context.Background(), []string{"sb-backend"})
	require.NoError(t, err)
	require.Equal(t, constants.SnapshotBackendXFS, got["sb-backend"].Backend)

	one, err := Get(context.Background(), "sb-backend")
	require.NoError(t, err)
	require.Equal(t, constants.SnapshotBackendXFS, one.Backend)
}
