// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func TestSnapshotJobClaimSQLite(t *testing.T) {
	db := newBuiltCallbackTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{JobID: "snapshot-claim", Status: JobStatusPending}).Error)
	claimed, err := claimSnapshotJobExecution(context.Background(), "snapshot-claim", JobPhaseRollbackDriving, 25)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = claimSnapshotJobExecution(context.Background(), "snapshot-claim", JobPhaseRollbackDriving, 25)
	require.NoError(t, err)
	require.False(t, claimed)
	query := templateImageJobWriteTable(db.Session(&gorm.Session{DryRun: true})).
		Where("job_id = ? AND status = ?", "snapshot-claim", JobStatusPending).
		Updates(map[string]any{"status": JobStatusRunning})
	require.NoError(t, query.Error)
	require.NotContains(t, query.Statement.SQL.String(), "FORCE INDEX")
}

// Use the production migrations and a disposable database: the index choice
// caused cross-job deadlocks that an in-memory SQLite test cannot reproduce.
func TestSnapshotJobClaimsMySQL(t *testing.T) {
	env := newMySQLDockerEnv(t)
	defer env.teardown()
	db := openMigratedMySQLGORM(t, env)
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetMaxIdleConns(100)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const count = 100
	jobs := make([]models.TemplateImageJob, count)
	for i := range jobs {
		jobs[i] = models.TemplateImageJob{
			JobID: fmt.Sprintf("claim-%03d", i), TemplateID: fmt.Sprintf("template-%03d", i),
			RequestID: fmt.Sprintf("request-%03d", i), AttemptNo: 1,
			Operation: JobOperationSnapshotRollback, Status: JobStatusPending,
		}
	}
	require.NoError(t, db.Create(&jobs).Error)

	query := templateImageJobWriteTable(db.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true})).
		Where("job_id = ? AND status = ?", jobs[0].JobID, JobStatusPending).
		Updates(map[string]any{"status": JobStatusRunning, "phase": JobPhaseRollbackDriving})
	require.NoError(t, query.Error)
	require.Contains(t, query.Statement.SQL.String(), "FORCE INDEX (idx_template_image_job_id)")
	var plan []struct {
		Type string `gorm:"column:type"`
		Key  string `gorm:"column:key"`
	}
	require.NoError(t, db.Raw("EXPLAIN "+query.Statement.SQL.String(), query.Statement.Vars...).Scan(&plan).Error)
	require.Len(t, plan, 1)
	require.Equal(t, "idx_template_image_job_id", plan[0].Key)
	require.NotEqual(t, "index_merge", plan[0].Type)
	t.Logf("claim access path: type=%s key=%s", plan[0].Type, plan[0].Key)

	run := func(ids []string, expected int) {
		t.Helper()
		type result struct {
			claimed bool
			err     error
		}
		results := make(chan result, len(ids))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start
				ok, err := claimSnapshotJobExecution(ctx, id, JobPhaseRollbackDriving, 25)
				results <- result{ok, err}
			}(id)
		}
		close(start)
		wg.Wait()
		close(results)
		claimed := 0
		for r := range results {
			require.NoError(t, r.err)
			if r.claimed {
				claimed++
			}
		}
		require.Equal(t, expected, claimed)
	}
	ids := make([]string, count)
	for i := range jobs {
		ids[i] = jobs[i].JobID
	}
	for round := 0; round < 10; round++ {
		require.NoError(t, db.Table(constants.TemplateImageJobTableName).
			Where("job_id IN ?", ids).Update("status", JobStatusPending).Error)
		run(ids, count)
	}
	// Concurrent claimants for the same job must still have exactly one winner.
	require.NoError(t, templateImageJobWriteTable(db).Where("job_id = ?", ids[0]).Update("status", JobStatusPending).Error)
	for i := range ids {
		ids[i] = jobs[0].JobID
	}
	run(ids, 1)
	var status string
	require.NoError(t, db.Table(constants.TemplateImageJobTableName).Select("status").Where("job_id = ?", ids[0]).Scan(&status).Error)
	require.Equal(t, JobStatusRunning, strings.TrimSpace(status))
}
