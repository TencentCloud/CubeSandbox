// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"sort"
	"strings"
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

func TestArtifactGCLockSQL(t *testing.T) {
	tests := []struct {
		dialect         string
		wantAcquire     string
		wantRelease     string
		wantPlaceholder string // placeholder style the raw *sql.Conn must receive
	}{
		{
			dialect:         "postgres",
			wantAcquire:     "SELECT pg_try_advisory_lock(hashtext($1))::int",
			wantRelease:     "SELECT pg_advisory_unlock(hashtext($1))",
			wantPlaceholder: "$1",
		},
		{
			dialect:         "mysql",
			wantAcquire:     "SELECT GET_LOCK(?, 0)",
			wantRelease:     "SELECT RELEASE_LOCK(?)",
			wantPlaceholder: "?",
		},
		{
			// Empty/unknown dialect must fall back to MySQL for backwards
			// compatibility with configs that pre-date the driver field.
			dialect:         "",
			wantAcquire:     "SELECT GET_LOCK(?, 0)",
			wantRelease:     "SELECT RELEASE_LOCK(?)",
			wantPlaceholder: "?",
		},
	}
	for _, tt := range tests {
		t.Run(tt.dialect, func(t *testing.T) {
			acquire, release := artifactGCLockSQL(tt.dialect)
			if acquire != tt.wantAcquire {
				t.Errorf("acquire: got %q want %q", acquire, tt.wantAcquire)
			}
			if release != tt.wantRelease {
				t.Errorf("release: got %q want %q", release, tt.wantRelease)
			}
			if !strings.Contains(acquire, tt.wantPlaceholder) ||
				!strings.Contains(release, tt.wantPlaceholder) {
				t.Errorf("expected placeholder %q in both statements, got acquire=%q release=%q",
					tt.wantPlaceholder, acquire, release)
			}
		})
	}
}
