// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func newMigrateTestContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/cube/template/migrate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(CubeLog.WithRequestTrace(req.Context(), &CubeLog.RequestTrace{}))
	c.Request = req
	return c, w
}

func newMigrateStatusTestContext(t *testing.T, jobID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	url := "/cube/template/migrate?job_id=" + jobID
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(CubeLog.WithRequestTrace(req.Context(), &CubeLog.RequestTrace{}))
	c.Request = req
	return c, w
}

func TestHandleTemplateMigrateAction_ResolvesAliasIdentifier(t *testing.T) {
	origResolve := resolveTemplateIdentifierFn
	origSubmit := submitTemplateMigrateFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolve
		submitTemplateMigrateFn = origSubmit
	})

	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		if identifier != "stable-alias" {
			t.Fatalf("resolve identifier=%q, want stable-alias", identifier)
		}
		return "tpl-resolved", nil
	}
	submitTemplateMigrateFn = func(ctx context.Context, templateID, requestID string) (*types.TemplateImageJobInfo, error) {
		if templateID != "tpl-resolved" {
			t.Fatalf("submit templateID=%q, want tpl-resolved", templateID)
		}
		if requestID != "req-1" {
			t.Fatalf("submit requestID=%q, want req-1", requestID)
		}
		return &types.TemplateImageJobInfo{
			JobID:      "job-1",
			TemplateID: "tpl-resolved",
			ArtifactID: "rfs-1",
			Operation:  templatecenter.JobOperationMigrate,
			Status:     templatecenter.JobStatusPending,
			Phase:      templatecenter.JobPhaseMigratingArtifact,
		}, nil
	}

	c, w := newMigrateTestContext(t, `{"requestID":"req-1","template_id":"stable-alias"}`)
	handleTemplateMigrateAction(c)

	require.Equal(t, http.StatusOK, w.Code)
	resp := &migrateTemplateResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), resp), "body=%s", w.Body.String())
	require.NotNil(t, resp.Ret)
	assert.Equal(t, int(errorcode.ErrorCode_Success), resp.Ret.RetCode)
	require.NotNil(t, resp.Job)
	assert.Equal(t, "job-1", resp.Job.JobID)
	assert.Equal(t, "tpl-resolved", resp.Job.TemplateID)
}

func TestHandleTemplateMigrateAction_ResolveAliasNotFound(t *testing.T) {
	origResolve := resolveTemplateIdentifierFn
	origSubmit := submitTemplateMigrateFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolve
		submitTemplateMigrateFn = origSubmit
	})

	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return "", templatecenter.ErrTemplateNotFound
	}
	submitCalled := false
	submitTemplateMigrateFn = func(ctx context.Context, templateID, requestID string) (*types.TemplateImageJobInfo, error) {
		submitCalled = true
		return nil, nil
	}

	c, w := newMigrateTestContext(t, `{"requestID":"req-2","template_id":"missing-alias"}`)
	handleTemplateMigrateAction(c)

	require.Equal(t, http.StatusOK, w.Code)
	resp := &migrateTemplateResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), resp), "body=%s", w.Body.String())
	require.NotNil(t, resp.Ret)
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), resp.Ret.RetCode)
	assert.False(t, submitCalled, "submit must not be called when identifier resolution fails")
}

func TestGetTemplateMigrateStatusAction_Success(t *testing.T) {
	origGet := getTemplateMigrateJobInfoFn
	t.Cleanup(func() { getTemplateMigrateJobInfoFn = origGet })

	getTemplateMigrateJobInfoFn = func(ctx context.Context, jobID string) (*types.TemplateImageJobInfo, error) {
		if jobID != "job-1" {
			t.Fatalf("unexpected job id: %s", jobID)
		}
		return &types.TemplateImageJobInfo{
			JobID:      "job-1",
			TemplateID: "tpl-1",
			Operation:  templatecenter.JobOperationMigrate,
			Status:     templatecenter.JobStatusRunning,
			Phase:      templatecenter.JobPhaseMigratingArtifact,
			Progress:   60,
		}, nil
	}

	c, w := newMigrateStatusTestContext(t, "job-1")
	getTemplateMigrateStatusAction(c)

	require.Equal(t, http.StatusOK, w.Code)
	resp := &migrateTemplateResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), resp), "body=%s", w.Body.String())
	require.NotNil(t, resp.Ret)
	assert.Equal(t, int(errorcode.ErrorCode_Success), resp.Ret.RetCode)
	require.NotNil(t, resp.Job)
	assert.Equal(t, int32(60), resp.Job.Progress)
}
