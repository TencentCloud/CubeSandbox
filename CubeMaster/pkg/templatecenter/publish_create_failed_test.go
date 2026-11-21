// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newPublishCreateFailedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}, &models.TemplateImageJob{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldDB := store.db
	store.db = db
	t.Cleanup(func() {
		store.db = oldDB
		_ = sqlDB.Close()
	})
	return db
}

func definitionStatus(t *testing.T, db *gorm.DB, templateID string) (string, string) {
	t.Helper()
	def := &models.TemplateDefinition{}
	require.NoError(t, db.Where("template_id = ?", templateID).First(def).Error)
	return def.Status, def.LastError
}

// The job that hit the persist error is still the template's active CREATE:
// the definition is published FAILED.
func TestPublishCreateFailedIfJobActive_ActiveCreatePublishes(t *testing.T) {
	db := newPublishCreateFailedTestDB(t)
	require.NoError(t, db.Create(&models.TemplateDefinition{TemplateID: "tpl-1", Status: StatusPending}).Error)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID: "job-old", TemplateID: "tpl-1", Operation: JobOperationCreate, Status: JobStatusRunning,
	}).Error)

	published, err := publishCreateFailedIfJobActive(context.Background(), "tpl-1", "job-old", "sanitized")
	require.NoError(t, err)
	require.True(t, published)
	status, lastErr := definitionStatus(t, db, "tpl-1")
	require.Equal(t, StatusFailed, status)
	require.Equal(t, "sanitized", lastErr)
}

// Force delete + re-create under the same template_id while the old job was
// still finishing: the old job row is gone (or terminal), so the new READY
// generation must not be demoted.
func TestPublishCreateFailedIfJobActive_DoesNotDemoteNewGeneration(t *testing.T) {
	cases := []struct {
		name   string
		oldJob *models.TemplateImageJob
	}{
		{name: "old job row deleted"},
		{name: "old job terminal", oldJob: &models.TemplateImageJob{
			JobID: "job-old", TemplateID: "tpl-1", Operation: JobOperationCreate, Status: JobStatusFailed,
		}},
		{name: "old job not a create", oldJob: &models.TemplateImageJob{
			JobID: "job-old", TemplateID: "tpl-1", Operation: JobOperationRedo, Status: JobStatusRunning,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newPublishCreateFailedTestDB(t)
			require.NoError(t, db.Create(&models.TemplateDefinition{TemplateID: "tpl-1", Status: StatusReady}).Error)
			require.NoError(t, db.Create(&models.TemplateImageJob{
				JobID: "job-new", TemplateID: "tpl-1", Operation: JobOperationCreate, Status: JobStatusReady,
			}).Error)
			if tc.oldJob != nil {
				require.NoError(t, db.Create(tc.oldJob).Error)
			}

			published, err := publishCreateFailedIfJobActive(context.Background(), "tpl-1", "job-old", "sanitized")
			require.NoError(t, err)
			require.False(t, published)
			status, lastErr := definitionStatus(t, db, "tpl-1")
			require.Equal(t, StatusReady, status)
			require.Empty(t, lastErr)
		})
	}
}

// Defensive guard only: no current writer sets a definition to DELETING. The
// test pins the guard's behaviour (not found, row untouched) in case one does.
func TestPublishCreateFailedIfJobActive_DeletingDefinition(t *testing.T) {
	db := newPublishCreateFailedTestDB(t)
	require.NoError(t, db.Create(&models.TemplateDefinition{TemplateID: "tpl-1", Status: StatusDeleting}).Error)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID: "job-old", TemplateID: "tpl-1", Operation: JobOperationCreate, Status: JobStatusRunning,
	}).Error)

	published, err := publishCreateFailedIfJobActive(context.Background(), "tpl-1", "job-old", "sanitized")
	require.True(t, errors.Is(err, ErrTemplateNotFound), "err = %v", err)
	require.False(t, published)
	status, _ := definitionStatus(t, db, "tpl-1")
	require.Equal(t, StatusDeleting, status)
}

// Definition row already gone: ErrTemplateNotFound, which the caller suppresses.
func TestPublishCreateFailedIfJobActive_MissingDefinition(t *testing.T) {
	newPublishCreateFailedTestDB(t)

	published, err := publishCreateFailedIfJobActive(context.Background(), "tpl-gone", "job-old", "sanitized")
	require.True(t, errors.Is(err, ErrTemplateNotFound), "err = %v", err)
	require.False(t, published)
}
