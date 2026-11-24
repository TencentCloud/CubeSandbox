// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func TestRedoArtifactSharedChecksPeerReferences(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.SnapshotRecord{}, &models.TemplateReplica{}, &models.TemplateImageJob{}))

	const artifactID = "rfs-shared-redo-test"
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:       "tpl-a",
		InstanceType:     "cubebox",
		Status:           StatusReady,
		RootfsArtifactID: artifactID,
		RequestJSON:      "{}",
	}).Error)

	// Sole owner: excluding tpl-a leaves zero peer references.
	shared, err := redoArtifactShared(context.Background(), artifactID, "tpl-a")
	require.NoError(t, err)
	require.False(t, shared)

	// Adding a second template referencing the same artifact marks it shared.
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:       "tpl-b",
		InstanceType:     "cubebox",
		Status:           StatusReady,
		RootfsArtifactID: artifactID,
		RequestJSON:      "{}",
	}).Error)

	shared, err = redoArtifactShared(context.Background(), artifactID, "tpl-a")
	require.NoError(t, err)
	require.True(t, shared)
}

func TestCleanupRedoArtifactBeforeRedistribute(t *testing.T) {
	origShared := redoArtifactShared
	origDelete := deleteImageOnCubelet
	origAddr := getCubeletAddrForDelete
	t.Cleanup(func() {
		redoArtifactShared = origShared
		deleteImageOnCubelet = origDelete
		getCubeletAddrForDelete = origAddr
	})

	getCubeletAddrForDelete = func(ip string) string { return ip + ":9999" }
	var deleteCalls int
	var deletedArtifactID string
	deleteImageOnCubelet = func(_ context.Context, _ string, req *imagev1.DestroyImageRequest) (*imagev1.DestroyImageResponse, error) {
		deleteCalls++
		if req != nil && req.Spec != nil {
			deletedArtifactID = req.Spec.Image
		}
		return &imagev1.DestroyImageResponse{}, nil
	}
	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1"}}

	// Shared artifact: must skip node cleanup completely.
	redoArtifactShared = func(context.Context, string, string) (bool, error) { return true, nil }
	require.NoError(t, cleanupRedoArtifactBeforeRedistribute(context.Background(), "rfs-shared", "cubebox", "tpl-a", targets))
	require.Equal(t, 0, deleteCalls)

	// Reference lookup error: must fail closed without deleting on nodes.
	redoArtifactShared = func(context.Context, string, string) (bool, error) { return false, errors.New("db unavailable") }
	err := cleanupRedoArtifactBeforeRedistribute(context.Background(), "rfs-shared", "cubebox", "tpl-a", targets)
	require.Error(t, err)
	require.Contains(t, err.Error(), "check shared artifact references before cleanup failed")
	require.Equal(t, 0, deleteCalls)

	// Sole owner (not shared): must delete the artifact on target nodes.
	redoArtifactShared = func(context.Context, string, string) (bool, error) { return false, nil }
	require.NoError(t, cleanupRedoArtifactBeforeRedistribute(context.Background(), "rfs-sole", "cubebox", "tpl-a", targets))
	require.Equal(t, 1, deleteCalls)
	require.Equal(t, "rfs-sole", deletedArtifactID)
}
