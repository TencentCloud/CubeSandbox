// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// MySQL reports changed rows, so the 0-row probe read is only covered here.
func newReplicaWriteMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	db, _ := newArtifactGCMySQL(t)
	require.NoError(t, db.AutoMigrate(&models.TemplateReplica{}))
	// Production gets this unique index from the migrations; mirror it here.
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_template_node ON t_cube_template_replica (template_id(128), node_id(128))").Error)
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	return db
}

func readReplicaRowMySQL(t *testing.T, db *gorm.DB, templateID, nodeID string) models.TemplateReplica {
	t.Helper()
	var row models.TemplateReplica
	require.NoError(t, db.Table("t_cube_template_replica").
		Where("template_id = ? AND node_id = ?", templateID, nodeID).First(&row).Error)
	return row
}

func TestUpsertReplicaKeepingReadyMySQLUnchangedWriteStillApplies(t *testing.T) {
	db := newReplicaWriteMySQL(t)
	ctx := context.Background()
	stale := failedReplica("node-a", "job-1")

	require.NoError(t, upsertReplicaKeepingReady(ctx, "tpl-my", "cubebox", stale), "missing row must be inserted")

	// Identical values: 0 changed rows must not be mistaken for a kept row.
	require.NoError(t, upsertReplicaKeepingReady(ctx, "tpl-my", "cubebox", stale))
	require.Equal(t, ReplicaStatusFailed, readReplicaRowMySQL(t, db, "tpl-my", "node-a").Status)
}

func TestUpsertReplicaKeepingReadyMySQLKeepsCleanReady(t *testing.T) {
	db := newReplicaWriteMySQL(t)
	require.NoError(t, db.Table("t_cube_template_replica").Create(&models.TemplateReplica{
		TemplateID: "tpl-my-ready",
		NodeID:     "node-a",
		NodeIP:     "10.0.0.1",
		Status:     ReplicaStatusReady,
		Phase:      ReplicaPhaseReady,
		LastJobID:  "job-1",
	}).Error)

	require.NoError(t, upsertReplicaKeepingReady(context.Background(), "tpl-my-ready", "cubebox", failedReplica("node-a", "job-1")))
	require.Equal(t, ReplicaStatusReady, readReplicaRowMySQL(t, db, "tpl-my-ready", "node-a").Status)
}

// A soft-deleted row still holds the unique key: surface the duplicate-key error.
func TestUpsertReplicaKeepingReadyMySQLSoftDeletedRowSurfacesConflict(t *testing.T) {
	db := newReplicaWriteMySQL(t)
	ctx := context.Background()
	require.NoError(t, db.Table("t_cube_template_replica").Create(&models.TemplateReplica{
		TemplateID: "tpl-my-deleted",
		NodeID:     "node-a",
		Status:     ReplicaStatusFailed,
		Phase:      ReplicaPhaseFailed,
	}).Error)
	require.NoError(t, db.Table("t_cube_template_replica").
		Where("template_id = ? AND node_id = ?", "tpl-my-deleted", "node-a").
		Delete(&models.TemplateReplica{}).Error)

	err := upsertReplicaKeepingReady(ctx, "tpl-my-deleted", "cubebox", failedReplica("node-a", "job-1"))
	require.Error(t, err)
	require.True(t, isDuplicateKeyError(err), "expected the unique-key conflict, got %v", err)
}
