// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func TestClaimRootfsArtifactAdvancesPublishedGeneration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:artifact-generation-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.RootfsArtifact{}); err != nil {
		t.Fatal(err)
	}
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	req := &types.CreateTemplateFromImageReq{SourceImageRef: "example/image:latest"}

	if err := db.Create(&models.RootfsArtifact{
		ArtifactID: "artifact-ready", Generation: 3, Status: ArtifactStatusReady,
	}).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := claimRootfsArtifactForBuild(context.Background(), "artifact-ready", "fingerprint", req, "digest")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Generation != 4 || claimed.Status != ArtifactStatusBuilding {
		t.Fatalf("claimed row = %+v, want generation 4 BUILDING", claimed)
	}

	if err := db.Create(&models.RootfsArtifact{
		ArtifactID: "artifact-building", Generation: 7, Status: ArtifactStatusBuilding,
	}).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err = claimRootfsArtifactForBuild(context.Background(), "artifact-building", "fingerprint", req, "digest")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Generation != 7 {
		t.Fatalf("duplicate BUILDING claim advanced generation to %d", claimed.Generation)
	}

	claimed, err = claimRootfsArtifactForBuild(context.Background(), "artifact-new", "fingerprint", req, "digest")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Generation != 1 {
		t.Fatalf("new artifact generation = %d, want 1", claimed.Generation)
	}
}
