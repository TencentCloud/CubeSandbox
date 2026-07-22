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
	// Single-instance execution across the cluster: a non-blocking advisory lock
	// returns immediately; another instance holding it means we skip this pass.
	// The lock protects only candidate selection. cleanupArtifactFully performs
	// its own row-level serialisation and idempotent physical deletes, so slow
	// RPCs must not keep this HA-wide lock held.
	//
	// Advisory locks are session-scoped on both MySQL (GET_LOCK) and PostgreSQL
	// (pg_try_advisory_lock), so acquire and release must run on the SAME pooled
	// connection. We pin a dedicated *sql.Conn for the whole select span.
	sqlDB, err := store.db.DB()
	if err != nil {
		logger.Warnf("artifact gc: get sql.DB failed: %v", err)
		return nil, false
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		logger.Warnf("artifact gc: pin connection failed: %v", err)
		return nil, false
	}
	defer conn.Close()

	acquireSQL, releaseSQL := artifactGCLockSQL(store.db.Dialector.Name())

	var lockRes sql.NullInt64
	if err := conn.QueryRowContext(ctx, acquireSQL, artifactGCLockName).Scan(&lockRes); err != nil {
		logger.Warnf("artifact gc: acquire lock failed: %v", err)
		return nil, false
	}
	if !lockRes.Valid || lockRes.Int64 != 1 {
		return nil, false // another instance is selecting candidates
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, releaseSQL, artifactGCLockName); err != nil {
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

// artifactGCLockSQL returns the (acquire, release) statements for a
// non-blocking, session-scoped advisory lock keyed by a string name, in the
// dialect of the active driver. Both statements take the lock name as their
// single positional argument and the acquire statement returns a single
// integer column: 1 when the lock was granted, 0 when another session already
// holds it.
//
//   - MySQL:    GET_LOCK(name, 0) already returns 1/0; RELEASE_LOCK(name).
//   - Postgres: advisory locks are keyed by bigint, so the string name is
//     folded to a key with hashtext(); pg_try_advisory_lock returns bool, cast
//     to int so both dialects scan into the same sql.NullInt64.
//
// The statements run on a raw *sql.Conn (not through GORM), so placeholders are
// NOT rewritten by the dialector: MySQL uses "?", PostgreSQL uses "$1".
func artifactGCLockSQL(dialect string) (acquire, release string) {
	switch dialect {
	case "postgres":
		return "SELECT pg_try_advisory_lock(hashtext($1))::int",
			"SELECT pg_advisory_unlock(hashtext($1))"
	default: // "mysql" and backwards-compatible default
		return "SELECT GET_LOCK(?, 0)",
			"SELECT RELEASE_LOCK(?)"
	}
}
