// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestSnapshotReleaseAndArtifactCleanupMySQL(t *testing.T) {
	env := newMySQLDockerEnv(t)
	defer env.teardown()
	db := openMigratedMySQLGORM(t, env)
	old := store.db
	store.db = db
	t.Cleanup(func() { store.db = old })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const artifactID = "rfs-release-parallel"
	require.NoError(t, db.Create(&models.RootfsArtifact{ArtifactID: artifactID, TemplateSpecFingerprint: artifactID, Status: ArtifactStatusReady}).Error)
	// A permanent owner must prevent physical deletion throughout this test.
	require.NoError(t, db.Create(&models.TemplateDefinition{TemplateID: "release-owner", RootfsArtifactID: artifactID}).Error)
	for i := 0; i < 100; i++ {
		require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: fmt.Sprintf("release-%03d", i), RootfsArtifactID: artifactID, Status: StatusDeleting}).Error)
	}
	tx := db.WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	require.NoError(t, updateSnapshotFieldsTx(tx, "release-000", map[string]any{"rootfs_artifact_id": ""}))
	done := make(chan error, 1)
	go func() { done <- cleanupArtifactFully(ctx, artifactID, "cubebox", "") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		_ = tx.Rollback()
		<-done
		t.Fatal("reference counting blocked on a concurrently released snapshot")
	}
	require.NoError(t, tx.Rollback().Error)
	// Interleave independent releases with last-owner checks on one shared rootfs.
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			targets := &templateCleanupTargets{ArtifactIDs: map[string]struct{}{artifactID: {}}}
			if err := releaseSnapshotArtifactReferences(ctx, fmt.Sprintf("release-%03d", i), targets); err != nil {
				errs <- err
				return
			}
			errs <- cleanupArtifactFully(ctx, artifactID, "cubebox", "")
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var remaining int64
	require.NoError(t, db.Model(&models.SnapshotRecord{}).Where("rootfs_artifact_id = ?", artifactID).Count(&remaining).Error)
	require.Zero(t, remaining)
	var artifact models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", artifactID).First(&artifact).Error)
	require.Equal(t, ArtifactStatusReady, artifact.Status)
}
