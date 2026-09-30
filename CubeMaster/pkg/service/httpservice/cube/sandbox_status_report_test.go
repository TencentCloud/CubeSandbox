// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxstatus"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSandboxStatusBatchSeq(t *testing.T) {
	gin.SetMode(gin.TestMode)
	name := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	client, err := gorm.Open(sqlite.Open(name), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, client.AutoMigrate(&models.SandboxStatus{}))
	require.NoError(t, sandboxstatus.Init(client))

	engine := gin.New()
	RegisterInternalSandboxStatusRoutes(engine.Group(""))

	body := `{"host_id":"host-a","items":[{"sandbox_id":"sb-1","restart_state":"BackOff","restart_policy":"RESTART_POLICY_ON_FAILURE","status_seq":2,"restart_count":1}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/sandbox-status:batch", bytes.NewReader([]byte(body)))
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := sandboxstatus.Get(context.Background(), "sb-1")
	require.NoError(t, err)
	require.Equal(t, "host-a", got.HostID)
	require.Equal(t, "BackOff", got.RestartState)
	require.Equal(t, int64(2), got.StatusSeq)

	stale := `{"items":[{"sandbox_id":"sb-1","host_id":"host-stale","restart_state":"Running","status_seq":1}]}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/sandbox-status:batch", bytes.NewReader([]byte(stale)))
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got, err = sandboxstatus.Get(context.Background(), "sb-1")
	require.NoError(t, err)
	require.Equal(t, "BackOff", got.RestartState)

	require.NoError(t, sandboxstatus.SoftDelete(context.Background(), "sb-1"))
	late := `{"items":[{"sandbox_id":"sb-1","restart_state":"Running","status_seq":9}]}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/sandbox-status:batch", bytes.NewReader([]byte(late)))
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	_, err = sandboxstatus.Get(context.Background(), "sb-1")
	require.ErrorIs(t, err, sandboxstatus.ErrNotFound)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
}
