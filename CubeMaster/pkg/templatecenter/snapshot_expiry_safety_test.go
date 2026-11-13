// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"gorm.io/gorm"
)

func TestSnapshotExpiryStatePrecedence(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	for _, tc := range []struct {
		status string
		want   error
	}{
		{StatusCreating, ErrTemplateAttemptInProgress}, {StatusFailed, ErrTemplateAttemptInProgress},
		{StatusDeleting, ErrSnapshotNotFound}, {StatusDeleted, ErrSnapshotNotFound}, {StatusReady, ErrSnapshotExpired},
	} {
		t.Run(tc.status, func(t *testing.T) {
			require.ErrorIs(t, validateSnapshotNewUse(&models.SnapshotRecord{SnapshotID: "snap-state", Status: tc.status, ExpiresAt: &past}, now), tc.want)
		})
	}
}

func TestSnapshotTTLReadFailureStopsCapture(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		readErr       error
	}{
		{"database", "", errors.New("database unavailable")}, {"malformed", "{", nil},
		{"missing_ttl", "{}", nil}, {"negative", `{"ttl_seconds":-1}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := gomonkey.NewPatches()
			t.Cleanup(p.Reset)
			p.ApplyFunc(updateTemplateImageJob, func(context.Context, string, map[string]any) error { return nil })
			p.ApplyFunc(getSnapshotRecord, func(context.Context, string) (*models.SnapshotRecord, error) {
				past := time.Now().Add(-time.Hour)
				return &models.SnapshotRecord{SnapshotID: "snap-ttl", ExpiresAt: &past}, nil
			})
			p.ApplyFunc(getTemplateImageJobRecordByID, func(context.Context, string) (*models.TemplateImageJob, error) {
				return &models.TemplateImageJob{RequestJSON: tc.payload}, tc.readErr
			})
			p.ApplyFunc(cubelet.CommitSandbox, func(context.Context, string, *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
				t.Fatal("TTL failure must stop before the capture RPC")
				return nil, nil
			})
			p.ApplyFunc(failSnapshotCreateJob, func(_ context.Context, _, _, nodeIP, _ string, _ *cubeboxv1.CommitSandboxResponse, cause error, _ string) error {
				require.Empty(t, nodeIP, "no node cleanup before capture")
				return cause
			})
			err := runSnapshotCreateJob(context.Background(), "job-ttl", "sb", "node", "", &sandboxtypes.CreateCubeSandboxReq{
				Annotations: map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "snap-ttl"},
			}, &sandboxtypes.CreateCubeSandboxReq{})
			require.Error(t, err)
		})
	}
}

func snapshotReservationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.SnapshotRecord{}, &models.TemplateImageJob{}, &models.SnapshotRuntimeActive{}, &models.SnapshotRuntimeRef{}))
	return db
}

func TestSnapshotCreateReservationFencesCleanup(t *testing.T) {
	db := snapshotReservationTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: "snap-pin", Status: StatusReady}).Error)
	require.NoError(t, ReserveSnapshotCreate(ctx, "snap-pin", "sb-pin", "node", ""))
	require.NoError(t, tombstoneSnapshotRecord(ctx, "snap-pin"))
	job, err := beginTombstonePhysicalCleanup(ctx, "snap-pin")
	require.NoError(t, err)
	require.Empty(t, job)
	// Final registration converts the durable pin without exposing a zero-ref gap.
	require.NoError(t, AttachSnapshotRuntimeBinding(ctx, SnapshotRuntimeRefInfo{SnapshotID: "snap-pin", SandboxID: "sb-pin", NodeID: "node"}, "created"))
	var active []models.SnapshotRuntimeActive
	require.NoError(t, db.Find(&active).Error)
	require.Len(t, active, 1)
	require.Equal(t, SnapshotRuntimeBindingMemoryBacking, active[0].BindingType)
	require.NoError(t, DetachSnapshotRuntimeBinding(ctx, "sb-pin", "", "destroyed"))
	job, err = beginTombstonePhysicalCleanup(ctx, "snap-pin")
	require.NoError(t, err)
	require.NotEmpty(t, job)
	require.ErrorIs(t, ReserveSnapshotCreate(ctx, "snap-pin", "too-late", "node", ""), ErrSnapshotNotFound)
}

func TestSnapshotPendingCreateSurvivesIncompleteObservation(t *testing.T) {
	db := snapshotReservationTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: "snap-pending", Status: StatusReady}).Error)
	require.NoError(t, ReserveSnapshotCreate(ctx, "snap-pending", "sb-pending", "node", ""))
	require.NoError(t, RefreshSnapshotRuntimeRefsFromNode(ctx, "node", "", nil))
	n, err := CountActiveSnapshotRuntimeRefs(ctx, "snap-pending")
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.NoError(t, RefreshSnapshotRuntimeRefsFromNode(ctx, "node", "", []SnapshotRuntimeRefInfo{{SnapshotID: "snap-pending", SandboxID: "sb-pending"}}))
	var active []models.SnapshotRuntimeActive
	require.NoError(t, db.Find(&active).Error)
	require.Len(t, active, 1)
	require.Equal(t, SnapshotRuntimeBindingMemoryBacking, active[0].BindingType)
}

func TestSnapshotExpiryWaitsForAcceptedRollback(t *testing.T) {
	db := snapshotReservationTestDB(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: "snap-busy", Status: StatusReady, ExpiresAt: &past}).Error)
	require.NoError(t, db.Create(&models.TemplateImageJob{JobID: "rollback", ResourceID: "snap-busy", Operation: JobOperationSnapshotRollback, Status: JobStatusRunning}).Error)
	require.NoError(t, expireSnapshotRecord(ctx, "snap-busy", time.Now()))
	rec, err := getSnapshotRecord(ctx, "snap-busy")
	require.NoError(t, err)
	require.Equal(t, StatusReady, rec.Status)
	require.ErrorIs(t, EnsureSnapshotReadyForNewUse(ctx, "snap-busy"), ErrSnapshotExpired)
	require.NoError(t, db.Model(&models.TemplateImageJob{}).Where("job_id = ?", "rollback").Update("status", JobStatusReady).Error)
	require.NoError(t, expireSnapshotRecord(ctx, "snap-busy", time.Now()))
	rec, err = getSnapshotRecord(ctx, "snap-busy")
	require.NoError(t, err)
	require.Equal(t, StatusDeleted, rec.Status)
}
