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

// errPassedSubmitStatusCheck is returned by the stubbed active-job lookup,
// which runs right after the definition is locked. Seeing it proves the
// submission was not rejected because of the template status.
var errPassedSubmitStatusCheck = errors.New("passed submit status check")

func newRedoSubmitTestDB(t *testing.T, status string) {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}, &models.TemplateImageJob{}))
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID: "tpl-redo-submit",
		Status:     status,
	}).Error)
}

// TestSubmitRedoTemplateFromImage_AcceptsRedoRegardlessOfTemplateStatus
// guards the documented scale-out workflow (`tpl redo --template-id <id>
// --node <new-node>` on a healthy template): redo submission must not be
// rejected because the template is READY. A replica persistence failure is
// instead handled by publishRedoStatusAfterPersistErr, which never publishes
// FAILED while READY replicas remain.
func TestSubmitRedoTemplateFromImage_AcceptsRedoRegardlessOfTemplateStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		failedOnly bool
		scope      []string
	}{
		{name: "READY all nodes", status: StatusReady},
		{name: "READY node scoped", status: StatusReady, scope: []string{"10.0.4.7"}},
		{name: "READY failed-only", status: StatusReady, failedOnly: true},
		{name: "PARTIALLY_READY all nodes", status: StatusPartiallyReady},
		{name: "FAILED all nodes", status: StatusFailed},
		{name: "PENDING all nodes", status: StatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newRedoSubmitTestDB(t, tc.status)
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(getActiveTemplateImageJobByTemplateIDTx, func(tx *gorm.DB, templateID string) (*models.TemplateImageJob, error) {
				return nil, errPassedSubmitStatusCheck
			})

			_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
				Request:           &types.Request{RequestID: "req-redo-submit"},
				TemplateID:        "tpl-redo-submit",
				FailedOnly:        tc.failedOnly,
				DistributionScope: tc.scope,
			}, "http://master.example")

			require.ErrorIs(t, err, errPassedSubmitStatusCheck)
			require.NotErrorIs(t, err, ErrTemplateNotReady)
		})
	}
}
