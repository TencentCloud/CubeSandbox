// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"gorm.io/gorm"
)

func stubSnapshotRollbackRun(t *testing.T, ret errorcodev1.ErrorCode, rpcErr error) {
	t.Helper()
	if ret == 0 {
		ret = errorcodev1.ErrorCode_Success
	}
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(cubelet.RollbackSandbox, func(_ context.Context, _ string, _ *cubeboxv1.RollbackSandboxRequest) (*cubeboxv1.RollbackSandboxResponse, error) {
		if rpcErr != nil {
			return nil, rpcErr
		}
		return &cubeboxv1.RollbackSandboxResponse{Ret: &errorcodev1.Ret{RetCode: ret}}, nil
	})
	// AcquireSnapshotRuntimeRef is inlined into its caller, so stub the real binding write.
	patches.ApplyFunc(AttachSnapshotRuntimeBinding, func(context.Context, SnapshotRuntimeRefInfo, string) error { return nil })
	patches.ApplyFunc(updateTemplateImageJob, func(context.Context, string, map[string]any) error { return nil })
	patches.ApplyFunc(recordSnapshotRollbackResult, func(bool) {})
	patches.ApplyFunc(failSnapshotRollbackJob, func(_ context.Context, _ string, _ string, _ []byte, cause error) error { return cause })
}

func TestRunSnapshotRollbackJobEmitsRestoreThenRegisterSiblings(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, nil)

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if err := runSnapshotRollbackJob(rootCtx, "job-rb", "sb-1", "snap-1", "node-1", "10.0.0.1",
		ReplicaStatus{}, 1, 0, "xfs"); err != nil {
		t.Fatalf("runSnapshotRollbackJob: %v", err)
	}
	root.End()
	flush()

	restore := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRestore)
	register := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRegister)
	if restore.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("restore parent = %s, want the request span %s", restore.Parent().SpanID(), root.SpanContext().SpanID())
	}
	if register.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("register parent = %s, want the request span %s", register.Parent().SpanID(), root.SpanContext().SpanID())
	}
	if restore.Status().Code == codes.Error || register.Status().Code == codes.Error {
		t.Errorf("successful rollback stages must not be marked failed")
	}
	if register.StartTime().Before(restore.EndTime()) {
		t.Errorf("register started at %s, before restore ended at %s", register.StartTime(), restore.EndTime())
	}
	if got := spanStringAttr(t, restore, telemetry.AttrSandboxID); got != "sb-1" {
		t.Errorf("restore %s = %q, want sb-1", telemetry.AttrSandboxID, got)
	}
}

func TestRunSnapshotRollbackJobRPCFailureMarksRestoreAndSkipsRegister(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, errors.New("dial cubelet: connection refused"))

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if err := runSnapshotRollbackJob(rootCtx, "job-rb", "sb-1", "snap-1", "node-1", "10.0.0.1",
		ReplicaStatus{}, 1, 0, "xfs"); err == nil {
		t.Fatal("runSnapshotRollbackJob must surface the transport failure")
	}
	root.End()
	flush()

	restore := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRestore)
	if restore.Status().Code != codes.Error {
		t.Errorf("restore span status = %v, want Error on a transport failure", restore.Status().Code)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotRollbackRegister); got != 0 {
		t.Errorf("failed restore emitted %d register span(s), want 0", got)
	}
}

func TestRunSnapshotRollbackJobBusinessFailureMarksRestoreError(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_GrpcError, nil)

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if err := runSnapshotRollbackJob(rootCtx, "job-rb", "sb-1", "snap-1", "node-1", "10.0.0.1",
		ReplicaStatus{}, 1, 0, "xfs"); err == nil {
		t.Fatal("runSnapshotRollbackJob must surface the business failure")
	}
	root.End()
	flush()

	restore := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRestore)
	if restore.Status().Code != codes.Error {
		t.Errorf("restore span status = %v, want Error on a business failure", restore.Status().Code)
	}
	if kv, ok := attrOf(restore, telemetry.AttrRetCode); !ok || kv.Value.AsInt64() != int64(errorcodev1.ErrorCode_GrpcError) {
		t.Errorf("restore %s = %v (present=%v), want %d", telemetry.AttrRetCode, kv.Value.AsInt64(), ok, int64(errorcodev1.ErrorCode_GrpcError))
	}
}

func stubRollbackSubmission(t *testing.T, infoFn func(ctx context.Context, call int) (*sandboxtypes.TemplateImageJobInfo, error)) {
	t.Helper()
	payload, err := marshalSnapshotRollbackRequest("req-rb", "sb-1", "snap-1", "node-1", "10.0.0.1", 1, 0, "xfs")
	if err != nil {
		t.Fatalf("marshalSnapshotRollbackRequest: %v", err)
	}
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(context.Context, string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       "job-rb",
			Operation:   JobOperationSnapshotRollback,
			TemplateID:  "snap-1",
			RequestJSON: payload,
		}, nil
	})
	patches.ApplyFunc(snapshotRollbackRequestMatches, func(string, string, string, string) bool { return true })
	patches.ApplyFunc(getSnapshotReadyReplica, func(context.Context, string, string) (ReplicaStatus, error) {
		return ReplicaStatus{}, nil
	})
	patches.ApplyFunc(claimSnapshotJobExecution, func(context.Context, string, string, int32) (bool, error) {
		return true, nil
	})
	calls := 0
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		calls++
		return infoFn(ctx, calls)
	})
}

func rollbackJobInfo(status string) *sandboxtypes.TemplateImageJobInfo {
	return &sandboxtypes.TemplateImageJobInfo{
		JobID:      "job-rb",
		RequestID:  "req-rb",
		TemplateID: "snap-1",
		SandboxID:  "sb-1",
		Status:     status,
	}
}

func TestRollbackSubmissionEmitsPrepareRestoreRegisterSiblings(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, nil)
	var finalInfoAt time.Time
	stubRollbackSubmission(t, func(_ context.Context, call int) (*sandboxtypes.TemplateImageJobInfo, error) {
		if call > 1 {
			finalInfoAt = time.Now()
			return rollbackJobInfo(JobStatusReady), nil
		}
		return rollbackJobInfo(JobStatusPending), nil
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	if _, err := RollbackSandboxToSnapshot(rootCtx, "req-rb", "sb-1", "snap-1", "cubebox", "xfs"); err != nil {
		t.Fatalf("RollbackSandboxToSnapshot: %v", err)
	}
	root.End()
	flush()

	rootID := root.SpanContext().SpanID()
	prepare := spanNamed(t, rec, telemetry.SpanSnapshotRollbackPrepare)
	for _, name := range []string{
		telemetry.SpanSnapshotRollbackPrepare,
		telemetry.SpanSnapshotRollbackRestore,
		telemetry.SpanSnapshotRollbackRegister,
	} {
		span := spanNamed(t, rec, name)
		if span.Parent().SpanID() != rootID {
			t.Errorf("%s parent span id = %s, want the request span %s (stage spans must be siblings)",
				name, span.Parent().SpanID(), rootID)
		}
	}
	if kv, ok := attrOf(prepare, telemetry.AttrReused); !ok || !kv.Value.AsBool() {
		t.Errorf("prepare %s = %v (present=%v), want true on an idempotent replay", telemetry.AttrReused, kv.Value.AsBool(), ok)
	}
	if got := spanStringAttr(t, prepare, telemetry.AttrSnapshotID); got != "snap-1" {
		t.Errorf("prepare %s = %q, want snap-1", telemetry.AttrSnapshotID, got)
	}
	register := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRegister)
	if register.EndTime().Before(finalInfoAt) {
		t.Errorf("register ended at %s, before the final job read at %s; the terminal result must be attributed to register",
			register.EndTime(), finalInfoAt)
	}
}

func TestRollbackFinalReadKeepsCallerContext(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, nil)

	wantDeadline := time.Now().Add(time.Hour)
	var (
		gotErr         error
		gotDeadline    time.Time
		gotHasDeadline bool
	)
	stubRollbackSubmission(t, func(ctx context.Context, call int) (*sandboxtypes.TemplateImageJobInfo, error) {
		if call > 1 {
			gotErr = ctx.Err()
			gotDeadline, gotHasDeadline = ctx.Deadline()
			return rollbackJobInfo(JobStatusReady), nil
		}
		return rollbackJobInfo(JobStatusPending), nil
	})

	ctx, cancel := context.WithDeadline(context.Background(), wantDeadline)
	cancel()
	if _, err := RollbackSandboxToSnapshot(ctx, "req-rb", "sb-1", "snap-1", "cubebox", "xfs"); err != nil {
		t.Fatalf("RollbackSandboxToSnapshot: %v", err)
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Errorf("final read ctx err = %v, want the caller cancellation to survive the detached job context", gotErr)
	}
	if !gotHasDeadline || !gotDeadline.Equal(wantDeadline) {
		t.Errorf("final read deadline = %v (present=%v), want the caller deadline %v", gotDeadline, gotHasDeadline, wantDeadline)
	}
}

func TestRollbackSubmissionReadyReplayEmitsPrepareOnly(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, nil)
	stubRollbackSubmission(t, func(context.Context, int) (*sandboxtypes.TemplateImageJobInfo, error) {
		return rollbackJobInfo(JobStatusReady), nil
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	if _, err := RollbackSandboxToSnapshot(rootCtx, "req-rb", "sb-1", "snap-1", "cubebox", "xfs"); err != nil {
		t.Fatalf("RollbackSandboxToSnapshot: %v", err)
	}
	root.End()
	flush()

	prepare := spanNamed(t, rec, telemetry.SpanSnapshotRollbackPrepare)
	if prepare.EndTime().IsZero() {
		t.Error("prepare span was left open on a ready replay")
	}
	if got := spanCount(rec, telemetry.SpanSnapshotRollbackRestore); got != 0 {
		t.Errorf("ready replay emitted %d restore span(s), want 0", got)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotRollbackRegister); got != 0 {
		t.Errorf("ready replay emitted %d register span(s), want 0", got)
	}
}

type rollbackPrepareCapture struct {
	name    string
	started chan struct{}
	mu      sync.Mutex
	start   time.Time
	end     time.Time
}

func (c *rollbackPrepareCapture) OnStart(_ context.Context, span sdktrace.ReadWriteSpan) {
	if span.Name() != c.name {
		return
	}
	c.mu.Lock()
	c.start = span.StartTime()
	c.mu.Unlock()
	select {
	case <-c.started:
	default:
		close(c.started)
	}
}

func (c *rollbackPrepareCapture) OnEnd(span sdktrace.ReadOnlySpan) {
	if span.Name() != c.name {
		return
	}
	c.mu.Lock()
	c.end = span.EndTime()
	c.mu.Unlock()
}

func (c *rollbackPrepareCapture) Shutdown(context.Context) error   { return nil }
func (c *rollbackPrepareCapture) ForceFlush(context.Context) error { return nil }

func (c *rollbackPrepareCapture) times() (time.Time, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start, c.end
}

func installRollbackPrepareCapture(t *testing.T, name string) *rollbackPrepareCapture {
	t.Helper()
	capture := &rollbackPrepareCapture{name: name, started: make(chan struct{})}
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(capture))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return capture
}

func TestRollbackPrepareSpanCoversLockWait(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	capture := installRollbackPrepareCapture(t, telemetry.SpanSnapshotRollbackPrepare)

	payload, err := marshalSnapshotRollbackRequest("req-lock", "sb-1", "snap-1", "node-1", "10.0.0.1", 1, 0, "xfs")
	if err != nil {
		t.Fatalf("marshalSnapshotRollbackRequest: %v", err)
	}
	insideLock := make(chan struct{})
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(context.Context, string) (*models.TemplateImageJob, error) {
		close(insideLock)
		return &models.TemplateImageJob{
			JobID:       "job-lock",
			Operation:   JobOperationSnapshotRollback,
			TemplateID:  "snap-1",
			RequestJSON: payload,
		}, nil
	})
	patches.ApplyFunc(snapshotRollbackRequestMatches, func(string, string, string, string) bool { return true })
	patches.ApplyFunc(getSnapshotReadyReplica, func(context.Context, string, string) (ReplicaStatus, error) {
		return ReplicaStatus{}, nil
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(context.Context, string) (*sandboxtypes.TemplateImageJobInfo, error) {
		return rollbackJobInfo(JobStatusReady), nil
	})

	lock := templateRequestLockGroup.get(snapshotSandboxLockKey("sb-1"))
	if lock == nil {
		t.Fatal("snapshot sandbox lock must not be nil")
	}
	lock.Lock()

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RollbackSandboxToSnapshot(rootCtx, "req-lock", "sb-1", "snap-1", "cubebox", "xfs")
	}()

	select {
	case <-capture.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prepare stage never opened; it must start before the write lock is acquired")
	}
	select {
	case <-insideLock:
		t.Fatal("prepare body ran before the lock was released")
	case <-time.After(50 * time.Millisecond):
	}
	releaseAt := time.Now()
	lock.Unlock()
	<-done
	root.End()

	start, end := capture.times()
	if start.IsZero() {
		t.Fatal("prepare span never started")
	}
	if start.After(releaseAt) {
		t.Errorf("prepare started at %s, after the lock was released at %s; the span must cover the lock wait", start, releaseAt)
	}
	if end.Before(releaseAt) {
		t.Errorf("prepare ended at %s, before the lock was released at %s", end, releaseAt)
	}
}

func TestRunSnapshotRollbackJobRPCPanicClosesRestoreSpanWithError(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, nil)
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(cubelet.RollbackSandbox, func(context.Context, string, *cubeboxv1.RollbackSandboxRequest) (*cubeboxv1.RollbackSandboxResponse, error) {
		panic("rollback rpc exploded")
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the RPC panic must be rethrown")
			}
		}()
		_ = runSnapshotRollbackJob(rootCtx, "job-rb", "sb-1", "snap-1", "node-1", "10.0.0.1", ReplicaStatus{}, 1, 0, "xfs")
	}()
	root.End()
	flush()

	restore := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRestore)
	if restore.Status().Code != codes.Error {
		t.Errorf("restore span status = %v, want Error when a panic unwinds it", restore.Status().Code)
	}
	if restore.EndTime().IsZero() {
		t.Error("restore span was left open after a panic")
	}
}

func TestRunSnapshotRollbackJobRegistrationPanicClosesRegisterSpanWithError(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, nil)
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(AttachSnapshotRuntimeBinding, func(context.Context, SnapshotRuntimeRefInfo, string) error {
		panic("runtime ref registration exploded")
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the registration panic must be rethrown")
			}
		}()
		_ = runSnapshotRollbackJob(rootCtx, "job-rb", "sb-1", "snap-1", "node-1", "10.0.0.1", ReplicaStatus{}, 1, 0, "xfs")
	}()
	root.End()
	flush()

	register := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRegister)
	if register.Status().Code != codes.Error {
		t.Errorf("register span status = %v, want Error when a panic unwinds it", register.Status().Code)
	}
	if register.EndTime().IsZero() {
		t.Error("register span was left open after a panic")
	}
}

func TestRollbackSubmissionFinalReadPanicClosesRegisterSpanWithError(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotRollbackRun(t, errorcodev1.ErrorCode_Success, nil)
	stubRollbackSubmission(t, func(_ context.Context, call int) (*sandboxtypes.TemplateImageJobInfo, error) {
		if call > 1 {
			panic("final job read exploded")
		}
		return rollbackJobInfo(JobStatusPending), nil
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the final-read panic must be rethrown")
			}
		}()
		_, _ = RollbackSandboxToSnapshot(rootCtx, "req-rb", "sb-1", "snap-1", "cubebox", "xfs")
	}()
	root.End()
	flush()

	register := spanNamed(t, rec, telemetry.SpanSnapshotRollbackRegister)
	if register.Status().Code != codes.Error {
		t.Errorf("register span status = %v, want Error when the terminal read panics", register.Status().Code)
	}
	if register.EndTime().IsZero() {
		t.Error("register span was left open after the final-read panic")
	}
}
