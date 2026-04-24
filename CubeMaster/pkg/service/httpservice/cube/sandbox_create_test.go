// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestCreateSandboxMapsMissingTemplateToNotFound(t *testing.T) {
	origDealFn := createSandboxDealCubeboxCreateReqWithTemplateFn
	origCreateFn := createSandboxRunFn
	t.Cleanup(func() {
		createSandboxDealCubeboxCreateReqWithTemplateFn = origDealFn
		createSandboxRunFn = origCreateFn
	})

	createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
		return templatecenter.ErrTemplateNotFound
	}
	createSandboxRunFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		t.Fatalf("sandbox.CreateSandbox should not be called when template lookup fails")
		return nil
	}

	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{
		"requestID":"req-1",
		"annotations":{
			"` + constants.CubeAnnotationAppSnapshotTemplateID + `":"tpl-missing",
			"` + constants.CubeAnnotationAppSnapshotTemplateVersion + `":"v2"
		}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.Equal(t, templatecenter.ErrTemplateNotFound.Error(), got.Ret.RetMsg)
	assert.Equal(t, int64(errorcode.ErrorCode_NotFound), rt.RetCode)
}

func TestCreateSandboxKeepsOtherTemplateErrorsAsParamsError(t *testing.T) {
	origDealFn := createSandboxDealCubeboxCreateReqWithTemplateFn
	origCreateFn := createSandboxRunFn
	t.Cleanup(func() {
		createSandboxDealCubeboxCreateReqWithTemplateFn = origDealFn
		createSandboxRunFn = origCreateFn
	})

	createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
		return assert.AnError
	}
	createSandboxRunFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		t.Fatalf("sandbox.CreateSandbox should not be called when template resolution fails")
		return nil
	}

	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{
		"requestID":"req-2",
		"annotations":{
			"` + constants.CubeAnnotationAppSnapshotTemplateID + `":"tpl-other-error",
			"` + constants.CubeAnnotationAppSnapshotTemplateVersion + `":"v2"
		}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, assert.AnError.Error(), got.Ret.RetMsg)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}
