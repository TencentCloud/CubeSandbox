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

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
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
