// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxevent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSandboxEventsAction(t *testing.T) {
	client, err := gorm.Open(sqlite.Open("file:sandbox_events_action?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, client.AutoMigrate(&models.SandboxEvent{}))
	require.NoError(t, sandboxevent.Init(client))

	ctx := context.Background()
	sandboxevent.Record(ctx, "sb-gone", sandboxevent.EventCreated, "tpl-1", "")
	sandboxevent.Record(ctx, "sb-gone", sandboxevent.EventPaused, "", "")
	sandboxevent.Record(ctx, "sb-gone", sandboxevent.EventKilled, "", "request")
	require.Eventually(t, func() bool {
		rows, _, err := sandboxevent.List(ctx, "sb-gone", 0, 0)
		return err == nil && len(rows) == 3
	}, 2*time.Second, 10*time.Millisecond)

	engine := gin.New()
	engine.GET("/cube"+SandboxEventsAction, handleSandboxEventsAction)
	engine.POST("/cube"+SandboxEventsAction, handleSandboxEventsAction)
	call := func(req *http.Request) SandboxEventsRes {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		var res SandboxEventsRes
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
		return res
	}

	// The sandbox no longer exists anywhere; its history is still served.
	first := call(httptest.NewRequest(http.MethodPost, "/cube/sandbox/events",
		bytes.NewReader([]byte(`{"sandboxID":"sb-gone","limit":2}`))))
	require.NotNil(t, first.Res)
	assert.Equal(t, 0, first.Ret.RetCode)
	require.Len(t, first.Events, 2)
	assert.Equal(t, "created", first.Events[0].Event)
	assert.Equal(t, "tpl-1", first.Events[0].TemplateID)
	assert.True(t, first.HasMore)
	assert.Equal(t, first.Events[1].ID, first.NextCursor)

	rest := call(httptest.NewRequest(http.MethodGet,
		"/cube/sandbox/events?sandbox_id=sb-gone&cursor="+jsonUint(first.NextCursor), nil))
	require.Len(t, rest.Events, 1)
	assert.Equal(t, "killed", rest.Events[0].Event)
	assert.Equal(t, "request", rest.Events[0].Reason)
	assert.False(t, rest.HasMore)

	// A short ID of a live sandbox resolves to the full ID the events are stored under.
	orig := resolveSandboxIDFn
	t.Cleanup(func() { resolveSandboxIDFn = orig })
	resolveSandboxIDFn = func(_ context.Context, id string) (string, error) {
		if id == "sb-g" {
			return "sb-gone", nil
		}
		return "", errors.New("not found")
	}
	short := call(httptest.NewRequest(http.MethodGet, "/cube/sandbox/events?sandbox_id=sb-g", nil))
	assert.Len(t, short.Events, 3)
	// A full ID with no history is never resolved (that would scan the cluster).
	resolveCalls := 0
	resolveSandboxIDFn = func(context.Context, string) (string, error) {
		resolveCalls++
		return "", errors.New("not found")
	}
	fullID := strings.Repeat("a", 32)
	require.True(t, sandboxid.IsFullID(fullID))
	full := call(httptest.NewRequest(http.MethodGet, "/cube/sandbox/events?sandbox_id="+fullID, nil))
	assert.Empty(t, full.Events)
	assert.Equal(t, 0, resolveCalls)

	unknown := call(httptest.NewRequest(http.MethodGet, "/cube/sandbox/events?sandbox_id=sb-none", nil))
	assert.Equal(t, 0, unknown.Ret.RetCode)
	assert.Empty(t, unknown.Events)

	missing := call(httptest.NewRequest(http.MethodGet, "/cube/sandbox/events", nil))
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), missing.Ret.RetCode)
}

func jsonUint(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
