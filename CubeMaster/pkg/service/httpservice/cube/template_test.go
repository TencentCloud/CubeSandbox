// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestConstructCreateReqDefaultsToCubeboxForTemplateRestore(t *testing.T) {
	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{
		"requestID":"req-1",
		"annotations":{
			"cube.master.appsnapshot.template.id":"template-1",
			"cube.master.appsnapshot.template.version":"v2"
		}
	}`))

	got, err := constructCreateReq(req)
	if err != nil {
		t.Fatalf("constructCreateReq failed: %v", err)
	}
	assert.Equal(t, cubebox.InstanceType_cubebox.String(), got.InstanceType)
	assert.Equal(t, "v2", got.Annotations[constants.CubeAnnotationAppSnapshotVersion])
	assert.Equal(t, "v2", got.Annotations[constants.CubeAnnotationAppSnapshotTemplateVersion])
}

func TestConstructCreateReqPreservesDistributionScope(t *testing.T) {
	req := httptest.NewRequest("POST", "/cube/template", strings.NewReader(`{
		"requestID":"req-scope",
		"distribution_scope":["node-a","10.0.0.2"]
	}`))

	got, err := constructCreateReq(req)
	if err != nil {
		t.Fatalf("constructCreateReq failed: %v", err)
	}
	assert.Equal(t, []string{"node-a", "10.0.0.2"}, got.DistributionScope)
}

func TestDeleteTemplateMapsAttemptInProgressToParamsError(t *testing.T) {
	origDeleteTemplateFn := deleteTemplateFn
	t.Cleanup(func() {
		deleteTemplateFn = origDeleteTemplateFn
	})
	deleteTemplateFn = func(ctx context.Context, templateID, instanceType string) error {
		return fmtWrapped(templatecenter.ErrTemplateAttemptInProgress, "build still running")
	}

	req := httptest.NewRequest(http.MethodDelete, "/cube/template", strings.NewReader(`{"RequestID":"req-1","template_id":"tpl-1"}`))
	rt := &CubeLog.RequestTrace{}
	resp := deleteTemplate(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "build still running")
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

func TestDeleteTemplateMapsCleanupLocatorMissingToNotFound(t *testing.T) {
	origDeleteTemplateFn := deleteTemplateFn
	t.Cleanup(func() {
		deleteTemplateFn = origDeleteTemplateFn
	})
	deleteTemplateFn = func(ctx context.Context, templateID, instanceType string) error {
		return fmtWrapped(templatecenter.ErrTemplateCleanupLocatorMissing, "historical locator missing")
	}

	req := httptest.NewRequest(http.MethodDelete, "/cube/template", strings.NewReader(`{"RequestID":"req-2","template_id":"tpl-2"}`))
	rt := &CubeLog.RequestTrace{}
	resp := deleteTemplate(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "historical locator missing")
	assert.Equal(t, int64(errorcode.ErrorCode_NotFound), rt.RetCode)
}

func TestDeleteTemplateSuccessResponse(t *testing.T) {
	origDeleteTemplateFn := deleteTemplateFn
	t.Cleanup(func() {
		deleteTemplateFn = origDeleteTemplateFn
	})
	deleteTemplateFn = func(ctx context.Context, templateID, instanceType string) error {
		return nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/cube/template", strings.NewReader(`{"RequestID":"req-3","template_id":"tpl-3","instance_type":"cubebox"}`))
	rt := &CubeLog.RequestTrace{}
	resp := deleteTemplate(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "tpl-3", got.TemplateID)
	assert.Equal(t, "req-3", got.RequestID)
	assert.Equal(t, "cubebox", rt.InstanceType)
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func fmtWrapped(base error, msg string) error {
	return errors.Join(base, errors.New(msg))
}

func TestDeleteTemplateRejectsMissingTemplateID(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/cube/template", strings.NewReader(`{"RequestID":"req-4"}`))
	rt := &CubeLog.RequestTrace{}
	resp := deleteTemplate(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, "template_id is required", got.Ret.RetMsg)
}

func TestDeleteTemplateRequestBodyUsesTemplateDeleteRequestSchema(t *testing.T) {
	body, err := json.Marshal(&deleteTemplateRequest{
		RequestID:    "req-5",
		TemplateID:   "tpl-5",
		InstanceType: "cubebox",
	})
	if err != nil {
		t.Fatalf("marshal deleteTemplateRequest failed: %v", err)
	}
	assert.Contains(t, string(body), `"template_id":"tpl-5"`)
	assert.Contains(t, string(body), `"RequestID":"req-5"`)
}

func TestGetTemplateIncludeRequest(t *testing.T) {
	origGetTemplateInfoFn := getTemplateInfoFn
	origGetTemplateRequestFn := getTemplateRequestFn
	t.Cleanup(func() {
		getTemplateInfoFn = origGetTemplateInfoFn
		getTemplateRequestFn = origGetTemplateRequestFn
	})
	getTemplateInfoFn = func(ctx context.Context, templateID string) (*templatecenter.TemplateInfo, error) {
		return &templatecenter.TemplateInfo{
			TemplateID:   templateID,
			InstanceType: "cubebox",
			Version:      "v2",
			Status:       "READY",
		}, nil
	}
	getTemplateRequestFn = func(ctx context.Context, templateID string) (*types.CreateCubeSandboxReq, error) {
		return &types.CreateCubeSandboxReq{
			Request: &types.Request{RequestID: "req-preview"},
			Annotations: map[string]string{
				constants.CubeAnnotationAppSnapshotTemplateID: templateID,
			},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/template?template_id=tpl-include&include_request=true", nil)
	rt := &CubeLog.RequestTrace{}
	resp := getTemplate(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	if assert.NotNil(t, got.CreateRequest) {
		assert.Equal(t, "tpl-include", got.CreateRequest.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}
