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

func newRedoTestContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/cube/template/redo", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(CubeLog.WithRequestTrace(req.Context(), &CubeLog.RequestTrace{}))
	c.Request = req
	return c, w
}

func TestHandleRedoTemplateAction_ResolvesAliasIdentifier(t *testing.T) {
	origResolve := resolveTemplateIdentifierFn
	origRedo := redoTemplateFromImageFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolve
		redoTemplateFromImageFn = origRedo
	})

	var resolvedInput string
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		resolvedInput = identifier
		return "tpl-resolved", nil
	}
	redoTemplateFromImageFn = func(ctx context.Context, req *types.RedoTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
		if req.TemplateID != "tpl-resolved" {
			t.Fatalf("redo req.TemplateID=%q, want tpl-resolved", req.TemplateID)
		}
		return &types.TemplateImageJobInfo{JobID: "job-redo-1", TemplateID: req.TemplateID}, nil
	}

	c, w := newRedoTestContext(t, `{"request_id":"req-1","template_id":"my-alias"}`)
	handleRedoTemplateAction(c)

	require.Equal(t, http.StatusOK, w.Code)
	resp := &types.CreateTemplateFromImageRes{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), resp), "body=%s", w.Body.String())
	require.NotNil(t, resp.Ret)
	assert.Equal(t, int(errorcode.ErrorCode_Success), resp.Ret.RetCode)
	require.NotNil(t, resp.Job)
	assert.Equal(t, "tpl-resolved", resp.Job.TemplateID)
	assert.Equal(t, "my-alias", resolvedInput)
}

func TestHandleRedoTemplateAction_ResolveAliasNotFound(t *testing.T) {
	origResolve := resolveTemplateIdentifierFn
	origRedo := redoTemplateFromImageFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolve
		redoTemplateFromImageFn = origRedo
	})

	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return "", templatecenter.ErrTemplateNotFound
	}
	redoCalled := false
	redoTemplateFromImageFn = func(ctx context.Context, req *types.RedoTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
		redoCalled = true
		return nil, nil
	}

	c, w := newRedoTestContext(t, `{"request_id":"req-2","template_id":"missing-alias"}`)
	handleRedoTemplateAction(c)

	require.Equal(t, http.StatusOK, w.Code)
	resp := &types.CreateTemplateFromImageRes{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), resp), "body=%s", w.Body.String())
	require.NotNil(t, resp.Ret)
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), resp.Ret.RetCode)
	assert.False(t, redoCalled, "redo submit must not be called when identifier resolution fails")
}
