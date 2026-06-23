// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestHandleTemplateLookupRejectsNonGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/cube/template/lookup?name=my-env", nil)
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateLookupAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateLookupResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, -1, got.Ret.RetCode)
	assert.Equal(t, int64(-1), rt.RetCode)
}

func TestHandleTemplateLookupRequiresName(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/cube/template/lookup?name=%20", nil)
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateLookupAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateLookupResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, "name is required", got.Ret.RetMsg)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

func TestHandleTemplateDisplayNameRejectsNonPost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/cube/template/display-name", nil)
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateDisplayNameAction(httptest.NewRecorder(), req, rt)

	res, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, -1, res.Ret.RetCode)
	assert.Equal(t, int64(-1), rt.RetCode)
}

func TestHandleTemplateDisplayNameRequiresTemplateID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/cube/template/display-name", strings.NewReader(`{"RequestID":"req-1"}`))
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateDisplayNameAction(httptest.NewRecorder(), req, rt)

	res, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), res.Ret.RetCode)
	assert.Equal(t, "template_id is required", res.Ret.RetMsg)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

func TestHandleTemplateDisplayNameRejectsBadBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/cube/template/display-name", strings.NewReader("not-json"))
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateDisplayNameAction(httptest.NewRecorder(), req, rt)

	res, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), res.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

func TestHandleTemplateLookupReturnsTemplateID(t *testing.T) {
	orig := lookupTemplateIDByDisplayName
	lookupTemplateIDByDisplayName = func(_ context.Context, _ string) (string, error) {
		return "tpl-abc", nil
	}
	t.Cleanup(func() { lookupTemplateIDByDisplayName = orig })

	req := httptest.NewRequest(http.MethodGet, "/cube/template/lookup?name=my-env", nil)
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateLookupAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateLookupResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "tpl-abc", got.TemplateID)
	assert.Equal(t, "my-env", got.DisplayName)
}

func TestHandleTemplateLookupMapsNotFound(t *testing.T) {
	orig := lookupTemplateIDByDisplayName
	lookupTemplateIDByDisplayName = func(_ context.Context, _ string) (string, error) {
		return "", templatecenter.ErrTemplateNameNotFound
	}
	t.Cleanup(func() { lookupTemplateIDByDisplayName = orig })

	req := httptest.NewRequest(http.MethodGet, "/cube/template/lookup?name=missing", nil)
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateLookupAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateLookupResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
}

func TestHandleTemplateLookupMapsAmbiguous(t *testing.T) {
	orig := lookupTemplateIDByDisplayName
	lookupTemplateIDByDisplayName = func(_ context.Context, _ string) (string, error) {
		return "", templatecenter.ErrTemplateNameAmbiguous
	}
	t.Cleanup(func() { lookupTemplateIDByDisplayName = orig })

	req := httptest.NewRequest(http.MethodGet, "/cube/template/lookup?name=dup", nil)
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateLookupAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*templateLookupResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.Equal(t, "template name not found", got.Ret.RetMsg)
}

func TestHandleTemplateDisplayNameMapsConflict(t *testing.T) {
	patches := gomonkey.ApplyFunc(templatecenter.UpdateDefinitionDisplayName, func(context.Context, string, string) error {
		return errors.Join(templatecenter.ErrTemplateNameInUse, errors.New(`"my-env"`))
	})
	t.Cleanup(patches.Reset)

	req := httptest.NewRequest(http.MethodPost, "/cube/template/display-name", strings.NewReader(`{"template_id":"tpl-1","display_name":"my-env"}`))
	rt := &CubeLog.RequestTrace{}
	resp := handleTemplateDisplayNameAction(httptest.NewRecorder(), req, rt)

	res, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), res.Ret.RetCode)
}
