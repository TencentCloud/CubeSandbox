// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// A repairer may create and upgrade replica rows, never downgrade a clean
// READY one (see upsertReplicaKeepingReady).

func newReplicaWriteTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateReplica{}))
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	return db
}

func seedReplicaRow(t *testing.T, db *gorm.DB, templateID, nodeID, status, jobID string, cleanupRequired bool) {
	t.Helper()
	require.NoError(t, db.Table("t_cube_template_replica").Create(&models.TemplateReplica{
		TemplateID:      templateID,
		NodeID:          nodeID,
		NodeIP:          "10.0.0.1",
		Status:          status,
		Phase:           status,
		LastJobID:       jobID,
		CleanupRequired: cleanupRequired,
	}).Error)
}

func readReplicaRow(t *testing.T, db *gorm.DB, templateID, nodeID string) models.TemplateReplica {
	t.Helper()
	var row models.TemplateReplica
	require.NoError(t, db.Table("t_cube_template_replica").
		Where("template_id = ? AND node_id = ?", templateID, nodeID).First(&row).Error)
	return row
}

func failedReplica(nodeID, jobID string) ReplicaStatus {
	return ReplicaStatus{
		NodeID:          nodeID,
		NodeIP:          "10.0.0.1",
		Status:          ReplicaStatusFailed,
		Phase:           ReplicaPhaseFailed,
		LastJobID:       jobID,
		LastErrorPhase:  ReplicaPhaseSnapshotting,
		CleanupRequired: true,
		ErrorMessage:    "appsapshot already exists",
	}
}

// Job and sweep share last_job_id, so the guard cannot rely on comparing it.
func TestUpsertReplicaKeepingReadyKeepsCleanReadyRow(t *testing.T) {
	db := newReplicaWriteTestDB(t)
	seedReplicaRow(t, db, "tpl-keep", "node-a", ReplicaStatusReady, "job-1", false)

	require.NoError(t, upsertReplicaKeepingReady(context.Background(), "tpl-keep", "cubebox", failedReplica("node-a", "job-1")))

	row := readReplicaRow(t, db, "tpl-keep", "node-a")
	require.Equal(t, ReplicaStatusReady, row.Status)
	require.Empty(t, row.ErrorMessage)
}

// A cleanup_required row is half-deleted: repairing it is the sweep's job.
func TestUpsertReplicaKeepingReadyRepairsFlaggedReadyRow(t *testing.T) {
	db := newReplicaWriteTestDB(t)
	seedReplicaRow(t, db, "tpl-flag", "node-a", ReplicaStatusReady, "job-1", true)

	require.NoError(t, upsertReplicaKeepingReady(context.Background(), "tpl-flag", "cubebox", failedReplica("node-a", "job-2")))

	row := readReplicaRow(t, db, "tpl-flag", "node-a")
	require.Equal(t, ReplicaStatusFailed, row.Status)
	require.Equal(t, "job-2", row.LastJobID)
}

func TestUpsertReplicaKeepingReadyUpgradesFailedRow(t *testing.T) {
	db := newReplicaWriteTestDB(t)
	seedReplicaRow(t, db, "tpl-up", "node-a", ReplicaStatusFailed, "job-1", true)

	ready := ReplicaStatus{
		NodeID: "node-a",
		NodeIP: "10.0.0.1",
		Status: ReplicaStatusReady,
		Phase:  ReplicaPhaseReady,
	}
	require.NoError(t, upsertReplicaKeepingReady(context.Background(), "tpl-up", "cubebox", ready))

	row := readReplicaRow(t, db, "tpl-up", "node-a")
	require.Equal(t, ReplicaStatusReady, row.Status)
	require.False(t, row.CleanupRequired)
}

func TestUpsertReplicaKeepingReadyCreatesMissingRow(t *testing.T) {
	db := newReplicaWriteTestDB(t)

	require.NoError(t, upsertReplicaKeepingReady(context.Background(), "tpl-new", "cubebox", failedReplica("node-a", "job-1")))

	row := readReplicaRow(t, db, "tpl-new", "node-a")
	require.Equal(t, ReplicaStatusFailed, row.Status)
	require.Equal(t, "cubebox", row.InstanceType)
}

// The owner path stays unconditional: a failed job must publish its own FAILED.
func TestUpsertReplicaOwnerStillOverwritesReadyRow(t *testing.T) {
	db := newReplicaWriteTestDB(t)
	seedReplicaRow(t, db, "tpl-owner", "node-a", ReplicaStatusReady, "job-1", false)

	require.NoError(t, UpsertReplica(context.Background(), "tpl-owner", "cubebox", failedReplica("node-a", "job-2")))

	row := readReplicaRow(t, db, "tpl-owner", "node-a")
	require.Equal(t, ReplicaStatusFailed, row.Status)
	require.Equal(t, "job-2", row.LastJobID)
}

// Stubs node-facing calls only, so replica writes still hit the database.
func stubDistributionCubelets(t *testing.T, createImage func(ip string) (*imagev1.CreateImageRequestResponse, error)) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(cubelet.CreateImage, func(_ context.Context, ip string, _ *imagev1.CreateImageRequest) (*imagev1.CreateImageRequestResponse, error) {
		return createImage(ip)
	})
	patches.ApplyFunc(upsertArtifactNodePlacement, func(context.Context, string, string, string) error { return nil })
}

// Distribution writes a FAILED baseline row too, so it needs the same guard.
func TestDistributionRepairWriteKeepsCleanReadyRow(t *testing.T) {
	db := newReplicaWriteTestDB(t)
	seedReplicaRow(t, db, "tpl-dist", "n1", ReplicaStatusReady, "job-1", false)
	stubDistributionCubelets(t, func(string) (*imagev1.CreateImageRequestResponse, error) {
		return nil, errors.New("create image failed")
	})

	_, _, ready, failed, err := distributeRootfsArtifactToNodes(context.Background(),
		&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
		&types.CreateCubeSandboxReq{InstanceType: "cubebox"},
		readyDistributeArtifact("rfs-dist"), "tpl-dist", "job-1",
		[]*node.Node{{InsID: "n1", IP: "10.0.0.1"}}, replicaWriteRepair)

	require.Error(t, err)
	require.Zero(t, ready)
	require.Equal(t, int32(1), failed)
	require.Equal(t, ReplicaStatusReady, readReplicaRow(t, db, "tpl-dist", "n1").Status)
}

// Control: the same failure on the owner path still publishes FAILED.
func TestDistributionOwnerWritePublishesFailure(t *testing.T) {
	db := newReplicaWriteTestDB(t)
	seedReplicaRow(t, db, "tpl-dist-owner", "n1", ReplicaStatusReady, "job-1", false)
	stubDistributionCubelets(t, func(string) (*imagev1.CreateImageRequestResponse, error) {
		return nil, errors.New("create image failed")
	})

	_, _, _, failed, err := distributeRootfsArtifactToNodes(context.Background(),
		&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
		&types.CreateCubeSandboxReq{InstanceType: "cubebox"},
		readyDistributeArtifact("rfs-dist-owner"), "tpl-dist-owner", "job-2",
		[]*node.Node{{InsID: "n1", IP: "10.0.0.1"}}, replicaWriteOwner)

	require.Error(t, err)
	require.Equal(t, int32(1), failed)
	row := readReplicaRow(t, db, "tpl-dist-owner", "n1")
	require.Equal(t, ReplicaStatusFailed, row.Status)
	require.Equal(t, "job-2", row.LastJobID)
}

// jfs distribution must not fabricate replica rows: a placement push leaves the
// table untouched. Guards the early return in replicaWrite.persist, which is
// otherwise only exercised through stubbed seams.
func TestDistributionNoneWritePersistsNothing(t *testing.T) {
	db := newReplicaWriteTestDB(t)
	stubDistributionCubelets(t, func(string) (*imagev1.CreateImageRequestResponse, error) {
		return nil, errors.New("create image failed")
	})

	_, _, _, failed, _ := distributeRootfsArtifactToNodes(context.Background(),
		&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
		&types.CreateCubeSandboxReq{InstanceType: "cubebox"},
		readyDistributeArtifact("rfs-none"), "tpl-none", "job-1",
		[]*node.Node{{InsID: "n1", IP: "10.0.0.1"}}, replicaWriteNone)

	require.Equal(t, int32(1), failed)
	var count int64
	require.NoError(t, db.Table("t_cube_template_replica").
		Where("template_id = ?", "tpl-none").Count(&count).Error)
	require.Zero(t, count, "replicaWriteNone must not write replica rows")
}
