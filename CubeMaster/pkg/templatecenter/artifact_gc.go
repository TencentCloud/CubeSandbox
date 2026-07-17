// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
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
	// errArtifactGCLockNotAcquired is returned from the candidate-selection
	// transaction when another instance already holds the session lock.
	errArtifactGCLockNotAcquired = errors.New("artifact gc lock not acquired")
)

// trySessionLock attempts to acquire a cross-instance session lock with 0
// timeout (immediate return). MySQL: GET_LOCK(name, 0); PG: pg_try_advisory_lock(hashtext(name)).
// Caller must pass a *gorm.DB that is pinned to one connection (e.g. inside
// Transaction) so acquire and release share the same session.
func trySessionLock(tx *gorm.DB, name string) bool {
	driver := tx.Dialector.Name()
	switch driver {
	case "postgres":
		var ok bool
		err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", name).Scan(&ok).Error
		return err == nil && ok
	default: // mysql
		var res int64
		err := tx.Raw("SELECT GET_LOCK(?, 0)", name).Scan(&res).Error
		return err == nil && res == 1
	}
}

// releaseSessionLock releases a cross-instance session lock on the same
// connection that acquired it.
func releaseSessionLock(tx *gorm.DB, name string) {
	driver := tx.Dialector.Name()
	var err error
	switch driver {
	case "postgres":
		err = tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", name).Error
	default:
		err = tx.Exec("SELECT RELEASE_LOCK(?)", name).Error
	}
	if err != nil {
		ctx := context.Background()
		if tx.Statement != nil && tx.Statement.Context != nil {
			ctx = tx.Statement.Context
		}
		log.G(ctx).Warnf("artifact gc: release lock %q failed: %v", name, err)
	}
}

// startArtifactGC launches the orphan/expired rootfs-artifact garbage
// collector. It is registered alongside the snapshot reconciler (not folded
// into it) and converges the cases online deletion cannot finish in one pass:
// interrupted builds, artifacts whose nodes were busy (CLEANUP_PENDING), and
// TTL-expired artifacts. A component-scoped advisory lock keeps candidate
// selection single-instance across the HA cluster without covering slow
// cross-node cleanup RPCs; the lock name is intentionally distinct from
// schema-migration locks.
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

	// Pin one connection for acquire + query + release: MySQL GET_LOCK and
	// PostgreSQL pg_try_advisory_lock are session-scoped, so unlocking on a
	// different pooled connection would silently no-op and leak the lock.
	var candidates []models.RootfsArtifact
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !trySessionLock(tx, artifactGCLockName) {
			return errArtifactGCLockNotAcquired
		}
		defer releaseSessionLock(tx, artifactGCLockName)

		now := time.Now().Unix()
		if err := tx.Table(constants.RootfsArtifactTableName).
			Where("status IN ? OR (gc_deadline > 0 AND gc_deadline < ?)",
				[]string{ArtifactStatusFailed, ArtifactStatusOrphaned, ArtifactStatusCleanupPending}, now).
			Limit(artifactGCMaxPerPass).Find(&candidates).Error; err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, errArtifactGCLockNotAcquired) {
		return nil, false
	}
	if err != nil {
		logger.Warnf("artifact gc: list candidates failed: %v", err)
		return nil, false
	}
	return candidates, true
}
