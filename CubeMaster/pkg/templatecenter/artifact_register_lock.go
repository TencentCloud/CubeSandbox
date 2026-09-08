// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This file implements the cluster-wide registration lock that makes the
// BUILT-callback artifact registration mutually exclusive with
// CubeTemplateCenter's build critical section.
//
// Why the lock name must match TC's: t_cube_rootfs_artifact carries a UNIQUE
// index on template_spec_fingerprint (idx_artifact_fingerprint), while
// buildArtifactID mints a UUID-suffixed artifact_id per build. Two
// concurrent same-spec builds therefore reach registration with DIFFERENT
// artifact_ids; both miss the artifact_id lookup and the second insert fails
// with Error 1062. TC already serializes its builds on
// GET_LOCK("tc_build_<fingerprint>") (CubeTemplateCenter/pkg/lock), so
// running the registration under the SAME named lock makes a registering
// CubeMaster and a building TC replica (or two CubeMaster replicas) exclude
// each other, and the loser finds the winner's READY row by fingerprint
// instead of inserting a duplicate.

const (
	// maxMySQLLockNameLen is MySQL's hard limit for GET_LOCK/RELEASE_LOCK
	// identifiers. Same constant as CubeTemplateCenter/pkg/lock — keep the
	// normalization below byte-identical to TC's.
	maxMySQLLockNameLen = 64

	// artifactRegisterLockTimeout bounds the blocking GET_LOCK wait. The
	// critical section it protects is a handful of DB round-trips
	// (claim + finalize), never ext4 builds or cross-node RPCs, so a short
	// wait suffices; on timeout the caller leaves the job in BUILT and the
	// image-job reconciler replays the registration later.
	artifactRegisterLockTimeout = 10 * time.Second

	// artifactRegisterLockPoll is the retry cadence for the PostgreSQL
	// advisory-lock path (pg's blocking pg_advisory_lock has no timeout).
	artifactRegisterLockPoll = 200 * time.Millisecond
)

// errArtifactRegisterRetryable marks registration outcomes that must NOT
// fail the job: the job stays BUILT and the image-job reconciler replays the
// resume (lock held by a peer past the timeout, or a crashed predecessor's
// half-registered row still holding the fingerprint).
var errArtifactRegisterRetryable = errors.New("artifact registration deferred; reconciler will retry")

// errArtifactFinalizeLostCAS means the final status-guarded UPDATE found the
// row already finalized by a duplicate BUILT replay that skipped the named
// register lock. The caller re-reads the row and adopts the winner's READY
// version instead of rotating its download_token.
var errArtifactFinalizeLostCAS = errors.New("artifact finalized by a concurrent replay")

// normalizeArtifactLockName mirrors CubeTemplateCenter pkg/lock's
// normalizeLockName EXACTLY. "tc_build_<64-char sha256>" is 73 bytes, over
// MySQL's 64-char GET_LOCK limit (Error 4163), so both sides shorten long
// names to <prefix>_<16 hex of sha256(full name)>. If the two
// implementations ever diverge, the processes take DIFFERENT physical locks
// for the same fingerprint and the mutual exclusion silently disappears.
func normalizeArtifactLockName(name string) string {
	if len(name) <= maxMySQLLockNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:])[:16] // 17 bytes, fixed width
	prefixBudget := maxMySQLLockNameLen - len(suffix)
	if prefixBudget < 0 {
		prefixBudget = 0
	}
	prefix := name
	if len(prefix) > prefixBudget {
		prefix = prefix[:prefixBudget]
	}
	return prefix + suffix
}

// artifactRegisterLockName returns the shared lock name for a template-spec
// fingerprint. MUST stay identical to CubeTemplateCenter's WithBuildLock
// name construction ("tc_build_" + artifactID, called with the fingerprint).
func artifactRegisterLockName(fingerprint string) string {
	return "tc_build_" + fingerprint
}

// blockingSessionLock acquires the named session lock, waiting up to
// timeout. MySQL: GET_LOCK(name, seconds). PostgreSQL: bounded
// pg_try_advisory_lock retry loop (pg's blocking pg_advisory_lock has no
// timeout). The caller must pass a session pinned to one physical
// connection (withArtifactRegisterLock does).
//
// name must already be normalized (the caller normalizes once so acquire and
// release agree; releaseSessionLock does not normalize on this side).
func blockingSessionLock(ctx context.Context, sess *gorm.DB, name string, timeout time.Duration) (bool, error) {
	switch sess.Dialector.Name() {
	case "mysql":
		var res sql.NullInt64
		if err := sess.Raw("SELECT GET_LOCK(?, ?)", name, int(timeout.Seconds())).Scan(&res).Error; err != nil {
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
	case "postgres":
		deadline := time.Now().Add(timeout)
		for {
			var ok bool
			if err := sess.Raw("SELECT pg_try_advisory_lock(hashtext(?))", name).Scan(&ok).Error; err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
			if time.Now().After(deadline) {
				return false, nil
			}
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(artifactRegisterLockPoll):
			}
		}
	default:
		return false, fmt.Errorf("unsupported database dialect %q", sess.Dialector.Name())
	}
}

// withArtifactRegisterLock runs fn while holding the cluster-wide
// fingerprint lock on ONE pinned physical connection (GET_LOCK and
// RELEASE_LOCK must share a session — same discipline as artifact_gc.go,
// including discarding the connection when the lock state becomes
// uncertain).
//
// Dialects without named session locks (sqlite in unit tests) skip the lock
// and run fn directly: single-process test/dev setups are already serialized
// by the in-process resumeArtifactLocks mutex.
func withArtifactRegisterLock(ctx context.Context, fingerprint string, fn func(sess *gorm.DB) error) error {
	lockName := normalizeArtifactLockName(artifactRegisterLockName(fingerprint))
	return store.db.WithContext(ctx).Connection(func(sess *gorm.DB) error {
		dialect := sess.Dialector.Name()
		if dialect != "mysql" && dialect != "postgres" {
			log.G(ctx).Debugf("artifact register lock skipped on dialect %s", dialect)
			return fn(sess)
		}
		locked, err := blockingSessionLock(ctx, sess, lockName, artifactRegisterLockTimeout)
		if err != nil {
			return errors.Join(fmt.Errorf("acquire register lock: %w", err), discardPinnedSession(sess))
		}
		if !locked {
			return fmt.Errorf("%w: register lock %q held by a peer for over %s",
				errArtifactRegisterRetryable, lockName, artifactRegisterLockTimeout)
		}
		defer func() {
			releaseSess := pinnedSessionWithContext(sess, context.Background())
			if _, relErr := releaseSessionLock(releaseSess, lockName); relErr != nil {
				_ = discardPinnedSession(sess)
			}
		}()
		return fn(sess)
	})
}

// findRootfsArtifactByFingerprintForUpdate returns the newest artifact row
// holding a template-spec fingerprint (including soft-deleted rows), read on
// the pinned lock session so the lookup agrees with the registration
// critical section it runs inside. The named session lock is the actual
// mutual exclusion; the FOR UPDATE clause only orders the read against any
// legacy writer that updates the row without taking the named lock.
func findRootfsArtifactByFingerprintForUpdate(sess *gorm.DB, fingerprint string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := sess.Clauses(clause.Locking{Strength: "UPDATE"}).Unscoped().
		Table(constants.RootfsArtifactTableName).
		Where("template_spec_fingerprint = ?", fingerprint).
		Order("created_at DESC").
		First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}
