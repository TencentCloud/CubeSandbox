// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The heartbeat decides whether a long upload survives the stale sweep, so the
// interval has to stay well inside the stale window: if a future change makes
// it longer, failStaleActiveTemplateMigrateJob starts failing live jobs again
// (and every such submit spawns a second, concurrent migration).
func TestMigrateJobHeartbeatFitsInsideStaleWindow(t *testing.T) {
	if migrateJobHeartbeatInterval <= 0 {
		t.Fatalf("migrateJobHeartbeatInterval must be positive, got %v", migrateJobHeartbeatInterval)
	}
	if migrateJobHeartbeatInterval*2 >= migrateJobStaleAfter {
		t.Fatalf("migrateJobHeartbeatInterval %v must stay far below migrateJobStaleAfter %v",
			migrateJobHeartbeatInterval, migrateJobStaleAfter)
	}
}

func TestMigrateAttemptNoMustAccountForAllTemplateJobs(t *testing.T) {
	latest := nextAttemptNoFromLatest(3)
	if latest != 4 {
		t.Fatalf("nextAttemptNoFromLatest(3)=%d, want 4", latest)
	}
}

func newMigrateAttemptTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}, &models.TemplateImageJob{}))
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_template_image_template_attempt ON "+constants.TemplateImageJobTableName+"(template_id, attempt_no)").Error)
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	return db
}

func TestInsertTemplateMigrateJobUsesGlobalAttemptNo(t *testing.T) {
	db := newMigrateAttemptTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID:      "job-create",
		TemplateID: "tpl-global-attempt",
		RequestID:  "req-create",
		AttemptNo:  1,
		Operation:  JobOperationCreate,
		Status:     JobStatusReady,
	}).Error)

	record := &models.TemplateImageJob{
		JobID:      "job-migrate",
		TemplateID: "tpl-global-attempt",
		RequestID:  "req-migrate",
		Operation:  JobOperationMigrate,
		Status:     JobStatusPending,
	}
	require.NoError(t, insertTemplateMigrateJobWithAttemptRetry(context.Background(), "tpl-global-attempt", record))
	require.Equal(t, int32(2), record.AttemptNo)
}

func TestInsertTemplateMigrateJobRetriesAfterConcurrentAttemptInsert(t *testing.T) {
	db := newMigrateAttemptTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID:      "job-create",
		TemplateID: "tpl-attempt-race",
		RequestID:  "req-create",
		AttemptNo:  1,
		Operation:  JobOperationCreate,
		Status:     JobStatusReady,
	}).Error)

	oldHook := beforeTemplateMigrateJobInsert
	conflictInserted := false
	beforeTemplateMigrateJobInsert = func(ctx context.Context, record *models.TemplateImageJob) {
		if conflictInserted {
			return
		}
		conflictInserted = true
		require.NoError(t, db.Create(&models.TemplateImageJob{
			JobID:      "job-concurrent-redo",
			TemplateID: "tpl-attempt-race",
			RequestID:  "req-concurrent-redo",
			AttemptNo:  2,
			Operation:  JobOperationRedo,
			Status:     JobStatusPending,
		}).Error)
	}
	t.Cleanup(func() { beforeTemplateMigrateJobInsert = oldHook })

	record := &models.TemplateImageJob{
		JobID:      "job-migrate",
		TemplateID: "tpl-attempt-race",
		RequestID:  "req-migrate",
		Operation:  JobOperationMigrate,
		Status:     JobStatusPending,
	}
	require.NoError(t, insertTemplateMigrateJobWithAttemptRetry(context.Background(), "tpl-attempt-race", record))
	require.True(t, conflictInserted, "test must insert the concurrent attempt")
	require.Equal(t, int32(3), record.AttemptNo)
}

// stop must return even though the next tick is a minute away and the row write
// it performs fails in a process without a store, and the goroutine must not
// write to the job row after the runner has returned.
func TestStartMigrateJobHeartbeatStopsWithTheRunner(t *testing.T) {
	stop := startMigrateJobHeartbeat(context.Background(), "job-heartbeat")
	done := make(chan struct{})
	go func() {
		defer close(done)
		stop()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startMigrateJobHeartbeat stop blocked: the goroutine never exited")
	}
}
