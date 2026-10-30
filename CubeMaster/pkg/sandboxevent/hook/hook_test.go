// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package hook

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxevent"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestHooksRecordLifecycle(t *testing.T) {
	client, err := gorm.Open(sqlite.Open("file:hook_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, client.AutoMigrate(&models.SandboxEvent{}))
	require.NoError(t, sandboxevent.Init(client))

	ctx := context.Background()
	req := &sandboxtypes.CreateCubeSandboxReq{}
	req.Annotations = map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1"}
	require.NoError(t, onAfterCreate(ctx, "sb-1", "host", "10.0.0.1", req))
	onAfterUpdate(ctx, "sb-1", "cubebox", "pause", "req-1")
	onAfterUpdate(ctx, "sb-1", "cubebox", "resume", "req-2")
	onAfterUpdate(ctx, "sb-1", "cubebox", "bogus", "req-3")
	require.NoError(t, onAfterDestroy(sandboxevent.WithKillReason(ctx, "timeout"), "sb-1"))

	var rows []models.SandboxEvent
	require.Eventually(t, func() bool {
		rows, _, err = sandboxevent.List(ctx, "sb-1", 0, 0)
		return err == nil && len(rows) == 4
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, sandboxevent.EventCreated, rows[0].Event)
	assert.Equal(t, "tpl-1", rows[0].TemplateID)
	assert.Equal(t, sandboxevent.EventPaused, rows[1].Event)
	assert.Equal(t, sandboxevent.EventResumed, rows[2].Event)
	assert.Equal(t, sandboxevent.EventKilled, rows[3].Event)
	assert.Equal(t, "timeout", rows[3].Reason)
}

func TestInitSurvivesUnopenedDatabase(t *testing.T) {
	// db.Init panics when the shared handle was never opened; Init must not.
	assert.NotPanics(t, func() { Init(context.Background()) })
}
