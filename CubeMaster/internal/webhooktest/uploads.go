// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhooktest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/remotestatus"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	snapshotv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/snapshot/v1"
	"gorm.io/gorm"
)

func SnapshotDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "snapshots.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.SnapshotRecord{}, &models.PauseSnapshotRecord{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return db
}

// ReconcileUpload exercises the real upload completion path (polling, not a
// callback). The caller has already executed the local lifecycle success.
func ReconcileUpload(t *testing.T, db *gorm.DB, table, snapshotID, terminal string, p *webhook.BufferedPublisher) {
	t.Helper()
	before := len(p.Events())
	require.Equal(t, 1, before, "local success must already have published")
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	queries := 0
	patches.ApplyFunc(cubelet.SnapshotStatus, func(context.Context, string, *snapshotv1.StatusRequest) (*snapshotv1.StatusResponse, error) {
		queries++
		return &snapshotv1.StatusResponse{State: terminal, RemoteReady: terminal == constants.RemoteStatusReady}, nil
	})
	remotestatus.ReconcileOnce(context.Background(), db)
	remotestatus.ReconcileOnce(context.Background(), db)
	require.Equal(t, 1, queries, "terminal rows must not be polled again")
	var row struct {
		Status       string
		RemoteStatus string
	}
	require.NoError(t, db.Table(table).Where("snapshot_id = ?", snapshotID).Scan(&row).Error)
	require.Equal(t, "READY", row.Status, "remote failure must not undo local success")
	require.Equal(t, terminal, row.RemoteStatus)
	require.Len(t, p.Events(), before, "upload result must neither retract nor repeat event")
}
