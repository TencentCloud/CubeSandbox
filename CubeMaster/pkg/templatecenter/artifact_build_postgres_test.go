// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// TestClaimRootfsArtifactForBuildConcurrentPostgres exercises independent DB
// transactions, as separate CubeMaster replicas would. The unique artifact ID
// plus live lease must elect exactly one builder; every other caller observes
// the same BUILDING generation instead of starting another mkfs.ext4 build.
func TestClaimRootfsArtifactForBuildConcurrentPostgres(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()

	oldDB := store.db
	store.db = openMigratedPostgresGORM(t, env)
	t.Cleanup(func() { store.db = oldDB })

	const callers = 24
	artifactID := fmt.Sprintf("artifact-concurrent-%d", time.Now().UnixNano())
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
			record, owner, err := claimRootfsArtifactForBuild(context.Background(), artifactID, "fingerprint-concurrent", req, "sha256:source")
			outcomes <- outcome{record: record, owner: owner, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	owners := 0
	for result := range outcomes {
		if result.err != nil {
			t.Fatalf("concurrent claim failed: %v", result.err)
		}
		if result.record == nil {
			t.Fatal("concurrent claim returned no record")
		}
		if result.owner {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("owners=%d, want exactly one", owners)
	}

	var record models.RootfsArtifact
	if err := store.db.Table(constants.RootfsArtifactTableName).Where("artifact_id = ?", artifactID).First(&record).Error; err != nil {
		t.Fatalf("load claimed artifact: %v", err)
	}
	if record.Status != ArtifactStatusBuilding || record.BuildGeneration != 1 || record.BuildOwnerToken == "" {
		t.Fatalf("unexpected claimed record: status=%q generation=%d owner=%q", record.Status, record.BuildGeneration, record.BuildOwnerToken)
	}
	if record.BuildLeaseExpireAt <= time.Now().Unix() {
		t.Fatalf("lease is not live: %d", record.BuildLeaseExpireAt)
	}
}
