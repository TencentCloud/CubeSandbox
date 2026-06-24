// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"testing"

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

func TestCleanupMasterLocalArtifactForFinalDeleteOnlyWhenStillPending(t *testing.T) {
	orig := cleanupLocalRootfsArtifactForLifecycle
	defer func() { cleanupLocalRootfsArtifactForLifecycle = orig }()

	calls := 0
	cleanupLocalRootfsArtifactForLifecycle = func(artifactID, ext4Path string) error {
		calls++
		if artifactID != "rfs-1" || ext4Path != "/managed/rfs-1/rootfs.ext4" {
			t.Fatalf("unexpected cleanup args artifact=%q path=%q", artifactID, ext4Path)
		}
		return nil
	}

	artifact := models.RootfsArtifact{
		ArtifactID: "rfs-1",
		Ext4Path:   "/managed/rfs-1/rootfs.ext4",
		Status:     ArtifactStatusBuilding,
	}
	canFinalize, err := cleanupMasterLocalArtifactForFinalDelete(artifact, 0)
	if err != nil || canFinalize || calls != 0 {
		t.Fatalf("building artifact should skip local cleanup, canFinalize=%v err=%v calls=%d", canFinalize, err, calls)
	}

	artifact.Status = ArtifactStatusCleanupPending
	canFinalize, err = cleanupMasterLocalArtifactForFinalDelete(artifact, 1)
	if err != nil || canFinalize || calls != 0 {
		t.Fatalf("referenced artifact should skip local cleanup, canFinalize=%v err=%v calls=%d", canFinalize, err, calls)
	}

	canFinalize, err = cleanupMasterLocalArtifactForFinalDelete(artifact, 0)
	if err != nil || !canFinalize || calls != 1 {
		t.Fatalf("unreferenced cleanup-pending artifact should be locally cleaned, canFinalize=%v err=%v calls=%d", canFinalize, err, calls)
	}
}
