// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
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
			"`+constants.CubeAnnotationAppSnapshotTemplateID+`":"tpl-missing",
			"`+constants.CubeAnnotationAppSnapshotTemplateVersion+`":"v2"
		}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(req, rt)

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
			"`+constants.CubeAnnotationAppSnapshotTemplateID+`":"tpl-other-error",
			"`+constants.CubeAnnotationAppSnapshotTemplateVersion+`":"v2"
		}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(req, rt)

	got, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, assert.AnError.Error(), got.Ret.RetMsg)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

// A Never policy combined with a caller-supplied liveness probe must come back
// as MasterParamsError (130400), which CubeAPI maps to HTTP 400. The probe
// could never restart the sandbox, so it would only kill it.
func TestCreateSandboxRejectsNeverWithLivenessProbe(t *testing.T) {
	origDealFn := createSandboxDealCubeboxCreateReqWithTemplateFn
	origCreateFn := createSandboxRunFn
	t.Cleanup(func() {
		createSandboxDealCubeboxCreateReqWithTemplateFn = origDealFn
		createSandboxRunFn = origCreateFn
	})

	// Exercise the HTTP status mapping without depending on a configured
	// template store; the merge+validate sequence has its own unit tests.
	createSandboxDealCubeboxCreateReqWithTemplateFn = func(_ context.Context, req *types.CreateCubeSandboxReq) error {
		return sandbox.ValidateRestartLiveness(req)
	}
	createSandboxRunFn = func(context.Context, *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		t.Fatalf("sandbox.CreateSandbox should not be called for an invalid restart probe")
		return nil
	}

	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{
		"requestID":"req-never-probe",
		"restart_policy":"Never",
		"liveness_probe":{"http_get":{"path":"/health","port":49983}},
		"annotations":{
			"`+constants.CubeAnnotationAppSnapshotTemplateID+`":"tpl-1",
			"`+constants.CubeAnnotationAppSnapshotTemplateVersion+`":"v2"
		}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(req, rt)

	got, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "liveness_probe requires restart policy")
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

func TestCreateSandboxRegistersSnapshotRefBeforeReturning(t *testing.T) {
	for _, tc := range []struct {
		name, backend, kind string
		failed              bool
		registerErr         bool
		wantCalls           int
	}{
		{name: "jfs_cross_node", backend: "jfs", kind: templatecenter.TemplateKindSnapshot, wantCalls: 1},
		{name: "xfs", backend: "xfs", kind: templatecenter.TemplateKindSnapshot, wantCalls: 1},
		{name: "s3", backend: "s3", kind: templatecenter.TemplateKindSnapshot, wantCalls: 1},
		{name: "ordinary_template", backend: "jfs", kind: templatecenter.TemplateKindTemplate},
		{name: "failed_create", backend: "jfs", kind: templatecenter.TemplateKindSnapshot, failed: true},
		{name: "registration_error_keeps_created_sandbox", backend: "jfs", kind: templatecenter.TemplateKindSnapshot, registerErr: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldDeal, oldRun := createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn
			oldKind, oldRegister, oldReplica := createSandboxGetTemplateKindFn, createSandboxRegisterRuntimeRefFn, createSandboxRegisterRuntimeRefWithReplicaFn
			t.Cleanup(func() {
				createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn = oldDeal, oldRun
				createSandboxGetTemplateKindFn, createSandboxRegisterRuntimeRefFn, createSandboxRegisterRuntimeRefWithReplicaFn = oldKind, oldRegister, oldReplica
			})
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(runInsReq2Affinity, func(ctx context.Context, _ *types.CreateCubeSandboxReq) (context.Context, error) { return ctx, nil })
			replica := templatecenter.ReplicaStatus{NodeID: "target", NodeIP: "10.0.0.2", Status: templatecenter.ReplicaStatusReady}
			createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
				req.Backend = tc.backend
				req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = "snap-source"
				resolved := templateResolveResultFromContext(ctx)
				*resolved = templateResolveResult{TemplateID: "snap-source", Kind: tc.kind, ChosenReplica: replica, HasChosenReplica: true}
				return nil
			}
			created := false
			response := &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}, SandboxID: "sb-new", HostID: "target", HostIP: "10.0.0.2"}
			if tc.failed {
				response.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			}
			createSandboxRunFn = func(context.Context, *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
				created = true
				return response
			}
			createSandboxGetTemplateKindFn = func(context.Context, string) (string, error) { t.Fatal("must reuse resolved kind"); return "", nil }
			createSandboxRegisterRuntimeRefFn = func(context.Context, string, string, string, string) error {
				t.Fatal("must reuse chosen replica")
				return nil
			}
			calls := 0
			createSandboxRegisterRuntimeRefWithReplicaFn = func(_ context.Context, snapshotID, sandboxID, nodeID, nodeIP string, chosen templatecenter.ReplicaStatus) error {
				calls++
				assert.True(t, created)
				assert.Equal(t, "snap-source", snapshotID)
				assert.Equal(t, response.SandboxID, sandboxID)
				assert.Equal(t, response.HostID, nodeID)
				assert.Equal(t, response.HostIP, nodeIP)
				assert.Equal(t, replica, chosen)
				if tc.registerErr {
					return assert.AnError
				}
				return nil
			}
			req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{"requestID":"req-register","instanceType":"cubebox"}`))
			got := createSandbox(req, &CubeLog.RequestTrace{})
			assert.Same(t, response, got)
			assert.Equal(t, tc.wantCalls, calls, "registration must finish before the handler returns")
		})
	}
}
