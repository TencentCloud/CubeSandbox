// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type testSpanRecorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *testSpanRecorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *testSpanRecorder) Shutdown(context.Context) error { return nil }

func (r *testSpanRecorder) snapshot() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdktrace.ReadOnlySpan, len(r.spans))
	copy(out, r.spans)
	return out
}

func installTestTracer(t *testing.T) (*testSpanRecorder, func()) {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := &testSpanRecorder{}
	shutdown, err := telemetry.SetupWithExporter(rec, "cube-test")
	if err != nil {
		t.Fatalf("SetupWithExporter: %v", err)
	}
	var once sync.Once
	flush := func() {
		once.Do(func() {
			if err := shutdown(context.Background()); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		flush()
		otel.SetTracerProvider(prev)
	})
	return rec, flush
}

func TestRegisterCreatedSandboxRuntimeRefEmitsSpanOnlyWhenRegistering(t *testing.T) {
	origKind := createSandboxGetTemplateKindFn
	origReg := createSandboxRegisterRuntimeRefFn
	origRegReplica := createSandboxRegisterRuntimeRefWithReplicaFn
	t.Cleanup(func() {
		createSandboxGetTemplateKindFn = origKind
		createSandboxRegisterRuntimeRefFn = origReg
		createSandboxRegisterRuntimeRefWithReplicaFn = origRegReplica
	})

	registered := false
	createSandboxRegisterRuntimeRefFn = func(context.Context, string, string, string, string) error {
		registered = true
		return nil
	}
	createSandboxGetTemplateKindFn = func(context.Context, string) (string, error) {
		return templatecenter.TemplateKindTemplate, nil
	}

	rec, flush := installTestTracer(t)
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
	}}
	ret := &types.CreateCubeSandboxRes{SandboxID: "sb-1", HostID: "node-1", HostIP: "10.0.0.1"}
	if err := registerCreatedSandboxRuntimeRef(context.Background(), req, ret); err != nil {
		t.Fatalf("registerCreatedSandboxRuntimeRef: %v", err)
	}
	if registered {
		t.Fatal("a non-snapshot template must not register a runtime ref")
	}
	flush()
	if spans := rec.snapshot(); len(spans) != 0 {
		t.Fatalf("want no register_ref span when nothing registers, got %d span(s)", len(spans))
	}
}

func TestRegisterCreatedSandboxPromotesReservation(t *testing.T) {
	previous := createSandboxPromoteReservedSnapshotRefFn
	previousKind := createSandboxGetTemplateKindFn
	t.Cleanup(func() {
		createSandboxPromoteReservedSnapshotRefFn = previous
		createSandboxGetTemplateKindFn = previousKind
	})
	createSandboxGetTemplateKindFn = func(context.Context, string) (string, error) {
		return templatecenter.TemplateKindSnapshot, nil
	}
	called := false
	createSandboxPromoteReservedSnapshotRefFn = func(_ context.Context, snapshotID, sandboxID, nodeID, nodeIP string) error {
		called = true
		assert.Equal(t, "snap-promote", snapshotID)
		assert.Equal(t, "sb-promote", sandboxID)
		assert.Equal(t, "node-a", nodeID)
		assert.Equal(t, "10.0.0.1", nodeIP)
		return nil
	}
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID: "snap-promote",
		constants.CubeAnnotationRuntimeSnapshotID:     "snap-promote",
		constants.CubeAnnotationDesiredSandboxID:      "sb-promote",
	}}
	ret := &types.CreateCubeSandboxRes{SandboxID: "sb-promote", HostID: "node-a", HostIP: "10.0.0.1"}
	assert.NoError(t, registerCreatedSandboxRuntimeRef(context.Background(), req, ret))
	assert.True(t, called)
}

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

func TestCreateSandboxPanicRecordsFailureCodeOnBusinessSpan(t *testing.T) {
	origDealFn := createSandboxDealCubeboxCreateReqWithTemplateFn
	origCreateFn := createSandboxRunFn
	t.Cleanup(func() {
		createSandboxDealCubeboxCreateReqWithTemplateFn = origDealFn
		createSandboxRunFn = origCreateFn
	})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(runInsReq2Affinity, func(ctx context.Context, _ *types.CreateCubeSandboxReq) (context.Context, error) { return ctx, nil })

	createSandboxDealCubeboxCreateReqWithTemplateFn = func(context.Context, *types.CreateCubeSandboxReq) error { return nil }
	createSandboxRunFn = func(context.Context, *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes { panic("boom") }

	rec, flush := installTestTracer(t)
	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{"requestID":"req-panic","instanceType":"cubebox"}`))
	rt := &CubeLog.RequestTrace{}

	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered = %v, want the original panic re-raised for the middleware", r)
			}
		}()
		_ = createSandbox(req, rt)
		t.Fatal("createSandbox must not swallow the panic")
	}()
	flush()

	// The -1 default assigned at handler entry must survive the panic so the
	// business span agrees with the route span recorded after the middleware recovers.
	if rt.RetCode != -1 {
		t.Errorf("rt.RetCode = %d, want -1", rt.RetCode)
	}
	var business sdktrace.ReadOnlySpan
	for _, s := range rec.snapshot() {
		if s.Name() == telemetry.SpanCreate {
			business = s
		}
	}
	if business == nil {
		t.Fatalf("no %s span exported on the panic path", telemetry.SpanCreate)
	}
	if business.Status().Code != codes.Error {
		t.Errorf("business span status = %v, want Error", business.Status().Code)
	}
	if got := attrInt(t, business.Attributes(), telemetry.AttrRetCode); got != -1 {
		t.Errorf("%s = %d, want -1", telemetry.AttrRetCode, got)
	}
}

func TestCreateSandboxRejectsCallerSuppliedNetworkQosAnnotation(t *testing.T) {
	origDealFn := createSandboxDealCubeboxCreateReqWithTemplateFn
	origCreateFn := createSandboxRunFn
	t.Cleanup(func() {
		createSandboxDealCubeboxCreateReqWithTemplateFn = origDealFn
		createSandboxRunFn = origCreateFn
	})

	createSandboxDealCubeboxCreateReqWithTemplateFn = func(context.Context, *types.CreateCubeSandboxReq) error {
		t.Fatal("template resolution must not run for caller-supplied network qos")
		return nil
	}
	createSandboxRunFn = func(context.Context, *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		t.Fatal("sandbox create must not run for caller-supplied network qos")
		return nil
	}

	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{
		"requestID":"req-qos",
		"annotations":{
			"`+constants.CubeAnnotationAppSnapshotTemplateID+`":"tpl-1",
			"`+constants.CubeAnnotationAppSnapshotTemplateVersion+`":"v2",
			"`+constants.CubeAnnotationsNetWork+`":"{}"
		}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(req, rt)

	got, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "template-managed")
}

func TestCreateSandboxRejectsCallerSuppliedBlockIOQosAnnotation(t *testing.T) {
	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{
		"requestID":"req-qos",
		"annotations":{"`+constants.CubeAnnotationsBlkQos+`":"{}"}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(req, rt)

	got, ok := resp.(*types.Res)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "template-managed")
}

func TestCreateSandboxPreservesCallerSuppliedFilesystemQosAnnotation(t *testing.T) {
	origDealFn := createSandboxDealCubeboxCreateReqWithTemplateFn
	origCreateFn := createSandboxRunFn
	t.Cleanup(func() {
		createSandboxDealCubeboxCreateReqWithTemplateFn = origDealFn
		createSandboxRunFn = origCreateFn
	})

	const filesystemQos = `{"bandwidth":{"size":1024}}`
	createSandboxDealCubeboxCreateReqWithTemplateFn = func(_ context.Context, req *types.CreateCubeSandboxReq) error {
		assert.Equal(t, filesystemQos, req.Annotations[constants.CubeAnnotationsFSQos])
		return nil
	}
	createSandboxRunFn = func(_ context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		assert.Equal(t, filesystemQos, req.Annotations[constants.CubeAnnotationsFSQos])
		return &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
	}

	req := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{
		"requestID":"req-fs-qos",
		"annotations":{"`+constants.CubeAnnotationsFSQos+`":"{\"bandwidth\":{\"size\":1024}}"}
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSandbox(req, rt)

	got, ok := resp.(*types.CreateCubeSandboxRes)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
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
