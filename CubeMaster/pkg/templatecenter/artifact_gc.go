// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"runtime/debug"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

const (
	artifactGCInterval    = 10 * time.Minute
	artifactGCLockName    = "cubemaster_templatecenter_artifact_gc_v1"
	artifactGCMaxPerPass  = 100
	artifactGCWorkerLimit = 5
)

var (
	artifactGCOnce         sync.Once
	cleanupArtifactFullyGC = cleanupArtifactFully
)

// startArtifactGC launches the orphan/expired rootfs-artifact garbage
// collector. It is registered alongside the snapshot reconciler (not folded
// into it) and converges the cases online deletion cannot finish in one pass:
// interrupted builds, artifacts whose nodes were busy (CLEANUP_PENDING), and
// TTL-expired artifacts. A component-scoped MySQL GET_LOCK keeps candidate
// selection single-instance across the HA cluster without covering slow
// cross-node cleanup RPCs; the lock name is intentionally distinct from
// schema-migration locks (`cubemaster_schema_migration_global` and
// `cubemaster_migration_*`).
func startArtifactGC(ctx context.Context) {
	artifactGCOnce.Do(func() {
		go func() {
			runArtifactGCPass(detachTemplateImageJobContext(ctx, "artifact_gc", nil))
			ticker := time.NewTicker(artifactGCInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runArtifactGCPass(detachTemplateImageJobContext(ctx, "artifact_gc", nil))
				}
			}
		}()
	})
}

func runArtifactGCPass(ctx context.Context) {
	if !isReady() {
		return
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "artifact_gc"})

	candidates, ok := listArtifactGCCandidatesLocked(ctx)
	if !ok || len(candidates) == 0 {
		return
	}
	logger.Infof("artifact gc: evaluating %d candidate artifacts", len(candidates))
	processArtifactGCCandidates(ctx, candidates)
}

func processArtifactGCCandidates(ctx context.Context, candidates []models.RootfsArtifact) {
	if len(candidates) == 0 {
		return
	}
	workerCount := artifactGCWorkerLimit
	if len(candidates) < workerCount {
		workerCount = len(candidates)
	}
	jobs := make(chan models.RootfsArtifact)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for artifact := range jobs {
				cleanupArtifactGCCandidate(ctx, artifact)
			}
		}()
	}
	for i := range candidates {
		jobs <- candidates[i]
	}
	close(jobs)
	wg.Wait()
}

func cleanupArtifactGCCandidate(ctx context.Context, artifact models.RootfsArtifact) {
	logger := log.G(ctx).WithFields(map[string]any{"component": "artifact_gc"})
	artifactID := artifact.ArtifactID
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("artifact gc: cleanup %s panic: %v\n%s", artifactID, r, string(debug.Stack()))
		}
	}()
	if artifactID != "" {
		// exclude="" => globally unreferenced artifacts are cleaned; referenced
		// ones are kept and their TTL renewed by cleanupArtifactFully. ext4
		// instanceType defaults to cubebox inside the node destroy path.
		if err := cleanupArtifactFullyGC(ctx, artifactID, "", ""); err != nil {
			logger.Warnf("artifact gc: cleanup %s failed: %v", artifactID, err)
		}
	}
}

func listArtifactGCCandidatesLocked(ctx context.Context) ([]models.RootfsArtifact, bool) {
	logger := log.G(ctx).WithFields(map[string]any{"component": "artifact_gc"})
	// Single-instance execution across the cluster: GET_LOCK with a 0s timeout
	// returns immediately; another instance holding it means we skip this pass.
	// The lock protects only candidate selection. cleanupArtifactFully performs
	// its own row-level serialisation and idempotent physical deletes, so slow
	// RPCs must not keep this HA-wide lock held.
	var lockRes sql.NullInt64
	if err := store.db.WithContext(ctx).
		Raw("SELECT GET_LOCK(?, ?)", artifactGCLockName, 0).Scan(&lockRes).Error; err != nil {
		logger.Warnf("artifact gc: acquire lock failed: %v", err)
		return nil, false
	}
	if !lockRes.Valid || lockRes.Int64 != 1 {
		return nil, false // another instance is selecting candidates
	}
	defer func() {
		if err := store.db.WithContext(ctx).Exec("SELECT RELEASE_LOCK(?)", artifactGCLockName).Error; err != nil {
			logger.Warnf("artifact gc: release lock failed: %v", err)
		}
	}()

	now := time.Now().Unix()
	var candidates []models.RootfsArtifact
	if err := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("status IN ? OR (gc_deadline > 0 AND gc_deadline < ?)",
			[]string{ArtifactStatusFailed, ArtifactStatusOrphaned, ArtifactStatusCleanupPending}, now).
		Limit(artifactGCMaxPerPass).Find(&candidates).Error; err != nil {
		logger.Warnf("artifact gc: list candidates failed: %v", err)
		return nil, false
	}
	return candidates, true
}
