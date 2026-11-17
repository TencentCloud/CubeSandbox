// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// errPassedReadyGuard is returned by the stubbed active-job lookup, which
// runs right after the READY guard. Seeing it proves the guard let the
// request through.
var errPassedReadyGuard = errors.New("passed READY guard")

func newRedoGuardTestDB(t *testing.T, status string) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}, &models.TemplateImageJob{}))
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID: "tpl-guard",
		Status:     status,
	}).Error)
	return db
}

func submitRedoForGuardTest(failedOnly bool, scope []string) error {
	_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-guard"},
		TemplateID:        "tpl-guard",
		FailedOnly:        failedOnly,
		DistributionScope: scope,
	}, "http://master.example")
	return err
}

func countRedoGuardJobs(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&models.TemplateImageJob{}).Where("template_id = ?", "tpl-guard").Count(&n).Error)
	return n
}

// TestSubmitRedoTemplateFromImage_RejectsNonFailedOnlyRedoOnReadyTemplate
// verifies that a full or node-scoped redo on a READY template is rejected
// before any job row is written, so a later replica persistence failure
// cannot downgrade a working template to FAILED.
func TestSubmitRedoTemplateFromImage_RejectsNonFailedOnlyRedoOnReadyTemplate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope []string
	}{
		{name: "all nodes"},
		{name: "node scoped", scope: []string{"node-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newRedoGuardTestDB(t, StatusReady)

			err := submitRedoForGuardTest(false, tc.scope)

			require.ErrorIs(t, err, ErrTemplateNotReady)
			require.Contains(t, err.Error(), "--failed-only")
			require.Zero(t, countRedoGuardJobs(t, db), "rejected redo must not persist a job row")
		})
	}
}

// TestSubmitRedoTemplateFromImage_ReadyGuardAllowsOtherRedos verifies the
// guard only blocks non-failed-only redos on READY templates: failed-only
// redos on READY, and any redo on a non-READY template, proceed.
func TestSubmitRedoTemplateFromImage_ReadyGuardAllowsOtherRedos(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		failedOnly bool
	}{
		{name: "READY failed-only", status: StatusReady, failedOnly: true},
		{name: "PARTIALLY_READY all", status: StatusPartiallyReady},
		{name: "FAILED all", status: StatusFailed},
		{name: "PENDING all", status: StatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newRedoGuardTestDB(t, tc.status)
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(getActiveTemplateImageJobByTemplateIDTx, func(tx *gorm.DB, templateID string) (*models.TemplateImageJob, error) {
				return nil, errPassedReadyGuard
			})

			err := submitRedoForGuardTest(tc.failedOnly, nil)

			require.ErrorIs(t, err, errPassedReadyGuard)
			require.NotErrorIs(t, err, ErrTemplateNotReady)
		})
	}
}
