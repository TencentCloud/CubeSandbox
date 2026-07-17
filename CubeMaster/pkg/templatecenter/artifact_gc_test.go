// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func TestProcessArtifactGCCandidatesRecoversPanicAndContinues(t *testing.T) {
	orig := cleanupArtifactFullyGC
	defer func() { cleanupArtifactFullyGC = orig }()

	var mu sync.Mutex
	seen := make([]string, 0, 3)
	cleanupArtifactFullyGC = func(ctx context.Context, artifactID, instanceType, excludeTemplateID string) error {
		mu.Lock()
		seen = append(seen, artifactID)
		mu.Unlock()
		if artifactID == "rfs-panic" {
			panic("boom")
		}
		return nil
	}

	processArtifactGCCandidates(context.Background(), []models.RootfsArtifact{
		{ArtifactID: "rfs-panic"},
		{ArtifactID: "rfs-next-1"},
		{ArtifactID: "rfs-next-2"},
	})

	sort.Strings(seen)
	want := []string{"rfs-next-1", "rfs-next-2", "rfs-panic"}
	if len(seen) != len(want) {
		t.Fatalf("expected %d processed artifacts, got %d: %v", len(want), len(seen), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("unexpected processed artifacts: got %v want %v", seen, want)
		}
	}
}

func TestTrySessionLockPostgreSQL(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()

	gormDB := openMigratedPostgresGORM(t, env)
	ctx := context.Background()

	err := gormDB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		acquired, err := trySessionLock(tx, "cubemaster_test_lock")
		if err != nil {
			return err
		}
		if !acquired {
			t.Fatal("expected pg_try_advisory_lock to succeed on fresh database")
		}
		released, err := releaseSessionLock(tx, "cubemaster_test_lock")
		if err != nil {
			return err
		}
		if !released {
			t.Fatal("expected pg_advisory_unlock to release the held lock")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
}
