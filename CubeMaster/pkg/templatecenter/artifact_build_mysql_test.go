// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// TestClaimRootfsArtifactForBuildConcurrentMySQL covers the production-default
// dialect. InnoDB may resolve concurrent first inserts with a duplicate key,
// deadlock, or lock timeout; all callers must still converge on one live owner.
func TestClaimRootfsArtifactForBuildConcurrentMySQL(t *testing.T) {
	gormDB, _ := newArtifactGCMySQL(t)
	// The shared MySQL fixture uses AutoMigrate, while the production baseline
	// defines artifact_id as unique. Recreate that constraint explicitly so the
	// contention behavior matches the real schema.
	if err := gormDB.Exec("CREATE UNIQUE INDEX idx_artifact_build_test_id ON " + constants.RootfsArtifactTableName + " (artifact_id(128))").Error; err != nil {
		t.Fatalf("create artifact_id test index: %v", err)
	}
	oldDB := store.db
	store.db = gormDB
	t.Cleanup(func() { store.db = oldDB })

	const callers = 24
	artifactID := fmt.Sprintf("artifact-concurrent-mysql-%d", time.Now().UnixNano())
	req := &types.CreateTemplateFromImageReq{
		SourceImageRef:    "docker.io/library/busybox:latest",
		WritableLayerSize: "1Gi",
	}
	type outcome struct {
		record *models.RootfsArtifact
		owner  bool
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			record, owner, err := claimRootfsArtifactForBuild(context.Background(), artifactID, "fingerprint-concurrent-mysql", req, "sha256:source")
			outcomes <- outcome{record: record, owner: owner, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	owners := 0
	for result := range outcomes {
		if result.err != nil {
			t.Fatalf("concurrent MySQL claim failed: %v", result.err)
		}
		if result.record == nil {
			t.Fatal("concurrent MySQL claim returned no record")
		}
		if result.owner {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("owners=%d, want exactly one", owners)
	}

	var record models.RootfsArtifact
	if err := gormDB.Table(constants.RootfsArtifactTableName).Where("artifact_id = ?", artifactID).First(&record).Error; err != nil {
		t.Fatalf("load claimed artifact: %v", err)
	}
	if record.Status != ArtifactStatusBuilding || record.BuildGeneration != 1 || record.BuildOwnerToken == "" {
		t.Fatalf("unexpected claimed record: status=%q generation=%d owner=%q", record.Status, record.BuildGeneration, record.BuildOwnerToken)
	}
	if record.BuildLeaseExpireAt <= time.Now().Unix() {
		t.Fatalf("lease is not live: %d", record.BuildLeaseExpireAt)
	}

	if err := finalizeRootfsArtifactRecord(context.Background(), &record, map[string]any{
		"status":                ArtifactStatusReady,
		"build_lease_expire_at": int64(0),
	}); err != nil {
		t.Fatalf("finalize claimed artifact: %v", err)
	}
	if err := renewRootfsArtifactBuildLease(context.Background(), &record); err != nil {
		t.Fatalf("late renewal after READY must be treated as a completed lease, got %v", err)
	}

	expired := &models.RootfsArtifact{
		ArtifactID:         fmt.Sprintf("artifact-expired-mysql-%d", time.Now().UnixNano()),
		Status:             ArtifactStatusBuilding,
		BuildOwnerToken:    "expired-owner",
		BuildGeneration:    1,
		BuildLeaseExpireAt: time.Now().Add(-time.Minute).Unix(),
	}
	if err := gormDB.Create(expired).Error; err != nil {
		t.Fatalf("seed expired build: %v", err)
	}
	if err := finalizeRootfsArtifactRecord(context.Background(), expired, map[string]any{"status": ArtifactStatusReady}); !errors.Is(err, errRootfsArtifactBuildLeaseLost) {
		t.Fatalf("expired owner finalize error=%v, want lease lost", err)
	}
}
