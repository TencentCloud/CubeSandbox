// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func TestReconcileUnclaimedSnapshotJobs(t *testing.T) {
	db := newBuiltCallbackTestDB(t)
	old := time.Now().Add(-2 * snapshotOperationTimeout)
	jobs := []models.TemplateImageJob{
		{JobID: "create", Operation: JobOperationSnapshotCreate, Status: JobStatusPending, Model: gorm.Model{UpdatedAt: old}},
		{JobID: "rollback", Operation: JobOperationSnapshotRollback, Status: JobStatusPending, Model: gorm.Model{UpdatedAt: old}},
		{JobID: "delete", Operation: JobOperationSnapshotDelete, Status: JobStatusPending, Model: gorm.Model{UpdatedAt: old}},
		{JobID: "running", Operation: JobOperationSnapshotRollback, Status: JobStatusRunning, Model: gorm.Model{UpdatedAt: old}},
		{JobID: "fresh", Operation: JobOperationSnapshotRollback, Status: JobStatusPending},
		{JobID: "other", Operation: JobOperationCreate, Status: JobStatusPending, Model: gorm.Model{UpdatedAt: old}},
	}
	require.NoError(t, db.Create(&jobs).Error)
	require.NoError(t, reconcileUnclaimedSnapshotJobs(context.Background()))
	for _, job := range jobs {
		got, err := getTemplateImageJobRecordByID(context.Background(), job.JobID)
		require.NoError(t, err)
		if job.JobID == "create" || job.JobID == "rollback" || job.JobID == "delete" {
			require.Equal(t, JobStatusFailed, got.Status)
			require.Contains(t, got.ErrorMessage, "no engine operation was started")
		} else {
			require.Equal(t, job.Status, got.Status)
		}
	}
}

func TestReconcileUnclaimedSnapshotJobLosesToConcurrentClaim(t *testing.T) {
	db := newBuiltCallbackTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{JobID: "raced", Operation: JobOperationSnapshotRollback,
		Status: JobStatusPending, Model: gorm.Model{UpdatedAt: time.Now().Add(-2 * snapshotOperationTimeout)}}).Error)
	// Simulate a competing master claiming the row after the scan but before
	// the sweeper's conditional UPDATE. Its newer state must win.
	const callback = "test:claim_before_sweep_update"
	fired := false
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if !fired && tx.Statement.Table == constants.TemplateImageJobTableName {
			fired = true
			require.NoError(t, db.Table(constants.TemplateImageJobTableName).Where("job_id = ?", "raced").Update("status", JobStatusRunning).Error)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	require.NoError(t, reconcileUnclaimedSnapshotJobs(context.Background()))
	got, err := getTemplateImageJobRecordByID(context.Background(), "raced")
	require.NoError(t, err)
	require.Equal(t, JobStatusRunning, got.Status)
}
