// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubeproxy"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/restoreplace"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	volrefcount "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func attrString(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

func TestUpdateResumeSuccessEmitsSiblingStagesWithRealWorker(t *testing.T) {
	const sandboxID = "sb-resume-trace-real"
	rec, flush := installQueueSpanRecorder(t)

	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache(sandboxID)

	// Cancelled mid-worker to prove the RPC keeps the lock callback's cancellation.
	parent, clientCancel := context.WithCancel(context.Background())
	t.Cleanup(clientCancel)

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, id string,
		opts sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Common: &config.CommonConf{}}
	})
	patches.ApplyFunc(sandboxspec.Get, func(ctx context.Context, id string) (*types.CreateCubeSandboxReq, error) {
		return nil, sandboxspec.ErrSandboxSpecNotFound
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(ctx context.Context, id string) (*pausesnap.Record, error) {
		clientCancel()
		return &pausesnap.Record{
			SandboxID:  sandboxID,
			SnapshotID: "snap-r1",
			NodeID:     "node-a",
			NodeIP:     "127.0.0.1",
			Status:     pausesnap.StatusReady,
		}, nil
	})
	patches.ApplyFunc(volrefcount.ApplyFromExtInfo, func(ctx context.Context, extInfo map[string][]byte) {})
	patches.ApplyFunc(refreshProxyMapAfterResume, func(ctx context.Context, id, hostIP string,
		rsp *cubebox.RunCubeSandboxResponse) error {
		return nil
	})
	patches.ApplyFunc(cubeproxy.InvalidateBackendCache, func(ctx context.Context, id, hostIP string) error {
		return nil
	})
	patches.ApplyFunc(pausesnap.CleanupPauseSnapshot, func(ctx context.Context, requestID, nodeIP, snapshotID, backend string) error {
		return nil
	})
	patches.ApplyFunc(pausesnap.Delete, func(ctx context.Context, snapshotID string) error {
		return nil
	})

	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP + ":50051" })

	var createCtx context.Context
	patches.ApplyFunc(cubelet.Create, func(ctx context.Context, ep string,
		req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
		createCtx = ctx
		return &cubebox.RunCubeSandboxResponse{
			SandboxID: sandboxID,
			SandboxIP: "10.0.0.9",
			Ret:       &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Success},
		}, nil
	})

	prevPlacement := decidePauseResumePlacementFn
	t.Cleanup(func() { decidePauseResumePlacementFn = prevPlacement })
	decidePauseResumePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		return &restoreplace.Placement{NodeID: "node-a", NodeIP: "127.0.0.1"}, nil
	}

	var hookSpanID string
	RegisterAfterUpdateSandboxSuccessHook(func(ctx context.Context, id, instanceType, action, requestID string) {
		hookSpanID = traceSpanID(ctx)
	})
	t.Cleanup(ResetAfterUpdateSandboxSuccessHooks)

	provider := &captureCtxTimeoutProvider{}
	SetTimeoutProvider(provider)
	t.Cleanup(func() { SetTimeoutProvider(nil) })

	rootCtx, root := telemetry.Start(parent, "test.root")
	rsp := Update(rootCtx, &types.UpdateRequest{
		RequestID:    "req-resume-real",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "resume",
		Timeout:      types.TimeoutPtr(120),
	})
	root.End()
	flush()

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("resume should succeed, got ret=%+v", rsp.Ret)
	}

	if createCtx == nil {
		t.Fatal("cubelet.Create was never called")
	}
	if createCtx.Err() != nil {
		t.Fatalf("the resume RPC ctx was cancelled by the caller: %v", createCtx.Err())
	}
	if updateStagesFrom(createCtx) == nil {
		t.Fatal("the stage tracker must survive into the RPC context")
	}

	spans := rec.snapshot()
	rootID := root.SpanContext().SpanID()
	for _, name := range []string{
		telemetry.SpanSandboxResumePrepare,
		telemetry.SpanSandboxResumeRuntime,
		telemetry.SpanSandboxResumeFinalize,
	} {
		found := spansNamed(spans, name)
		if len(found) != 1 {
			t.Fatalf("want exactly one %s span, got %d", name, len(found))
		}
		if found[0].Parent().SpanID() != rootID {
			t.Fatalf("%s must be a sibling under the update root", name)
		}
	}

	finalize := spansNamed(spans, telemetry.SpanSandboxResumeFinalize)[0]
	if got := finalize.SpanContext().SpanID().String(); got != hookSpanID {
		t.Fatalf("success hook must run under the finalize stage: hook span=%s finalize=%s", hookSpanID, got)
	}
	if provider.lastCtx == nil {
		t.Fatal("timeout must be published")
	}
	if got := traceSpanID(provider.lastCtx); got != finalize.SpanContext().SpanID().String() {
		t.Fatalf("timeout publish must run under the finalize stage: got %s want %s", got, finalize.SpanContext().SpanID())
	}
	if snap, ok := attrString(finalize, telemetry.AttrSnapshotID); !ok || snap != "snap-r1" {
		t.Fatalf("finalize must carry %s=snap-r1, got %q ok=%v", telemetry.AttrSnapshotID, snap, ok)
	}
}

func TestUpdatePauseFailureMarksRuntimeStage(t *testing.T) {
	const sandboxID = "sb-pause-trace-fail"
	rec, flush := installQueueSpanRecorder(t)

	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache(sandboxID)

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, id string,
		opts sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Common: &config.CommonConf{}}
	})
	patches.ApplyFunc(sandboxspec.Get, func(ctx context.Context, id string) (*types.CreateCubeSandboxReq, error) {
		return nil, sandboxspec.ErrSandboxSpecNotFound
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(ctx context.Context, id string) (*pausesnap.Record, error) {
		return nil, nil
	})
	patches.ApplyFunc(pausesnap.Begin, func(ctx context.Context, id, nodeID, nodeIP, instanceType, backend string) (string, error) {
		return "snap-pause-1", nil
	})
	patches.ApplyFunc(pausesnap.MarkFailed, func(ctx context.Context, id, snapID, msg string) {})
	patches.ApplyFunc(volrefcount.ApplyFromExtInfo, func(ctx context.Context, extInfo map[string][]byte) {})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP + ":50051" })
	patches.ApplyFunc(cubelet.UpdateWithTimeout, func(ctx context.Context, ep string,
		req *cubebox.UpdateCubeSandboxRequest, timeout time.Duration) (*cubebox.UpdateCubeSandboxResponse, error) {
		return &cubebox.UpdateCubeSandboxResponse{
			RequestID: req.GetRequestID(),
			Ret:       &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_TaskPauseFailed},
		}, nil
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	rsp := Update(rootCtx, &types.UpdateRequest{
		RequestID:    "req-pause-trace-fail",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "pause",
	})
	root.End()
	flush()

	if rsp.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		t.Fatalf("pause should fail, got ret=%+v", rsp.Ret)
	}

	spans := rec.snapshot()
	runtimes := spansNamed(spans, telemetry.SpanSandboxPauseRuntime)
	if len(runtimes) != 1 {
		t.Fatalf("want one pause runtime span, got %d", len(runtimes))
	}
	if runtimes[0].Status().Code != codes.Error {
		t.Fatalf("failed runtime must end with error status, got %v", runtimes[0].Status().Code)
	}
	if got := spansNamed(spans, telemetry.SpanSandboxPauseFinalize); len(got) != 0 {
		t.Fatalf("a pause that never finalized must not open a finalize stage, got %d", len(got))
	}
}

func TestUpdatePanicMarksActiveStageAndPropagates(t *testing.T) {
	const sandboxID = "sb-pause-trace-panic"
	rec, flush := installQueueSpanRecorder(t)

	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache(sandboxID)

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, id string,
		opts sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Common: &config.CommonConf{}}
	})
	patches.ApplyFunc(sandboxspec.Get, func(ctx context.Context, id string) (*types.CreateCubeSandboxReq, error) {
		return nil, sandboxspec.ErrSandboxSpecNotFound
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(ctx context.Context, id string) (*pausesnap.Record, error) {
		return nil, nil
	})
	patches.ApplyFunc(pausesnap.Begin, func(ctx context.Context, id, nodeID, nodeIP, instanceType, backend string) (string, error) {
		panic("boom-panic")
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		Update(rootCtx, &types.UpdateRequest{
			RequestID:    "req-pause-trace-panic",
			SandboxID:    sandboxID,
			InstanceType: "cubebox",
			Action:       "pause",
		})
	}()
	root.End()
	flush()

	if recovered != "boom-panic" {
		t.Fatalf("the panic must propagate unchanged, got %v", recovered)
	}
	prepares := spansNamed(rec.snapshot(), telemetry.SpanSandboxPausePrepare)
	if len(prepares) != 1 {
		t.Fatalf("want one pause prepare span, got %d", len(prepares))
	}
	if prepares[0].Status().Code != codes.Error {
		t.Fatalf("the active stage of a panicking update must end error, got %v", prepares[0].Status().Code)
	}
}

func TestUpdateResumeFailureTracesOnlyPrepareStage(t *testing.T) {
	const sandboxID = "sb-resume-trace-prepare-fail"
	rec, flush := installQueueSpanRecorder(t)

	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache(sandboxID)

	stubResumeUpdate(t, &types.Res{Ret: &types.Ret{
		RetCode: int(errorcode.ErrorCode_MasterInternalError),
		RetMsg:  "resume failed",
	}})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	rsp := Update(rootCtx, &types.UpdateRequest{
		RequestID:    "req-resume-trace",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "resume",
		Timeout:      types.TimeoutPtr(120),
	})
	root.End()
	flush()

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterInternalError) {
		t.Fatalf("resume failure must keep its error, got ret=%+v", rsp.Ret)
	}

	spans := rec.snapshot()
	prepares := spansNamed(spans, telemetry.SpanSandboxResumePrepare)
	if len(prepares) != 1 {
		t.Fatalf("want one resume prepare span, got %d", len(prepares))
	}
	if prepares[0].Status().Code != codes.Error {
		t.Fatalf("failed prepare must end with an error status, got %v", prepares[0].Status().Code)
	}
	if got := spansNamed(spans, telemetry.SpanSandboxResumeRuntime); len(got) != 0 {
		t.Fatalf("a resume that never reached the RPC must not open a runtime stage, got %d", len(got))
	}
	if got := spansNamed(spans, telemetry.SpanSandboxResumeFinalize); len(got) != 0 {
		t.Fatalf("a resume that never reached finalize must not open a finalize stage, got %d", len(got))
	}
}

func traceSpanID(ctx context.Context) string {
	return trace.SpanContextFromContext(ctx).SpanID().String()
}

type captureCtxTimeoutProvider struct {
	lastCtx context.Context
}

func (p *captureCtxTimeoutProvider) RefreshTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) (int64, error) {
	p.lastCtx = ctx
	return 0, nil
}

func (p *captureCtxTimeoutProvider) LookupEndAt(ctx context.Context, sandboxID string) (int64, error) {
	return 0, nil
}
