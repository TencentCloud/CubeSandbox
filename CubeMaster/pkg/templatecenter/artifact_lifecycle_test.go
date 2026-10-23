// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestCountArtifactReferencesRejectsLikeWildcards(t *testing.T) {
	for _, artifactID := range []string{"rfs-bad%id", "rfs-bad_id"} {
		_, err := countArtifactReferencesTx(context.Background(), nil, artifactID, "")
		if err == nil {
			t.Fatalf("expected wildcard artifact id %q to be rejected", artifactID)
		}
		if !strings.Contains(err.Error(), "SQL wildcard") {
			t.Fatalf("unexpected error for %q: %v", artifactID, err)
		}
	}
}

func TestCleanupArtifactKeepsBuiltJobReference(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&models.TemplateReplica{},
		&models.TemplateImageJob{},
		&models.SnapshotRecord{},
		&models.ArtifactNodePlacement{},
	))
	const artifactID = "rfs-built-owner"
	require.NoError(t, db.Create(&models.RootfsArtifact{
		ArtifactID:              artifactID,
		TemplateSpecFingerprint: artifactID,
		Status:                  ArtifactStatusReady,
	}).Error)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID:      "job-built-owner",
		TemplateID: "tpl-next-owner",
		ArtifactID: artifactID,
		Status:     JobStatusBuilt,
	}).Error)
	require.NoError(t, db.Create(&models.ArtifactNodePlacement{
		ArtifactID: artifactID,
		NodeID:     "node-a",
		NodeIP:     "10.0.0.1",
	}).Error)

	oldNotify := requestTemplateCenterArtifactDelete
	notified := 0
	requestTemplateCenterArtifactDelete = func(context.Context, string) error {
		notified++
		return nil
	}
	t.Cleanup(func() { requestTemplateCenterArtifactDelete = oldNotify })

	require.NoError(t, cleanupArtifactFully(context.Background(), artifactID, "cubebox", "tpl-old-owner"))
	var artifact models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", artifactID).First(&artifact).Error)
	require.Equal(t, ArtifactStatusReady, artifact.Status)
	require.Zero(t, notified)
	var placements int64
	require.NoError(t, db.Model(&models.ArtifactNodePlacement{}).
		Where("artifact_id = ?", artifactID).Count(&placements).Error)
	require.EqualValues(t, 1, placements)
}

func TestCleanupArtifactDropsRetiredPlacement(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&models.TemplateReplica{},
		&models.TemplateImageJob{},
		&models.SnapshotRecord{},
		&models.ArtifactNodePlacement{},
	))
	stubReplicaRegistry(t, true, map[string]string{})
	const artifactID = "rfs-retired-placement"
	require.NoError(t, db.Create(&models.RootfsArtifact{
		ArtifactID:              artifactID,
		TemplateSpecFingerprint: artifactID,
		Status:                  ArtifactStatusReady,
	}).Error)
	require.NoError(t, db.Create(&models.ArtifactNodePlacement{
		ArtifactID: artifactID,
		NodeID:     "node-retired",
		NodeIP:     "10.0.0.9",
	}).Error)

	oldNotify := requestTemplateCenterArtifactDelete
	notified := 0
	requestTemplateCenterArtifactDelete = func(context.Context, string) error {
		notified++
		return nil
	}
	t.Cleanup(func() { requestTemplateCenterArtifactDelete = oldNotify })

	require.NoError(t, cleanupArtifactFully(context.Background(), artifactID, "cubebox", ""))
	var placements int64
	require.NoError(t, db.Model(&models.ArtifactNodePlacement{}).
		Where("artifact_id = ?", artifactID).Count(&placements).Error)
	require.Zero(t, placements)
	require.Equal(t, 1, notified)
}

func TestCleanupArtifactKeepsPlacementBeforeRegistryReady(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&models.TemplateReplica{},
		&models.TemplateImageJob{},
		&models.SnapshotRecord{},
		&models.ArtifactNodePlacement{},
	))
	stubReplicaRegistry(t, false, map[string]string{})
	const artifactID = "rfs-cold-registry-placement"
	require.NoError(t, db.Create(&models.RootfsArtifact{
		ArtifactID:              artifactID,
		TemplateSpecFingerprint: artifactID,
		Status:                  ArtifactStatusReady,
	}).Error)
	require.NoError(t, db.Create(&models.ArtifactNodePlacement{
		ArtifactID: artifactID,
		NodeID:     "node-unclassified",
	}).Error)

	oldNotify := requestTemplateCenterArtifactDelete
	notified := 0
	requestTemplateCenterArtifactDelete = func(context.Context, string) error {
		notified++
		return nil
	}
	t.Cleanup(func() { requestTemplateCenterArtifactDelete = oldNotify })

	require.NoError(t, cleanupArtifactFully(context.Background(), artifactID, "cubebox", ""))
	var placements int64
	require.NoError(t, db.Model(&models.ArtifactNodePlacement{}).
		Where("artifact_id = ?", artifactID).Count(&placements).Error)
	require.EqualValues(t, 1, placements)
	require.Zero(t, notified)
}

func TestRootfsArtifactIDFromCreateRequest(t *testing.T) {
	req := &sandboxtypes.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationRootfsArtifactID: " rfs-top ",
		},
		Containers: []*sandboxtypes.Container{{
			Image: &sandboxtypes.ImageSpec{
				Annotations: map[string]string{
					constants.CubeAnnotationRootfsArtifactID: "rfs-image",
				},
			},
		}},
	}
	if got := rootfsArtifactIDFromCreateRequest(req); got != "rfs-top" {
		t.Fatalf("expected top-level artifact id, got %q", got)
	}

	req.Annotations = nil
	if got := rootfsArtifactIDFromCreateRequest(req); got != "rfs-image" {
		t.Fatalf("expected image artifact id, got %q", got)
	}

	if got := rootfsArtifactIDFromCreateRequest(nil); got != "" {
		t.Fatalf("nil request should have empty artifact id, got %q", got)
	}
}

func TestPlacementToNodePrefersCurrentRegistryAddress(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{"node-a": "10.1.0.8"})
	target := placementToNode(&models.ArtifactNodePlacement{
		NodeID: "node-a",
		NodeIP: "10.0.0.8",
	})
	if target == nil || target.ID() != "node-a" || target.HostIP() != "10.1.0.8" {
		t.Fatalf("resolved placement = %+v", target)
	}
}

func TestArtifactPlacementRetirementRequiresStableMissingNodeID(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{"node-live": "10.1.0.8"})
	if artifactPlacementOnRetiredNode(&models.ArtifactNodePlacement{NodeID: "node-live"}) {
		t.Fatal("registered node must not be retired")
	}
	if !artifactPlacementOnRetiredNode(&models.ArtifactNodePlacement{NodeID: "node-gone"}) {
		t.Fatal("missing stable node id must be retired")
	}
	if artifactPlacementOnRetiredNode(&models.ArtifactNodePlacement{NodeIP: "10.0.0.9"}) {
		t.Fatal("IP-only placement must not be inferred as retired")
	}
}

// requestTemplateCenterArtifactDelete is the ONLY thing allowed to remove an
// artifact's S3 object/local file/row (see CubeTemplateCenter/pkg/build/
// deleter.go). cleanupArtifactFully reaches it via
// notifyTemplateCenterArtifactDelete exactly once per finalized artifact --
// regression test for the S3-leak bug where CubeMaster used to hard-delete the
// row itself without ever notifying TC.
//
// The DB-bound phases of cleanupArtifactFully (1 and 3) are not unit-testable
// without a live database, so this exercises the production notify step
// directly instead of asserting on the stub itself.
func TestNotifyTemplateCenterArtifactDeleteCallsSeamOnce(t *testing.T) {
	orig := requestTemplateCenterArtifactDelete
	defer func() { requestTemplateCenterArtifactDelete = orig }()

	var calledWith []string
	requestTemplateCenterArtifactDelete = func(ctx context.Context, artifactID string) error {
		calledWith = append(calledWith, artifactID)
		return nil
	}

	notifyTemplateCenterArtifactDelete(context.Background(), "rfs-1")
	if len(calledWith) != 1 || calledWith[0] != "rfs-1" {
		t.Fatalf("calledWith = %v, want exactly [rfs-1]", calledWith)
	}
}

// A notify failure must not propagate: the artifact row stays CLEANUP_PENDING
// and TC's reconciler backstop-sweeps it later, so template deletion proceeds.
func TestNotifyTemplateCenterArtifactDeleteSwallowsErrors(t *testing.T) {
	orig := requestTemplateCenterArtifactDelete
	defer func() { requestTemplateCenterArtifactDelete = orig }()

	requestTemplateCenterArtifactDelete = func(ctx context.Context, artifactID string) error {
		return errors.New("tc unreachable")
	}

	// Must not panic; there is no return value to check by design.
	notifyTemplateCenterArtifactDelete(context.Background(), "rfs-2")
}

// When TC's endpoint is not configured, the seam must fail loudly (not
// silently pretend success) so the caller knows to leave the row
// CLEANUP_PENDING rather than assume cleanup happened.
func TestRequestTemplateCenterArtifactDeleteRequiresEndpoint(t *testing.T) {
	t.Setenv("CUBE_TEMPLATE_CENTER_ADDR", "")
	if err := requestTemplateCenterArtifactDelete(context.Background(), "rfs-2"); err == nil {
		t.Fatal("expected error when template center endpoint is not configured")
	}
}
