// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lock

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// Example usage patterns for the session locks. These are placeholder
// callers for PR 5 — the real builder/reconciler code moves over then.

// WithBuildLock serializes ext4 builds for a given artifactID across
// multiple template center replicas.
//
// Mirrors design §9.2: replaces the per-process sync.Map[artifactID]*sync.Mutex
// (CubeMaster/pkg/templatecenter/artifact_build.go:56) with a cross-instance
// DB session lock.
//
// Lock name: "tc_build_<artifactID>" — scoped per artifact, so different
// artifacts still build in parallel across replicas.
//
// Returns ErrBuildInProgress when another replica holds the lock. The caller
// should poll the rootfs_artifacts row until it becomes READY (then reuse)
// or FAILED (then return), per design §9.2.
func WithBuildLock(ctx context.Context, db *gorm.DB, artifactID string, fn func() error) error {
	lockName := "tc_build_" + artifactID

	conn, sess, err := PinConn(db)
	if err != nil {
		return fmt.Errorf("build lock %q: %w", lockName, err)
	}
	defer func() { _ = conn.Close() }()

	ok, err := TrySessionLock(sess, lockName)
	if err != nil {
		// Lock state uncertain — discard the conn so MySQL/PG release anything held.
		_ = DiscardPinnedSession(sess)
		return fmt.Errorf("build lock %q: %w", lockName, err)
	}
	if !ok {
		return ErrBuildInProgress
	}

	releaseSess := PinnedSessionWithContext(sess, ctx)
	defer func() {
		_, _ = ReleaseSessionLock(releaseSess, lockName)
	}()

	return fn()
}

// ErrBuildInProgress means another replica holds the build lock for this
// artifact. The caller should poll the artifact row until it converges.
var ErrBuildInProgress = fmt.Errorf("another template center replica is building this artifact")

// WithReconcileLock serializes the background reconciler sweep across
// replicas. Global lock name — only one replica reconciles at a time.
func WithReconcileLock(ctx context.Context, db *gorm.DB, fn func() error) error {
	const lockName = "tc_reconcile_v1"

	conn, sess, err := PinConn(db)
	if err != nil {
		return fmt.Errorf("reconcile lock: %w", err)
	}
	defer func() { _ = conn.Close() }()

	ok, err := TrySessionLock(sess, lockName)
	if err != nil {
		_ = DiscardPinnedSession(sess)
		return fmt.Errorf("reconcile lock: %w", err)
	}
	if !ok {
		// Another replica is reconciling; skip this tick silently.
		return nil
	}

	releaseSess := PinnedSessionWithContext(sess, ctx)
	defer func() {
		_, _ = ReleaseSessionLock(releaseSess, lockName)
	}()

	return fn()
}
