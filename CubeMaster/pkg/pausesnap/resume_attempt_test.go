// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pausesnap

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func withResumeDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "pause.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.PauseSnapshotRecord{}))
	require.NoError(t, db.Create(&models.PauseSnapshotRecord{
		SnapshotID: "snap-a", SandboxID: "sb-a", NodeIP: "10.0.0.1", Status: StatusReady, Backend: "xfs-s3",
	}).Error)
	prev := getDB()
	Init(db)
	t.Cleanup(func() { Init(prev) })
}

func TestResumeAttemptPinsUntilTheTargetFails(t *testing.T) {
	withResumeDB(t)
	ctx := context.Background()

	rec, err := GetBySnapshotID(ctx, "snap-a")
	require.NoError(t, err)
	require.Empty(t, PinnedResumeTarget(rec), "a fresh binding is not pinned")

	first, err := BeginResumeAttempt(ctx, "snap-a", "10.0.0.2")
	require.NoError(t, err)
	require.Equal(t, 1, first.N)
	rec, _ = GetBySnapshotID(ctx, "snap-a")
	require.Equal(t, "10.0.0.2", PinnedResumeTarget(rec))

	// A retry after a lost reply goes to the same node again.
	retry, err := BeginResumeAttempt(ctx, "snap-a", "10.0.0.2")
	require.NoError(t, err)
	require.Equal(t, 2, retry.N)
	// Any other node is refused while the attempt is unresolved.
	_, err = BeginResumeAttempt(ctx, "snap-a", "10.0.0.3")
	require.True(t, errors.Is(err, ErrResumePinned), "got %v", err)

	// Another node's failure does not release the pin.
	require.NoError(t, FailResumeAttempt(ctx, "snap-a", &ResumeAttempt{Target: "10.0.0.3", N: retry.N}))
	rec, _ = GetBySnapshotID(ctx, "snap-a")
	require.Equal(t, "10.0.0.2", PinnedResumeTarget(rec))

	// The pinned node answering with a failure releases it.
	require.NoError(t, FailResumeAttempt(ctx, "snap-a", retry))
	rec, _ = GetBySnapshotID(ctx, "snap-a")
	require.Empty(t, PinnedResumeTarget(rec))
	next, err := BeginResumeAttempt(ctx, "snap-a", "10.0.0.3")
	require.NoError(t, err)
	require.Equal(t, 3, next.N)
	rec, _ = GetBySnapshotID(ctx, "snap-a")
	require.Equal(t, "10.0.0.3", PinnedResumeTarget(rec))
}

func TestStaleFailureCannotReleaseALaterAttempt(t *testing.T) {
	// Master A lost its lock while attempt 1 ran on 10.0.0.2; Master B sent
	// attempt 2 to the same node, which is now starting the sandbox. A's
	// late failure reply for attempt 1 must not release attempt 2, or the
	// next Resume could start the sandbox on a second node.
	withResumeDB(t)
	ctx := context.Background()
	a1, err := BeginResumeAttempt(ctx, "snap-a", "10.0.0.2")
	require.NoError(t, err)
	a2, err := BeginResumeAttempt(ctx, "snap-a", "10.0.0.2")
	require.NoError(t, err)

	require.NoError(t, FailResumeAttempt(ctx, "snap-a", a1))
	rec, _ := GetBySnapshotID(ctx, "snap-a")
	require.Equal(t, "10.0.0.2", PinnedResumeTarget(rec), "attempt 1 cannot release attempt 2")
	_, err = BeginResumeAttempt(ctx, "snap-a", "10.0.0.3")
	require.True(t, errors.Is(err, ErrResumePinned), "got %v", err)

	require.NoError(t, FailResumeAttempt(ctx, "snap-a", a2))
	rec, _ = GetBySnapshotID(ctx, "snap-a")
	require.Empty(t, PinnedResumeTarget(rec))
}

func TestResumeAttemptOnMissingBinding(t *testing.T) {
	withResumeDB(t)
	_, err := BeginResumeAttempt(context.Background(), "snap-missing", "10.0.0.2")
	require.True(t, errors.Is(err, ErrNotFound), "got %v", err)
}
