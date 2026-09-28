// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func TestSnapshotArtifactLifecycle(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.SnapshotRecord{}, &models.TemplateReplica{}, &models.TemplateImageJob{}, &models.ArtifactNodePlacement{}))
	testSnapshotArtifactLifecycle(t, db)
	testSnapshotArtifactCleanupRetry(t, db)
	testSnapshotReferenceReleaseRollback(t, db)
	testSnapshotArtifactConcurrentCleanup(t, db)
}

func TestSnapshotArtifactLifecyclePostgreSQL(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()
	db := openMigratedPostgresGORM(t, env)
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	testSnapshotArtifactLifecycle(t, db)
	testSnapshotArtifactCleanupRetry(t, db)
	testSnapshotReferenceReleaseRollback(t, db)
	testSnapshotArtifactConcurrentCleanup(t, db)
}

func TestSnapshotArtifactLifecycleMySQL(t *testing.T) {
	env := newMySQLDockerEnv(t)
	defer env.teardown()
	db := openMigratedMySQLGORM(t, env)
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	testSnapshotArtifactLifecycle(t, db)
	testSnapshotArtifactCleanupRetry(t, db)
	testSnapshotReferenceReleaseRollback(t, db)
	testSnapshotArtifactConcurrentCleanup(t, db)
}

func testSnapshotArtifactLifecycle(t *testing.T, db *gorm.DB) {
	ctx := context.Background()
	artifactID := "rfs-snapshot-reference"
	require.NoError(t, db.Create(&models.RootfsArtifact{ArtifactID: artifactID, TemplateSpecFingerprint: artifactID, Status: ArtifactStatusReady}).Error)
	require.NoError(t, db.Create(&models.TemplateDefinition{TemplateID: "source", RootfsArtifactID: artifactID}).Error)
	req := &sandboxtypes.CreateCubeSandboxReq{Annotations: map[string]string{constants.CubeAnnotationRootfsArtifactID: artifactID}}
	for _, id := range []string{"snapshot-a", "snapshot-b"} {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			return createSnapshotTx(ctx, tx, id, req, "cubebox", "v2", &models.SnapshotRecord{Status: StatusReady})
		}))
	}
	notified := 0
	oldNotify := requestTemplateCenterArtifactDelete
	requestTemplateCenterArtifactDelete = func(context.Context, string) error { notified++; return nil }
	t.Cleanup(func() { requestTemplateCenterArtifactDelete = oldNotify })
	require.NoError(t, cleanupArtifactFully(ctx, artifactID, "cubebox", "source"))
	require.NoError(t, db.Unscoped().Where("template_id = ?", "source").Delete(&models.TemplateDefinition{}).Error)
	var artifact models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", artifactID).First(&artifact).Error)
	require.Equal(t, ArtifactStatusReady, artifact.Status)
	require.Zero(t, notified)
	snap, err := getSnapshotRecord(ctx, "snapshot-a")
	require.NoError(t, err)
	require.Equal(t, artifactID, snap.RootfsArtifactID)
	restored, err := requestFromSnapshotJSON(snap.RequestJSON)
	require.NoError(t, err)
	require.Equal(t, artifactID, rootfsArtifactIDFromCreateRequest(restored))
	// Tombstones can have live runtime bindings; retain their references.
	require.NoError(t, updateSnapshotFields(ctx, "snapshot-b", map[string]any{"status": StatusDeleted}))
	require.NoError(t, cleanupTemplateMetadata(ctx, "snapshot-a"))
	require.NoError(t, cleanupArtifactFully(ctx, artifactID, "cubebox", "snapshot-a"))
	require.Zero(t, notified)
	require.NoError(t, cleanupTemplateMetadata(ctx, "snapshot-b"))
	require.NoError(t, cleanupArtifactFully(ctx, artifactID, "cubebox", "snapshot-b"))
	require.Equal(t, 1, notified)
	// New owners must not enter the lock-free physical deletion phase.
	err = db.Transaction(func(tx *gorm.DB) error {
		return createSnapshotTx(ctx, tx, "too-late", req, "cubebox", "v2", nil)
	})
	require.ErrorContains(t, err, "not ready")
	var count int64
	require.NoError(t, db.Model(&models.SnapshotRecord{}).Where("snapshot_id = ?", "too-late").Count(&count).Error)
	require.Zero(t, count)
}

func testSnapshotArtifactConcurrentCleanup(t *testing.T, db *gorm.DB) {
	if db.Dialector.Name() == "sqlite" {
		return
	} // SQLite has no row locks.
	ctx := context.Background()
	artifactID := "rfs-concurrent-snapshot"
	require.NoError(t, db.Create(&models.RootfsArtifact{ArtifactID: artifactID, TemplateSpecFingerprint: artifactID, Status: ArtifactStatusReady}).Error)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	req := &sandboxtypes.CreateCubeSandboxReq{Annotations: map[string]string{constants.CubeAnnotationRootfsArtifactID: artifactID}}
	require.NoError(t, createSnapshotTx(ctx, tx, "concurrent-snapshot", req, "cubebox", "v2", nil))
	done := make(chan error, 1)
	go func() { done <- cleanupArtifactFully(ctx, artifactID, "cubebox", "") }()
	select {
	case err := <-done:
		t.Fatalf("cleanup passed an uncommitted snapshot reference: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, tx.Commit().Error)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not resume after snapshot commit")
	}
	var artifact models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", artifactID).First(&artifact).Error)
	require.Equal(t, ArtifactStatusReady, artifact.Status)
}

func testSnapshotArtifactCleanupRetry(t *testing.T, db *gorm.DB) {
	ctx := context.Background()
	const snapshotID = "snapshot-cleanup-retry"
	const artifactID = "rfs-cleanup-retry"
	require.NoError(t, db.Create(&models.RootfsArtifact{ArtifactID: artifactID, TemplateSpecFingerprint: artifactID, Status: ArtifactStatusReady}).Error)
	require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: snapshotID, RootfsArtifactID: artifactID, Status: StatusReady, InstanceType: "cubebox", OriginNodeID: "node-retry", OriginNodeIP: "10.0.0.1", RequestJSON: "{}"}).Error)
	oldReplicaCleanup := runReplicaCleanup
	oldCount := countActiveSnapshotRuntimeRefsFn
	oldNotify := requestTemplateCenterArtifactDelete
	replicasCleaned := 0
	runReplicaCleanup = func(context.Context, string, []templateCleanupLocator, string) error {
		replicasCleaned++
		if replicasCleaned > 1 {
			return errors.New("previous cleanup node is offline")
		}
		return nil
	}
	countActiveSnapshotRuntimeRefsFn = func(context.Context, string) (int64, error) { return 0, nil }
	notified := 0
	requestTemplateCenterArtifactDelete = func(_ context.Context, id string) error { require.Equal(t, artifactID, id); notified++; return nil }
	defer func() {
		runReplicaCleanup = oldReplicaCleanup
		countActiveSnapshotRuntimeRefsFn = oldCount
		requestTemplateCenterArtifactDelete = oldNotify
	}()
	injected := errors.New("artifact database unavailable")
	failNext := true
	const callbackName = "test:snapshot-artifact-failure"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if failNext && tx.Statement.Table == constants.RootfsArtifactTableName {
			failNext = false
			tx.AddError(injected)
		}
	}))
	defer db.Callback().Query().Remove(callbackName)
	_, err := DeleteSnapshot(ctx, "delete-retry-first", snapshotID, "cubebox")
	require.ErrorIs(t, err, injected)
	require.False(t, failNext)
	rec, err := getSnapshotRecord(ctx, snapshotID)
	require.NoError(t, err)
	require.Equal(t, StatusDeleted, rec.Status)
	require.Empty(t, rec.RootfsArtifactID)
	require.JSONEq(t, `["rfs-cleanup-retry"]`, rec.CleanupArtifactIDsJSON)
	job, err := getTemplateImageJobByRequestID(ctx, "delete-retry-first")
	require.NoError(t, err)
	require.Equal(t, JobStatusFailed, job.Status)
	require.Contains(t, job.ErrorMessage, injected.Error())
	require.Zero(t, notified)
	// A failed request stays terminal; a fresh request resumes the durable plan.
	_, err = DeleteSnapshot(ctx, "delete-retry-first", snapshotID, "cubebox")
	require.ErrorContains(t, err, injected.Error())
	targets, err := discoverTemplateCleanupTargets(ctx, snapshotID, "")
	require.NoError(t, err)
	require.Contains(t, targets.ArtifactIDs, artifactID)
	info, err := DeleteSnapshot(ctx, "delete-retry-second", snapshotID, "cubebox")
	require.NoError(t, err)
	require.Equal(t, JobStatusReady, info.Status)
	require.Equal(t, 1, notified)
	require.Equal(t, 1, replicasCleaned)
	_, err = getSnapshotRecord(ctx, snapshotID)
	require.ErrorIs(t, err, ErrSnapshotNotFound)
}

func testSnapshotReferenceReleaseRollback(t *testing.T, db *gorm.DB) {
	ctx := context.Background()
	const snapshotID = "snapshot-release-rollback"
	const artifactID = "rfs-release-rollback"
	require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: snapshotID, RootfsArtifactID: artifactID, Status: StatusDeleting}).Error)
	require.NoError(t, db.Create(&models.TemplateReplica{TemplateID: snapshotID, NodeID: "rollback-node", ArtifactID: artifactID}).Error)
	const callbackName = "test:snapshot-release-failure"
	injected := errors.New("replica metadata database error")
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == constants.TemplateReplicaTableName {
			tx.AddError(injected)
		}
	}))
	targets := &templateCleanupTargets{ArtifactIDs: map[string]struct{}{artifactID: {}}}
	err := releaseSnapshotArtifactReferences(ctx, snapshotID, targets)
	require.ErrorIs(t, err, injected)
	require.NoError(t, db.Callback().Delete().Remove(callbackName))
	rec, err := getSnapshotRecord(ctx, snapshotID)
	require.NoError(t, err)
	require.Equal(t, artifactID, rec.RootfsArtifactID)
	require.Empty(t, rec.CleanupArtifactIDsJSON)
	var remaining int64
	require.NoError(t, db.Model(&models.TemplateReplica{}).Where("template_id = ?", snapshotID).Count(&remaining).Error)
	require.EqualValues(t, 1, remaining)
	// Retry atomically saves the target while removing both reference sources.
	require.NoError(t, releaseSnapshotArtifactReferences(ctx, snapshotID, targets))
	rec, err = getSnapshotRecord(ctx, snapshotID)
	require.NoError(t, err)
	require.Empty(t, rec.RootfsArtifactID)
	require.JSONEq(t, `["rfs-release-rollback"]`, rec.CleanupArtifactIDsJSON)
	require.NoError(t, db.Model(&models.TemplateReplica{}).Where("template_id = ?", snapshotID).Count(&remaining).Error)
	require.Zero(t, remaining)
}
