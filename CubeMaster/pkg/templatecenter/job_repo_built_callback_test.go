// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newBuiltCallbackTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateImageJob{}))
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	return db
}

func TestBuiltCallbackDoesNotMoveDistributionBackToBuilt(t *testing.T) {
	db := newBuiltCallbackTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID: "job-distributing", Status: JobStatusRunning, Phase: JobPhaseDistributing,
	}).Error)

	applied, err := ApplyTemplateImageJobBuiltReport(context.Background(), "job-distributing", map[string]any{
		"status": JobStatusBuilt,
		"phase":  JobPhaseReady,
	})
	require.NoError(t, err)
	require.False(t, applied)

	job, err := getTemplateImageJobRecordByID(context.Background(), "job-distributing")
	require.NoError(t, err)
	require.Equal(t, JobStatusRunning, job.Status)
	require.Equal(t, JobPhaseDistributing, job.Phase)
}

func TestBuiltCallbackContinuesWhenMatchedUpdateReportsZeroRows(t *testing.T) {
	db := newBuiltCallbackTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID: "job-built-retry", Status: JobStatusBuilt, Phase: JobPhaseReady,
	}).Error)
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:force_zero_rows", func(tx *gorm.DB) {
		tx.RowsAffected = 0
	}))

	applied, err := ApplyTemplateImageJobBuiltReport(context.Background(), "job-built-retry", map[string]any{
		"status": JobStatusBuilt,
		"phase":  JobPhaseReady,
	})
	require.NoError(t, err)
	require.True(t, applied)
}

func TestBuiltJobDistributionCanOnlyBeClaimedOnce(t *testing.T) {
	db := newBuiltCallbackTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID: "job-built", Status: JobStatusBuilt, Phase: JobPhaseReady,
	}).Error)

	values := map[string]any{"status": JobStatusRunning, "phase": JobPhaseDistributing}
	claimed, err := claimTemplateImageJobDistribution(context.Background(), "job-built", values)
	require.NoError(t, err)
	require.True(t, claimed)

	claimed, err = claimTemplateImageJobDistribution(context.Background(), "job-built", values)
	require.NoError(t, err)
	require.False(t, claimed)
}
