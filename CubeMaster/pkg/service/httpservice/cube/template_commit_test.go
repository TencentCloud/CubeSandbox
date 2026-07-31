// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// invokeCommitHandler drives the gin handler handleSandboxCommitAction with a
// test gin.Context carrying rt, returning the decoded JSON response.
func invokeCommitHandler(t *testing.T, req *http.Request, rt *CubeLog.RequestTrace) commitTemplateResponse {
	t.Helper()
	patches := gomonkey.NewPatches()
	patches.ApplyFunc(sandbox.ResolveSandboxID, func(_ context.Context, sandboxID string) (string, error) {
		return sandboxID, nil
	})
	defer patches.Reset()
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(ctx)
	handleSandboxCommitAction(c)
	var got commitTemplateResponse
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	return got
}

func TestHandleSandboxCommitActionRejectsEmptyRequestID(t *testing.T) {
	registerKnownSandboxTestID(t)

	body := `{
		"sandbox_id":"` + knownSandboxTestID + `",
		"template_id":"tpl-1",
		"create_request":{
			"instance_type":"cubebox",
			"network_type":"tap",
			"annotations":{
				"cube.master.appsnapshot.template.id":"tpl-1",
				"cube.master.appsnapshot.template.version":"v2"
			}
		}
	}`
	req := httptest.NewRequest("POST", "/cube/sandbox/commit", strings.NewReader(body))
	rt := &CubeLog.RequestTrace{}
	got := invokeCommitHandler(t, req, rt)

	require.NotNil(t, got.Res)
	require.NotNil(t, got.Res.Ret)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Res.Ret.RetCode)
	assert.Contains(t, got.Res.Ret.RetMsg, "requestID is required")
	assert.NotEqual(t, "tpl-1", got.TemplateID)
	assert.True(t, strings.HasPrefix(got.TemplateID, "tpl-"), got.TemplateID)
}

func TestHandleSandboxCommitActionRejectsMissingFields(t *testing.T) {
	body := `{"requestID":"req-1"}`
	req := httptest.NewRequest("POST", "/cube/sandbox/commit", strings.NewReader(body))
	rt := &CubeLog.RequestTrace{}
	got := invokeCommitHandler(t, req, rt)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Res.Ret.RetCode)
	assert.Contains(t, got.Res.Ret.RetMsg, "sandbox_id is required")
}

func TestHandleSandboxCommitActionAllowsMissingCreateRequest(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetSandboxCache, func(sandboxID string) *localcache.SandboxCache {
		return &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.1"}
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{InsID: "node-1", IP: ip}, true
	})
	var gotRequestID, gotSandboxID, gotTemplateID string
	var gotOverride *types.CreateCubeSandboxReq
	patches.ApplyFunc(templatecenter.SubmitTemplateCommit, func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, templateID string, override *types.CreateCubeSandboxReq) (*types.TemplateImageJobInfo, error) {
		gotRequestID = requestID
		gotSandboxID = sandboxID
		gotTemplateID = templateID
		gotOverride = override
		return &types.TemplateImageJobInfo{JobID: "job-1", TemplateID: templateID}, nil
	})

	body := `{"requestID":"req-1","sandbox_id":"sb-1"}`
	req := httptest.NewRequest("POST", "/cube/sandbox/commit", strings.NewReader(body))
	rt := &CubeLog.RequestTrace{}
	got := invokeCommitHandler(t, req, rt)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Res.Ret.RetCode)
	assert.Equal(t, "req-1", gotRequestID)
	assert.Equal(t, "sb-1", gotSandboxID)
	assert.Equal(t, got.TemplateID, gotTemplateID)
	assert.Nil(t, gotOverride)
}

func TestHandleSandboxCommitActionSanitizesInternalErrors(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetSandboxCache, func(sandboxID string) *localcache.SandboxCache {
		return &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.1"}
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{InsID: "node-1", IP: ip}, true
	})
	patches.ApplyFunc(templatecenter.SubmitTemplateCommit, func(context.Context, string, string, string, string, string, *types.CreateCubeSandboxReq) (*types.TemplateImageJobInfo, error) {
		return nil, fmt.Errorf("load sandbox spec: dial tcp 10.0.0.2:3306: connection refused")
	})

	body := `{"requestID":"req-1","sandbox_id":"sb-1"}`
	req := httptest.NewRequest("POST", "/cube/sandbox/commit", strings.NewReader(body))
	rt := &CubeLog.RequestTrace{}
	got := invokeCommitHandler(t, req, rt)
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), got.Res.Ret.RetCode)
	assert.Equal(t, "failed to submit template commit", got.Res.Ret.RetMsg)
	assert.NotContains(t, got.Res.Ret.RetMsg, "10.0.0.2:3306")
}

func TestHandleSandboxCommitActionIgnoresProvidedTemplateID(t *testing.T) {
	registerKnownSandboxTestID(t)

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var submittedTemplateID string
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{InsID: "node-1", IP: ip}, true
	})
	patches.ApplyFunc(templatecenter.SubmitTemplateCommit, func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, templateID string, req *types.CreateCubeSandboxReq) (*types.TemplateImageJobInfo, error) {
		submittedTemplateID = templateID
		return &types.TemplateImageJobInfo{
			JobID:      "job-1",
			TemplateID: submittedTemplateID,
		}, nil
	})

	body := `{
		"requestID":"req-1",
		"sandbox_id":"` + knownSandboxTestID + `",
		"template_id":"custom-template",
		"create_request":{
			"instance_type":"cubebox",
			"network_type":"tap",
			"annotations":{
				"cube.master.appsnapshot.template.id":"sb-bad",
				"cube.master.appsnapshot.template.version":"v2"
			}
		}
	}`
	req := httptest.NewRequest("POST", "/cube/sandbox/commit", strings.NewReader(body))
	rt := &CubeLog.RequestTrace{}
	got := invokeCommitHandler(t, req, rt)

	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Res.Ret.RetCode)
	assert.Equal(t, submittedTemplateID, got.TemplateID)
	assert.NotEqual(t, "custom-template", got.TemplateID)
	assert.NotEqual(t, "sb-bad", submittedTemplateID)
	assert.True(t, strings.HasPrefix(got.TemplateID, "tpl-"), got.TemplateID)
}

func TestCommitTemplateErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "template id required is params error",
			err:  templatecenter.ErrTemplateIDRequired,
			want: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name: "duplicate template is params error",
			err:  templatecenter.ErrDuplicateTemplate,
			want: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name: "attempt in progress is params error",
			err:  fmt.Errorf("commit conflict: %w", templatecenter.ErrTemplateAttemptInProgress),
			want: int(errorcode.ErrorCode_MasterParamsError),
		},
		{
			name: "store not initialized is db error",
			err:  templatecenter.ErrTemplateStoreNotInitialized,
			want: int(errorcode.ErrorCode_DBError),
		},
		{
			name: "missing origin template is not found",
			err:  fmt.Errorf("legacy fallback failed: %w", templatecenter.ErrTemplateNotFound),
			want: int(errorcode.ErrorCode_NotFound),
		},
		{
			name: "unknown error is internal error",
			err:  fmt.Errorf("unexpected"),
			want: int(errorcode.ErrorCode_MasterInternalError),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, commitTemplateErrorCode(tc.err))
		})
	}
}

func TestCommitTemplateErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "attempt details are sanitized",
			err:  fmt.Errorf("%w: template tpl-secret is building as job-secret", templatecenter.ErrTemplateAttemptInProgress),
			want: templatecenter.ErrTemplateAttemptInProgress.Error(),
		},
		{
			name: "legacy template details are sanitized",
			err:  fmt.Errorf("base template tpl-secret lookup failed: %w", templatecenter.ErrTemplateNotFound),
			want: templatecenter.ErrTemplateNotFound.Error(),
		},
		{
			name: "store state is sanitized",
			err:  templatecenter.ErrTemplateStoreNotInitialized,
			want: "template service is unavailable",
		},
		{
			name: "database details are sanitized",
			err:  fmt.Errorf("dial tcp 10.0.0.2:3306: connection refused"),
			want: "failed to submit template commit",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, commitTemplateErrorMessage(tc.err))
			assert.NotContains(t, commitTemplateErrorMessage(tc.err), "secret")
			assert.NotContains(t, commitTemplateErrorMessage(tc.err), "10.0.0.2:3306")
		})
	}
}
