// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxevent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newTestDB attaches an in-memory store. The writer goroutine started by the
// first Init keeps running for the test binary and follows the swapped handle.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	client, err := gorm.Open(sqlite.Open(name), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, client.AutoMigrate(&models.SandboxEvent{}))
	require.NoError(t, Init(client))
	t.Cleanup(func() {
		mu.Lock()
		db = nil
		mu.Unlock()
	})
	return client
}

func waitForEvents(t *testing.T, sandboxID string, n int) []models.SandboxEvent {
	t.Helper()
	var rows []models.SandboxEvent
	require.Eventually(t, func() bool {
		var err error
		rows, _, err = List(context.Background(), sandboxID, 0, 0)
		return err == nil && len(rows) == n
	}, 2*time.Second, 10*time.Millisecond)
	return rows
}

func TestRecordKeepsOrderAndFields(t *testing.T) {
	newTestDB(t)
	ctx := context.Background()
	Record(ctx, "sb-1", EventCreated, "tpl-1", "")
	Record(ctx, "sb-other", EventCreated, "", "")
	Record(ctx, "sb-1", EventPaused, "", "")
	Record(ctx, "sb-1", EventResumed, "", "")
	Record(ctx, "sb-1", EventKilled, "", "timeout")

	rows := waitForEvents(t, "sb-1", 4)
	events := make([]string, 0, len(rows))
	for _, row := range rows {
		events = append(events, row.Event)
	}
	assert.Equal(t, []string{EventCreated, EventPaused, EventResumed, EventKilled}, events)
	assert.Equal(t, "tpl-1", rows[0].TemplateID)
	assert.Equal(t, "timeout", rows[3].Reason)
	assert.False(t, rows[0].CreatedAt.IsZero())
}

func TestListPagesByCursor(t *testing.T) {
	newTestDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		Record(ctx, "sb-page", EventPaused, "", "")
	}
	all := waitForEvents(t, "sb-page", 5)

	first, more, err := List(ctx, "sb-page", 0, 2)
	require.NoError(t, err)
	assert.True(t, more)
	assert.Equal(t, all[:2], first)

	rest, more, err := List(ctx, "sb-page", first[1].ID, 10)
	require.NoError(t, err)
	assert.False(t, more)
	assert.Equal(t, all[2:], rest)
}

func TestListUnknownSandboxIsEmpty(t *testing.T) {
	newTestDB(t)
	rows, more, err := List(context.Background(), "sb-none", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.False(t, more)
}

func TestRecordIgnoresBlankInput(t *testing.T) {
	newTestDB(t)
	ctx := context.Background()
	Record(ctx, "  ", EventCreated, "", "")
	Record(ctx, "sb-blank", "", "", "")
	Record(ctx, "sb-blank", EventCreated, "", "")
	waitForEvents(t, "sb-blank", 1)
}

func TestListWithoutInitIsNotReady(t *testing.T) {
	mu.Lock()
	db = nil
	mu.Unlock()
	_, _, err := List(context.Background(), "sb", 0, 0)
	assert.ErrorIs(t, err, ErrNotReady)
	// Recording without a store is a silent no-op.
	Record(context.Background(), "sb", EventCreated, "", "")
}

func TestKillReasonRoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", KillReason(ctx))
	assert.Equal(t, ctx, WithKillReason(ctx, ""))
	assert.Equal(t, "timeout", KillReason(WithKillReason(ctx, "timeout")))
}
