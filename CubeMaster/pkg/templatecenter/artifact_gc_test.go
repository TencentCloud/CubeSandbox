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
