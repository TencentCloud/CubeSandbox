// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestPreviewSandboxReturnsResolvedRequests(t *testing.T) {
	origDealFn := previewDealCubeboxCreateReqWithTemplateFn
	origConstructFn := previewConstructCubeletReqFn
	t.Cleanup(func() {
		previewDealCubeboxCreateReqWithTemplateFn = origDealFn
		previewConstructCubeletReqFn = origConstructFn
	})

	previewDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
		req.Namespace = "resolved-ns"
		req.NetworkType = "tap"
		req.Containers = append(req.Containers, &types.Container{
			Name: "main",
		})
		req.Volumes = append(req.Volumes, &types.Volume{Name: "work"})
		return nil
	}
	previewConstructCubeletReqFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) (*cubeboxv1.RunCubeSandboxRequest, error) {
		return &cubeboxv1.RunCubeSandboxRequest{
			RequestID: req.RequestID,
			Namespace: req.Namespace,
			Containers: []*cubeboxv1.ContainerConfig{
				{Name: "main"},
			},
			Volumes: []*cubeboxv1.Volume{
				{Name: "work"},
			},
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/preview", strings.NewReader(`{
		"requestID":"req-1",
		"annotations":{
			"cube.master.appsnapshot.template.id":"tpl-1",
			"cube.master.appsnapshot.template.version":"v2"
		}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := previewSandbox(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*sandboxPreviewResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	if assert.NotNil(t, got.APIRequest) {
		assert.Equal(t, "tpl-1", got.APIRequest.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
	if assert.NotNil(t, got.MergedRequest) {
		assert.Equal(t, "resolved-ns", got.MergedRequest.Namespace)
		assert.Len(t, got.MergedRequest.Containers, 1)
	}
	if assert.NotNil(t, got.CubeletRequest) {
		assert.Equal(t, "resolved-ns", got.CubeletRequest.Namespace)
		assert.Len(t, got.CubeletRequest.Containers, 1)
	}
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func TestHandleSandboxPreviewRejectsGet(t *testing.T) {
	rt := &CubeLog.RequestTrace{}
	resp := handleSandboxPreviewAction(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/cube/sandbox/preview", nil), rt)

	got, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
}
