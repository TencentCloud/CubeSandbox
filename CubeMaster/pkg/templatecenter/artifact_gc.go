// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
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

	artifactGCSelectionTimeout   = 30 * time.Second
	artifactGCLockReleaseTimeout = 5 * time.Second
)

var (
	artifactGCOnce         sync.Once
	cleanupArtifactFullyGC = cleanupArtifactFully
)

// trySessionLock attempts to acquire a cross-instance session lock with 0
// timeout (immediate return). MySQL: GET_LOCK(name, 0); PG: pg_try_advisory_lock(hashtext(name)).
// Caller must pass a *gorm.DB that is pinned to one connection so acquire and
// release share the same session.
func trySessionLock(sess *gorm.DB, name string) (bool, error) {
	dialect := sess.Dialector.Name()
	switch dialect {
	case "postgres":
		var ok bool
		if err := sess.Raw("SELECT pg_try_advisory_lock(hashtext(?))", name).Scan(&ok).Error; err != nil {
			return false, err
		}
		return ok, nil
	case "mysql":
		var res sql.NullInt64
		if err := sess.Raw("SELECT GET_LOCK(?, 0)", name).Scan(&res).Error; err != nil {
			return false, err
		}
		if !res.Valid {
			return false, fmt.Errorf("GET_LOCK %q returned NULL", name)
		}
		switch res.Int64 {
		case 1:
			return true, nil
		case 0:
			return false, nil
		default:
			return false, fmt.Errorf("GET_LOCK %q returned unexpected value %d", name, res.Int64)
		}
	default:
		return false, fmt.Errorf("unsupported database dialect %q", dialect)
	}
}

// releaseSessionLock releases a cross-instance session lock on the same
// connection that acquired it. A false result means the current session is
// known not to hold the lock; an error means the lock state is unknown.
func releaseSessionLock(sess *gorm.DB, name string) (bool, error) {
	dialect := sess.Dialector.Name()
	switch dialect {
	case "postgres":
		var released bool
		if err := sess.Raw("SELECT pg_advisory_unlock(hashtext(?))", name).Scan(&released).Error; err != nil {
			return false, err
		}
		return released, nil
	case "mysql":
		var res sql.NullInt64
		if err := sess.Raw("SELECT RELEASE_LOCK(?)", name).Scan(&res).Error; err != nil {
			return false, err
		}
		if !res.Valid {
			return false, nil
		}
		switch res.Int64 {
		case 1:
			return true, nil
		case 0:
			return false, nil
		default:
			return false, fmt.Errorf("RELEASE_LOCK %q returned unexpected value %d", name, res.Int64)
		}
	default:
		return false, fmt.Errorf("unsupported database dialect %q", dialect)
	}
}

// discardPinnedSession prevents a connection with an uncertain advisory-lock
// state from returning to database/sql's pool. Closing the physical session
// makes MySQL/PostgreSQL release all session-scoped locks it still owns.
func discardPinnedSession(sess *gorm.DB) error {
	if sess == nil || sess.Statement == nil {
		return errors.New("discard pinned session: missing GORM statement")
	}
	conn, ok := sess.Statement.ConnPool.(*sql.Conn)
	if !ok {
		return fmt.Errorf("discard pinned session: unexpected connection pool %T", sess.Statement.ConnPool)
	}
	err := conn.Raw(func(_ any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discard pinned session: %w", err)
	}
	return errors.New("discard pinned session: connection remained usable")
}

// pinnedSessionWithContext derives a clean GORM session on the same pinned
// connection. The candidate query may have populated sess.Error; carrying that
// error into the release session would make GORM skip the unlock SQL entirely.
func pinnedSessionWithContext(sess *gorm.DB, ctx context.Context) *gorm.DB {
	clean := sess.Session(&gorm.Session{NewDB: true})
	clean.Error = nil
	return clean.WithContext(ctx)
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
	candidates, acquired, err := listArtifactGCCandidatesLockedWithError(ctx)
	if err != nil {
		logger.Warnf("artifact gc: candidate selection failed: %v", err)
		return nil, false
	}
	return candidates, acquired
}

func listArtifactGCCandidatesLockedWithError(ctx context.Context) ([]models.RootfsArtifact, bool, error) {
	selectionCtx, cancel := context.WithTimeout(ctx, artifactGCSelectionTimeout)
	defer cancel()

	// Connection pins one physical session without opening a transaction.
	// A transaction is insufficient here: cancellation/abort can make it
	// unusable before unlock, while rollback does not release session locks.
	var (
		candidates []models.RootfsArtifact
		acquired   bool
	)
	err := store.db.WithContext(selectionCtx).Connection(func(sess *gorm.DB) (retErr error) {
		locked, err := trySessionLock(sess, artifactGCLockName)
		if err != nil {
			return errors.Join(fmt.Errorf("acquire lock: %w", err), discardPinnedSession(sess))
		}
		if !locked {
			return nil
		}
		acquired = true
		defer func() {
			releaseCtx, releaseCancel := context.WithTimeout(
				context.WithoutCancel(selectionCtx), artifactGCLockReleaseTimeout)
			defer releaseCancel()

			releaseSess := pinnedSessionWithContext(sess, releaseCtx)
			released, releaseErr := releaseSessionLock(releaseSess, artifactGCLockName)
			if releaseErr != nil {
				// Lock state unknown after a SQL/scan failure: discard so the
				// physical session (and any held advisory lock) cannot re-enter the pool.
				retErr = errors.Join(retErr, fmt.Errorf("release lock: %w", releaseErr), discardPinnedSession(sess))
				return
			}
			if !released {
				// Known non-owner/no-lock result: do not discard. The session is
				// healthy; only the expected lock ownership invariant failed.
				retErr = errors.Join(retErr, errors.New("release lock: current session did not hold lock"))
			}
		}()

		now := time.Now().Unix()
		if err := sess.Select("artifact_id").Table(constants.RootfsArtifactTableName).
			Where("status IN ? OR (gc_deadline > 0 AND gc_deadline < ?)",
				[]string{ArtifactStatusFailed, ArtifactStatusOrphaned, ArtifactStatusCleanupPending}, now).
			Limit(artifactGCMaxPerPass).Find(&candidates).Error; err != nil {
			return fmt.Errorf("list candidates: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return candidates, acquired, nil
}
