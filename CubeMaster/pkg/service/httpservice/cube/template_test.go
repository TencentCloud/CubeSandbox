// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
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

func TestDeleteTemplateMapsAttemptInProgressToConflict(t *testing.T) {
	origDeleteTemplateFn := deleteTemplateFn
	t.Cleanup(func() {
		deleteTemplateFn = origDeleteTemplateFn
	})
	deleteTemplateFn = func(ctx context.Context, templateID, instanceType string) error {
		return fmtWrapped(templatecenter.ErrTemplateAttemptInProgress, "build still running")
	}

	req := httptest.NewRequest(http.MethodDelete, "/cube/template", strings.NewReader(`{"RequestID":"req-1","template_id":"tpl-1"}`))
	rt := &CubeLog.RequestTrace{}
	resp := deleteTemplate(req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "build still running")
	assert.Equal(t, int64(errorcode.ErrorCode_Conflict), rt.RetCode)
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
	resp := deleteTemplate(req, rt)

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
	resp := deleteTemplate(req, rt)

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
	resp := deleteTemplate(req, rt)

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
	origResolveTemplateIdentifierFn := resolveTemplateIdentifierFn
	t.Cleanup(func() {
		getTemplateInfoFn = origGetTemplateInfoFn
		getTemplateRequestFn = origGetTemplateRequestFn
		resolveTemplateIdentifierFn = origResolveTemplateIdentifierFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
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
	resp := getTemplate(req, rt)

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

func TestGetTemplateResolvesAliasBeforeLookup(t *testing.T) {
	origGetTemplateInfoFn := getTemplateInfoFn
	origGetTemplateRequestFn := getTemplateRequestFn
	origResolveTemplateIdentifierFn := resolveTemplateIdentifierFn
	t.Cleanup(func() {
		getTemplateInfoFn = origGetTemplateInfoFn
		getTemplateRequestFn = origGetTemplateRequestFn
		resolveTemplateIdentifierFn = origResolveTemplateIdentifierFn
	})

	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		assert.Equal(t, "stable-python", identifier)
		return "tpl-resolved", nil
	}
	getTemplateInfoFn = func(ctx context.Context, templateID string) (*templatecenter.TemplateInfo, error) {
		assert.Equal(t, "tpl-resolved", templateID)
		return &templatecenter.TemplateInfo{
			TemplateID:   templateID,
			InstanceType: "cubebox",
			Version:      "v2",
			Status:       "READY",
			DisplayName:  "stable-python",
		}, nil
	}
	getTemplateRequestFn = func(ctx context.Context, templateID string) (*types.CreateCubeSandboxReq, error) {
		assert.Equal(t, "tpl-resolved", templateID)
		return &types.CreateCubeSandboxReq{
			Request: &types.Request{RequestID: "req-preview"},
			Annotations: map[string]string{
				constants.CubeAnnotationAppSnapshotTemplateID: templateID,
			},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/template?template_id=stable-python&include_request=true", nil)
	rt := &CubeLog.RequestTrace{}
	resp := getTemplate(req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "tpl-resolved", got.TemplateID)
	assert.Equal(t, "stable-python", got.DisplayName)
	if assert.NotNil(t, got.CreateRequest) {
		assert.Equal(t, "tpl-resolved", got.CreateRequest.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
}

func TestGetTemplateIncludesDisplayMetadata(t *testing.T) {
	origGetTemplateInfoFn := getTemplateInfoFn
	origResolveTemplateIdentifierFn := resolveTemplateIdentifierFn
	t.Cleanup(func() {
		getTemplateInfoFn = origGetTemplateInfoFn
		resolveTemplateIdentifierFn = origResolveTemplateIdentifierFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	getTemplateInfoFn = func(ctx context.Context, templateID string) (*templatecenter.TemplateInfo, error) {
		return &templatecenter.TemplateInfo{
			TemplateID:   templateID,
			InstanceType: "cubebox",
			Version:      "v2",
			Status:       "READY",
			DisplayName:  "python-template",
			CreatedAt:    "2026-06-17 12:00:00",
			ImageInfo:    "docker.io/library/python:3.12",
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/template?template_id=tpl-metadata", nil)
	rt := &CubeLog.RequestTrace{}
	resp := getTemplate(req, rt)

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, "python-template", got.DisplayName)
	assert.Equal(t, "2026-06-17 12:00:00", got.CreatedAt)
	assert.Equal(t, "docker.io/library/python:3.12", got.ImageInfo)
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

// TestSetTemplateAliasHandler_ResolvesIdentifier_And_ReturnsUpdatedDetail
// verifies the happy path: path param is fed to resolveTemplateIdentifierFn,
// setTemplateAliasFn is invoked on the resolved id, and on success the
// refreshed TemplateInfo is returned with RetCode=Success.
func TestSetTemplateAliasHandler_ResolvesIdentifier_And_ReturnsUpdatedDetail(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	origGetInfoFn := getTemplateInfoFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
		getTemplateInfoFn = origGetInfoFn
	})

	var resolvedIdentifier string
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		resolvedIdentifier = identifier
		return "tpl-resolved", nil
	}
	var setCalledID, setCalledAlias string
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		setCalledID = templateID
		setCalledAlias = alias
		return nil
	}
	getTemplateInfoFn = func(ctx context.Context, templateID string) (*templatecenter.TemplateInfo, error) {
		return &templatecenter.TemplateInfo{
			TemplateID:   templateID,
			InstanceType: "cubebox",
			Version:      "v2",
			Status:       "READY",
			DisplayName:  "my-alias",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/stable-python/alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "stable-python")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, "stable-python", resolvedIdentifier,
		"path template_id must be forwarded to resolveTemplateIdentifierFn")
	assert.Equal(t, "tpl-resolved", setCalledID,
		"SetTemplateAlias must be invoked on the resolved id")
	assert.Equal(t, "my-alias", setCalledAlias)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "tpl-resolved", got.TemplateID)
	assert.Equal(t, "my-alias", got.DisplayName)
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

// TestSetTemplateAliasHandler_ClearPath_NoBody verifies that a PUT with no
// body is treated as a clear (alias=""), matching the shared contract
// ("omitted / null / empty alias -> clear").
func TestSetTemplateAliasHandler_ClearPath_NoBody(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	origGetInfoFn := getTemplateInfoFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
		getTemplateInfoFn = origGetInfoFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	var setCalledAlias string
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		setCalledAlias = alias
		return nil
	}
	getTemplateInfoFn = func(ctx context.Context, templateID string) (*templatecenter.TemplateInfo, error) {
		return &templatecenter.TemplateInfo{TemplateID: templateID, Status: "READY"}, nil
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/tpl-1/alias", nil)
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "tpl-1")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, "", setCalledAlias, "absent body must arrive as alias=\"\" (clear)")
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
}

// TestSetTemplateAliasHandler_400_OnInvalidAlias verifies that
// ErrInvalidAlias from SetTemplateAlias maps to MasterParamsError (400).
// SetTemplateAlias wraps validateTemplateAlias failures in ErrInvalidAlias
// so the handler can distinguish validation errors from raw DB errors
// (which map to 500).
func TestSetTemplateAliasHandler_400_OnInvalidAlias(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		return fmt.Errorf("%w: alias %q is invalid", templatecenter.ErrInvalidAlias, "BadAlias")
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/tpl-1/alias",
		strings.NewReader(`{"alias":"BadAlias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "tpl-1")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "invalid")
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

// TestSetTemplateAliasHandler_500_OnUnknownDBError verifies that a raw
// (non-sentinel) DB error from SetTemplateAlias maps to 500 rather than
// being mis-classified as a 400 params error.
func TestSetTemplateAliasHandler_500_OnUnknownDBError(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		return errors.New("connection refused: dial tcp 10.0.0.1:3306: connect: connection refused")
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/tpl-1/alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "tpl-1")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), got.Ret.RetCode,
		"raw DB errors must surface as 500, not 400")
	assert.Equal(t, int64(errorcode.ErrorCode_MasterInternalError), rt.RetCode)
}

// TestSetTemplateAliasHandler_404_OnMissingTemplate verifies that
// ErrTemplateNotFound from SetTemplateAlias maps to NotFound (404).
func TestSetTemplateAliasHandler_404_OnMissingTemplate(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		return templatecenter.ErrTemplateNotFound
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/tpl-missing/alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "tpl-missing")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_NotFound), rt.RetCode)
}

// TestSetTemplateAliasHandler_400_OnSnapshot verifies that
// ErrAliasNotApplicableToSnapshot maps to 400 (a snapshot exists but has no
// alias slot — distinct from a genuine 404 "template not found").
func TestSetTemplateAliasHandler_400_OnSnapshot(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		return templatecenter.ErrAliasNotApplicableToSnapshot
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/snap-1/alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "snap-1")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

// TestSetTemplateAliasHandler_409_OnDuplicateAlias verifies that a
// duplicate-key error from SetTemplateAlias (two clients racing to claim the
// same alias) is mapped to ErrorCode_Conflict (130409). CubeAPI's map_err
// then translates 130409 → HTTP 409 (design §3.3).
func TestSetTemplateAliasHandler_409_OnDuplicateAlias(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	// Simulate a duplicate-key error by returning an error that the real
	// IsDuplicateAliasError recognises. The detector keys on
	// *mysql.MySQLError(1062) or "23505"/"unique_constraint" in the message;
	// we use the message form so the test does not import the mysql driver.
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		return errors.New("Error 1062 (23000): Duplicate entry 'my-alias' for key 'alias_key' unique_constraint")
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/tpl-1/alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "tpl-1")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_Conflict), rt.RetCode)
}

// TestSetTemplateAliasHandler_409_OnNotReady verifies that a non-READY target
// is mapped to ErrorCode_Conflict (130409) -> CubeAPI HTTP 409, giving the
// client a clear retry signal (design §3.3, §3.5).
func TestSetTemplateAliasHandler_409_OnNotReady(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return identifier, nil
	}
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		return templatecenter.ErrTemplateNotReady
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/tpl-1/alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "tpl-1")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_Conflict), rt.RetCode)
}

// TestSetTemplateAliasHandler_ResolvesIdentifierFailure_PropagatesNotFound
// verifies that when resolveTemplateIdentifierFn returns ErrTemplateNotFound
// (e.g. alias-as-identifier does not exist), the handler returns 404 rather
// than calling SetTemplateAlias.
func TestSetTemplateAliasHandler_ResolvesIdentifierFailure_PropagatesNotFound(t *testing.T) {
	origResolveFn := resolveTemplateIdentifierFn
	origSetFn := setTemplateAliasFn
	t.Cleanup(func() {
		resolveTemplateIdentifierFn = origResolveFn
		setTemplateAliasFn = origSetFn
	})
	resolveTemplateIdentifierFn = func(ctx context.Context, identifier string) (string, error) {
		return "", templatecenter.ErrTemplateNotFound
	}
	setCalled := false
	setTemplateAliasFn = func(ctx context.Context, templateID, alias string) error {
		setCalled = true
		return nil
	}

	req := httptest.NewRequest(http.MethodPut, "/cube/template/missing-alias/alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "missing-alias")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.False(t, setCalled, "SetTemplateAlias must not be called when resolution fails")
}

// TestSetTemplateAliasHandler_400_OnMissingTemplateID verifies the path
// param is required (empty template_id → 400 before any fn is called).
func TestSetTemplateAliasHandler_400_OnMissingTemplateID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/cube/template//alias",
		strings.NewReader(`{"alias":"my-alias"}`))
	rt := &CubeLog.RequestTrace{}
	resp := setTemplateAlias(req, rt, "")

	got, ok := resp.(*templateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, "template_id is required", got.Ret.RetMsg)
}
