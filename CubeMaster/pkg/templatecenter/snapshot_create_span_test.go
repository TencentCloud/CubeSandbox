// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

type snapshotCreateStubs struct {
	commitRet         errorcodev1.ErrorCode
	commitErr         error
	commitPanic       any
	upsertErr         error
	finalReadyErr     error
	snapshotFieldsErr error
	cleanupRet        errorcodev1.ErrorCode
	cleanupRspNil     bool
	cleanupErr        error
	cleanupPanic      any
	deleteReplicasErr error
	observeCommit     func(context.Context)
}

func stubSnapshotCreateRun(t *testing.T, s snapshotCreateStubs) {
	t.Helper()
	if s.commitRet == 0 {
		s.commitRet = errorcodev1.ErrorCode_Success
	}
	if s.cleanupRet == 0 {
		s.cleanupRet = errorcodev1.ErrorCode_Success
	}
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(cubelet.CommitSandbox, func(ctx context.Context, _ string, _ *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
		if s.observeCommit != nil {
			s.observeCommit(ctx)
		}
		if s.commitPanic != nil {
			panic(s.commitPanic)
		}
		if s.commitErr != nil {
			return nil, s.commitErr
		}
		return &cubeboxv1.CommitSandboxResponse{
			Ret:          &errorcodev1.Ret{RetCode: s.commitRet},
			SnapshotPath: "/data/snap/snap-1",
		}, nil
	})
	patches.ApplyFunc(cubelet.CleanupTemplate, func(_ context.Context, _ string, _ *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		if s.cleanupPanic != nil {
			panic(s.cleanupPanic)
		}
		if s.cleanupErr != nil {
			return nil, s.cleanupErr
		}
		if s.cleanupRspNil {
			return nil, nil
		}
		return &cubeboxv1.CleanupTemplateResponse{Ret: &errorcodev1.Ret{RetCode: s.cleanupRet}}, nil
	})
	patches.ApplyFunc(getSnapshotRecord, func(_ context.Context, _ string) (*models.SnapshotRecord, error) {
		return nil, ErrSnapshotNotFound
	})
	patches.ApplyFunc(UpsertReplica, func(_ context.Context, _ string, _ string, _ ReplicaStatus) error {
		return s.upsertErr
	})
	patches.ApplyFunc(updateSnapshotFields, func(_ context.Context, _ string, _ map[string]any) error {
		return s.snapshotFieldsErr
	})
	patches.ApplyFunc(updateTemplateImageJob, func(_ context.Context, _ string, fields map[string]any) error {
		if status, ok := fields["status"].(string); ok && status == JobStatusReady && s.finalReadyErr != nil {
			return s.finalReadyErr
		}
		return nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(string, []ReplicaStatus) {})
	patches.ApplyFunc(setTemplateRequestCache, func(string, *sandboxtypes.CreateCubeSandboxReq) error { return nil })
	patches.ApplyFunc(registerTemplateReplicaForSnapshot, func(string, string, int64) {})
	patches.ApplyFunc(deleteReplicasByTemplateID, func(_ context.Context, _ string) error { return s.deleteReplicasErr })
	patches.ApplyFunc(invalidateTemplateCaches, func(string) {})
}

func snapshotCreateRequestFixture(snapshotID string) *sandboxtypes.CreateCubeSandboxReq {
	return &sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID: snapshotID,
		},
		SnapshotDir: "/data/snap/" + snapshotID,
	}
}

func snapshotStoredRequestFixture() *sandboxtypes.CreateCubeSandboxReq {
	return &sandboxtypes.CreateCubeSandboxReq{Request: &sandboxtypes.Request{RequestID: "req-1"}}
}

func spanStringAttr(t *testing.T, span sdktrace.ReadOnlySpan, key string) string {
	t.Helper()
	kv, ok := attrOf(span, key)
	if !ok {
		t.Fatalf("span %q has no attribute %q", span.Name(), key)
	}
	return kv.Value.AsString()
}

func TestRunSnapshotCreateJobSuccessEmitsCaptureAndRegisterWithoutCleanup(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if err := runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
		snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture()); err != nil {
		t.Fatalf("runSnapshotCreateJob: %v", err)
	}
	root.End()
	flush()

	capture := spanNamed(t, rec, telemetry.SpanSnapshotCreateCapture)
	register := spanNamed(t, rec, telemetry.SpanSnapshotCreateRegister)
	if got := spanCount(rec, telemetry.SpanSnapshotCreateCleanup); got != 0 {
		t.Errorf("successful capture emitted %d cleanup span(s), want 0", got)
	}
	for name, span := range map[string]sdktrace.ReadOnlySpan{"capture": capture, "register": register} {
		if span.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s parent span id = %s, want the request span %s",
				name, span.Parent().SpanID(), root.SpanContext().SpanID())
		}
		if span.Status().Code == codes.Error {
			t.Errorf("%s must not be marked failed on the success path", name)
		}
	}
	if got := spanStringAttr(t, capture, telemetry.AttrJobID); got != "job-1" {
		t.Errorf("capture %s = %q, want job-1", telemetry.AttrJobID, got)
	}
	if got := spanStringAttr(t, capture, telemetry.AttrSnapshotID); got != "snap-1" {
		t.Errorf("capture %s = %q, want snap-1", telemetry.AttrSnapshotID, got)
	}
}

func TestRunSnapshotCreateJobWithoutTraceEmitsNoSpans(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{})

	if err := runSnapshotCreateJob(context.Background(), "job-1", "sb-1", "node-a", "10.0.0.1",
		snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture()); err != nil {
		t.Fatalf("runSnapshotCreateJob: %v", err)
	}
	flush()

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("untraced snapshot job emitted %d span(s), want 0", len(got))
	}
}

func TestRunSnapshotCreateJobCaptureFailureNestsCleanupUnderCapture(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{commitRet: errorcodev1.ErrorCode_GrpcError})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	// failSnapshotCreateJob returns only cleanup/state-write failures, so this stays nil.
	if err := runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
		snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture()); err != nil {
		t.Fatalf("cleanup succeeded, so runSnapshotCreateJob must return nil: %v", err)
	}
	root.End()
	flush()

	capture := spanNamed(t, rec, telemetry.SpanSnapshotCreateCapture)
	if capture.Status().Code != codes.Error {
		t.Errorf("capture span status = %v, want Error", capture.Status().Code)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateRegister); got != 0 {
		t.Errorf("failed capture emitted %d register span(s), want 0", got)
	}
	cleanup := spanNamed(t, rec, telemetry.SpanSnapshotCreateCleanup)
	if cleanup.Parent().SpanID() != capture.SpanContext().SpanID() {
		t.Errorf("cleanup parent = %s, want the failing capture span %s",
			cleanup.Parent().SpanID(), capture.SpanContext().SpanID())
	}
	if cleanup.Status().Code == codes.Error {
		t.Error("cleanup must succeed when the cleanup RPC and state writes succeed")
	}
}

func TestRunSnapshotCreateJobRegisterFailureNestsCleanupUnderRegister(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{upsertErr: errors.New("replica write failed")})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if err := runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
		snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture()); err != nil {
		t.Fatalf("cleanup succeeded, so runSnapshotCreateJob must return nil: %v", err)
	}
	root.End()
	flush()

	capture := spanNamed(t, rec, telemetry.SpanSnapshotCreateCapture)
	if capture.Status().Code == codes.Error {
		t.Error("capture must stay successful when only registration fails")
	}
	register := spanNamed(t, rec, telemetry.SpanSnapshotCreateRegister)
	if register.Status().Code != codes.Error {
		t.Errorf("register span status = %v, want Error", register.Status().Code)
	}
	cleanup := spanNamed(t, rec, telemetry.SpanSnapshotCreateCleanup)
	if cleanup.Parent().SpanID() != register.SpanContext().SpanID() {
		t.Errorf("cleanup parent = %s, want the failing register span %s",
			cleanup.Parent().SpanID(), register.SpanContext().SpanID())
	}
}

func TestRunSnapshotCreateJobReadyWriteFailureEmitsNoCleanup(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{finalReadyErr: errors.New("persist ready status failed")})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if err := runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
		snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture()); err == nil {
		t.Fatal("expected the final READY write failure to surface")
	}
	root.End()
	flush()

	register := spanNamed(t, rec, telemetry.SpanSnapshotCreateRegister)
	if register.Status().Code != codes.Error {
		t.Errorf("register span status = %v, want Error", register.Status().Code)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateCleanup); got != 0 {
		t.Errorf("terminal READY write failure emitted %d cleanup span(s), want 0", got)
	}
}

func TestRunSnapshotCreateJobPanicEndsSpansAndRepanics(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{commitPanic: "boom"})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("panic value = %v, want the original %q", r, "boom")
			}
		}()
		_ = runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
			snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture())
		t.Fatal("runSnapshotCreateJob must re-raise the panic")
	}()
	root.End()
	flush()

	capture := spanNamed(t, rec, telemetry.SpanSnapshotCreateCapture)
	if capture.Status().Code != codes.Error {
		t.Errorf("panicking capture span status = %v, want Error", capture.Status().Code)
	}
	if capture.EndTime().IsZero() {
		t.Error("panicking capture span must still be ended")
	}
}

func TestRunSnapshotCreateJobCaptureFailureKeepsCaptureOpenDuringCleanup(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{commitRet: errorcodev1.ErrorCode_GrpcError})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if err := runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
		snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture()); err != nil {
		t.Fatalf("runSnapshotCreateJob: %v", err)
	}
	root.End()
	flush()

	capture := spanNamed(t, rec, telemetry.SpanSnapshotCreateCapture)
	cleanup := spanNamed(t, rec, telemetry.SpanSnapshotCreateCleanup)
	if cleanup.Parent().SpanID() != capture.SpanContext().SpanID() {
		t.Fatalf("cleanup parent = %s, want the failing capture span %s",
			cleanup.Parent().SpanID(), capture.SpanContext().SpanID())
	}
	if capture.EndTime().Before(cleanup.EndTime()) {
		t.Errorf("capture ended at %s, before its cleanup child ended at %s; the failed stage must stay open while cleanup runs",
			capture.EndTime(), cleanup.EndTime())
	}
}

func TestRunSnapshotCreateJobCleanupFailuresMarkOnlyCleanupSpan(t *testing.T) {
	cases := []struct {
		name string
		stub snapshotCreateStubs
	}{
		{
			name: "transport error",
			stub: snapshotCreateStubs{commitRet: errorcodev1.ErrorCode_GrpcError, cleanupErr: errors.New("dial cubelet: connection refused")},
		},
		{
			name: "business ret code",
			stub: snapshotCreateStubs{commitRet: errorcodev1.ErrorCode_GrpcError, cleanupRet: errorcodev1.ErrorCode_GrpcError},
		},
		{
			name: "nil cleanup response",
			stub: snapshotCreateStubs{commitRet: errorcodev1.ErrorCode_GrpcError, cleanupRspNil: true},
		},
		{
			name: "delete replicas error",
			stub: snapshotCreateStubs{commitRet: errorcodev1.ErrorCode_GrpcError, deleteReplicasErr: errors.New("delete replicas failed")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := installSpanRecorder(t)
			stubSnapshotCreateRun(t, tc.stub)

			rootCtx, root := telemetry.Start(context.Background(), "test.root")

			if err := runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
				snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture()); err != nil {
				t.Fatalf("cleanup telemetry errors must not change the returned error: %v", err)
			}
			root.End()
			flush()

			cleanup := spanNamed(t, rec, telemetry.SpanSnapshotCreateCleanup)
			if cleanup.Status().Code != codes.Error {
				t.Errorf("cleanup span status = %v, want Error on %s", cleanup.Status().Code, tc.name)
			}
		})
	}
}

func TestRunSnapshotCreateJobCleanupKeepsStateWriteErrorsInReturn(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{
		commitRet:         errorcodev1.ErrorCode_GrpcError,
		cleanupErr:        errors.New("dial cubelet: connection refused"),
		snapshotFieldsErr: errors.New("snapshot status write failed"),
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	err := runSnapshotCreateJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1",
		snapshotCreateRequestFixture("snap-1"), snapshotStoredRequestFixture())
	root.End()
	flush()

	if err == nil || !strings.Contains(err.Error(), "snapshot status write failed") {
		t.Fatalf("returned error = %v, want the state-write failure preserved", err)
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Errorf("telemetry-only cleanup RPC error leaked into the returned error: %v", err)
	}
	cleanup := spanNamed(t, rec, telemetry.SpanSnapshotCreateCleanup)
	if cleanup.Status().Code != codes.Error {
		t.Errorf("cleanup span status = %v, want Error", cleanup.Status().Code)
	}
}

func recoverSnapshotStagePanic(t *testing.T, fn func()) (recovered any) {
	t.Helper()
	defer func() { recovered = recover() }()
	fn()
	return nil
}

func TestPrepareSnapshotCreateJobPanicEndsPrepareSpanAndRepanics(t *testing.T) {
	rec, flush := installSpanRecorder(t)

	sentinel := errors.New("prepare loader exploded")
	origLoad := loadSandboxCreateRequestFn
	t.Cleanup(func() { loadSandboxCreateRequestFn = origLoad })
	loadSandboxCreateRequestFn = func(context.Context, string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		panic(sentinel)
	}

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	recovered := recoverSnapshotStagePanic(t, func() {
		_, _, _, _ = prepareSnapshotCreateJob(rootCtx, "req-1", "sb-1", "node-a", "10.0.0.1", "snap", "")
	})
	root.End()
	flush()

	if recovered != sentinel {
		t.Fatalf("recovered = %#v, want the original panic value preserved", recovered)
	}
	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	if prepare.Status().Code != codes.Error {
		t.Errorf("prepare span status = %v, want Error", prepare.Status().Code)
	}
	if prepare.EndTime().IsZero() {
		t.Error("prepare span was not ended after the panic")
	}
}

func TestFailSnapshotCreateJobCleanupPanicEndsCleanupSpanAndRepanics(t *testing.T) {
	rec, flush := installSpanRecorder(t)

	sentinel := errors.New("cleanup rpc exploded")
	stubSnapshotCreateRun(t, snapshotCreateStubs{cleanupPanic: sentinel})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	recovered := recoverSnapshotStagePanic(t, func() {
		_ = failSnapshotCreateJob(rootCtx, "job-1", "snap-1", "10.0.0.1", "/data/snap/snap-1", nil,
			errors.New("commit sandbox failed"), "s3")
	})
	root.End()
	flush()

	if recovered != sentinel {
		t.Fatalf("recovered = %#v, want the original panic value preserved", recovered)
	}
	cleanup := spanNamed(t, rec, telemetry.SpanSnapshotCreateCleanup)
	if cleanup.Status().Code != codes.Error {
		t.Errorf("cleanup span status = %v, want Error", cleanup.Status().Code)
	}
	if cleanup.EndTime().IsZero() {
		t.Error("cleanup span was not ended after the panic")
	}
}

// submitSnapshotReplayStubs makes SubmitSandboxSnapshot resolve an existing request as a replay.
func submitSnapshotReplayStubs(t *testing.T, status string) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)

	origLoad := loadSandboxCreateRequestFn
	t.Cleanup(func() { loadSandboxCreateRequestFn = origLoad })
	loadSandboxCreateRequestFn = func(context.Context, string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return &sandboxtypes.CreateCubeSandboxReq{InstanceType: "cubebox", Annotations: map[string]string{}}, nil
	}
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(context.Context, string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: "job-existing", Operation: JobOperationSnapshotCreate, TemplateID: "snap-existing"}, nil
	})
	patches.ApplyFunc(snapshotCreateRequestMatches, func(_, _, _, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(_ context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:      jobID,
			RequestID:  "req-existing",
			TemplateID: "snap-existing",
			Status:     status,
		}, nil
	})
}

func TestSubmitSandboxSnapshotReadyReplayEmitsPrepareOnly(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	submitSnapshotReplayStubs(t, JobStatusReady)

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if _, err := SubmitSandboxSnapshot(rootCtx, "req-existing", "sb-1", "node-1", "10.0.0.1", "snap", ""); err != nil {
		t.Fatalf("SubmitSandboxSnapshot: %v", err)
	}
	root.End()
	flush()

	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	if prepare.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("prepare parent span id = %s, want the request span %s",
			prepare.Parent().SpanID(), root.SpanContext().SpanID())
	}
	if got := spanStringAttr(t, prepare, telemetry.AttrSnapshotID); got != "snap-existing" {
		t.Errorf("prepare %s = %q, want the replayed snapshot id snap-existing", telemetry.AttrSnapshotID, got)
	}
	if kv, ok := attrOf(prepare, telemetry.AttrReused); !ok || !kv.Value.AsBool() {
		t.Errorf("prepare %s = %v (present=%v), want true on an idempotent replay", telemetry.AttrReused, kv.Value.AsBool(), ok)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateCapture); got != 0 {
		t.Errorf("ready replay emitted %d capture span(s), want 0", got)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateRegister); got != 0 {
		t.Errorf("ready replay emitted %d register span(s), want 0", got)
	}
}

func TestSubmitSandboxSnapshotPrepareIsSiblingOfCaptureAndRegister(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	var commitCtx context.Context
	stubSnapshotCreateRun(t, snapshotCreateStubs{observeCommit: func(ctx context.Context) { commitCtx = ctx }})
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)

	origLoad := loadSandboxCreateRequestFn
	t.Cleanup(func() { loadSandboxCreateRequestFn = origLoad })
	loadSandboxCreateRequestFn = func(context.Context, string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return &sandboxtypes.CreateCubeSandboxReq{InstanceType: "cubebox", Annotations: map[string]string{}}, nil
	}
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(context.Context, string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: "job-pending", Operation: JobOperationSnapshotCreate, TemplateID: "snap-existing"}, nil
	})
	patches.ApplyFunc(snapshotCreateRequestMatches, func(_, _, _, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	infoCalls := 0
	patches.ApplyFunc(GetTemplateImageJobInfo, func(_ context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		infoCalls++
		status := JobStatusPending
		if infoCalls > 1 {
			status = JobStatusReady
		}
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:      jobID,
			RequestID:  "req-pending",
			TemplateID: "snap-existing",
			Status:     status,
		}, nil
	})
	patches.ApplyFunc(claimSnapshotJobExecution, func(context.Context, string, string, int32) (bool, error) {
		return true, nil
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")

	if _, err := SubmitSandboxSnapshot(rootCtx, "req-pending", "sb-1", "node-1", "10.0.0.1", "snap", ""); err != nil {
		t.Fatalf("SubmitSandboxSnapshot: %v", err)
	}
	root.End()
	flush()

	rootID := root.SpanContext().SpanID()
	for _, name := range []string{
		telemetry.SpanSnapshotCreatePrepare,
		telemetry.SpanSnapshotCreateCapture,
		telemetry.SpanSnapshotCreateRegister,
	} {
		span := spanNamed(t, rec, name)
		if span.Parent().SpanID() != rootID {
			t.Errorf("%s parent span id = %s, want the request span %s (stage spans must be siblings)",
				name, span.Parent().SpanID(), rootID)
		}
	}

	// The detached sync context must still carry the request trace into the real RPC.
	capture := spanNamed(t, rec, telemetry.SpanSnapshotCreateCapture)
	if commitCtx == nil {
		t.Fatal("CommitSandbox was never invoked")
	}
	rpcSpan := trace.SpanContextFromContext(commitCtx)
	if rpcSpan.TraceID() != root.SpanContext().TraceID() {
		t.Errorf("CommitSandbox trace id = %s, want the request trace %s",
			rpcSpan.TraceID(), root.SpanContext().TraceID())
	}
	if rpcSpan.SpanID() != capture.SpanContext().SpanID() {
		t.Errorf("CommitSandbox span = %s, want the capture span %s",
			rpcSpan.SpanID(), capture.SpanContext().SpanID())
	}
}

func stubSnapshotCreateSubmitResolution(t *testing.T, infoFn func(call int) (*sandboxtypes.TemplateImageJobInfo, error), claimFn func() (bool, error)) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)

	origLoad := loadSandboxCreateRequestFn
	t.Cleanup(func() { loadSandboxCreateRequestFn = origLoad })
	loadSandboxCreateRequestFn = func(context.Context, string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return &sandboxtypes.CreateCubeSandboxReq{InstanceType: "cubebox", Annotations: map[string]string{}}, nil
	}
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(context.Context, string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: "job-pending", Operation: JobOperationSnapshotCreate, TemplateID: "snap-existing"}, nil
	})
	patches.ApplyFunc(snapshotCreateRequestMatches, func(_, _, _, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	calls := 0
	patches.ApplyFunc(GetTemplateImageJobInfo, func(_ context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		calls++
		return infoFn(calls)
	})
	patches.ApplyFunc(claimSnapshotJobExecution, func(context.Context, string, string, int32) (bool, error) {
		return claimFn()
	})
}

func pendingThenReadyInfo(call int) (*sandboxtypes.TemplateImageJobInfo, error) {
	status := JobStatusPending
	if call > 1 {
		status = JobStatusReady
	}
	return &sandboxtypes.TemplateImageJobInfo{
		JobID:      "job-pending",
		RequestID:  "req-pending",
		TemplateID: "snap-existing",
		Status:     status,
	}, nil
}

func requireSnapshotStageSiblings(t *testing.T, rec *spanRecorder, rootID trace.SpanID) {
	t.Helper()
	for _, name := range []string{
		telemetry.SpanSnapshotCreatePrepare,
		telemetry.SpanSnapshotCreateCapture,
		telemetry.SpanSnapshotCreateRegister,
	} {
		span := spanNamed(t, rec, name)
		if span.Parent().SpanID() != rootID {
			t.Errorf("%s parent span id = %s, want the request span %s", name, span.Parent().SpanID(), rootID)
		}
	}
}

func TestPrepareSpanCoversPreCapturePreparationAndEndsBeforeCapture(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{})

	var infoAt, claimAt, assembledAt time.Time
	submitInfoFn := func(call int) (*sandboxtypes.TemplateImageJobInfo, error) {
		if call == 1 {
			infoAt = time.Now()
		}
		return pendingThenReadyInfo(call)
	}
	claimFn := func() (bool, error) {
		claimAt = time.Now()
		return true, nil
	}
	stubSnapshotCreateSubmitResolution(t, submitInfoFn, claimFn)

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(buildSnapshotRequests, func(_ *sandboxtypes.CreateCubeSandboxReq, snapshotID string) (*sandboxtypes.CreateCubeSandboxReq, *sandboxtypes.CreateCubeSandboxReq, error) {
		assembledAt = time.Now()
		clone := &sandboxtypes.CreateCubeSandboxReq{
			InstanceType: "cubebox",
			Annotations:  map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: snapshotID},
			SnapshotDir:  "/data/snap/" + snapshotID,
		}
		return clone, clone, nil
	})

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	if _, err := SubmitSandboxSnapshot(rootCtx, "req-pending", "sb-1", "node-1", "10.0.0.1", "snap", ""); err != nil {
		t.Fatalf("SubmitSandboxSnapshot: %v", err)
	}
	root.End()
	flush()

	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	capture := spanNamed(t, rec, telemetry.SpanSnapshotCreateCapture)
	register := spanNamed(t, rec, telemetry.SpanSnapshotCreateRegister)
	for name, span := range map[string]sdktrace.ReadOnlySpan{"prepare": prepare, "capture": capture, "register": register} {
		if span.EndTime().IsZero() {
			t.Errorf("%s span was not ended", name)
		}
	}
	if prepare.EndTime().Before(infoAt) {
		t.Errorf("prepare ended at %s, before job-info lookup at %s", prepare.EndTime(), infoAt)
	}
	if prepare.EndTime().Before(claimAt) {
		t.Errorf("prepare ended at %s, before execution claim at %s", prepare.EndTime(), claimAt)
	}
	if prepare.EndTime().Before(assembledAt) {
		t.Errorf("prepare ended at %s, before request assembly at %s", prepare.EndTime(), assembledAt)
	}
	if capture.StartTime().Before(prepare.EndTime()) {
		t.Errorf("capture started at %s, before prepare ended at %s", capture.StartTime(), prepare.EndTime())
	}
	if register.StartTime().Before(capture.EndTime()) {
		t.Errorf("register started at %s, before capture ended at %s", register.StartTime(), capture.EndTime())
	}
	requireSnapshotStageSiblings(t, rec, root.SpanContext().SpanID())
}

func TestPrepareSpanEndsBeforeCaptureOnClaimError(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{})
	stubSnapshotCreateSubmitResolution(t,
		func(int) (*sandboxtypes.TemplateImageJobInfo, error) {
			return &sandboxtypes.TemplateImageJobInfo{JobID: "job-pending", TemplateID: "snap-existing", Status: JobStatusPending}, nil
		},
		func() (bool, error) { return false, errors.New("claim failed") })

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	_, err := SubmitSandboxSnapshot(rootCtx, "req-pending", "sb-1", "node-1", "10.0.0.1", "snap", "")
	root.End()
	flush()

	if err == nil || !strings.Contains(err.Error(), "claim failed") {
		t.Fatalf("SubmitSandboxSnapshot error = %v, want the claim failure preserved", err)
	}
	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	if prepare.Status().Code != codes.Error {
		t.Errorf("prepare span status = %v, want Error on a failed claim", prepare.Status().Code)
	}
	if prepare.EndTime().IsZero() {
		t.Error("prepare span was left open after the claim failure")
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateCapture); got != 0 {
		t.Errorf("failed claim emitted %d capture span(s), want 0", got)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateRegister); got != 0 {
		t.Errorf("failed claim emitted %d register span(s), want 0", got)
	}
}

func TestPrepareSpanEndsBeforeCaptureOnJobInfoError(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{})
	stubSnapshotCreateSubmitResolution(t,
		func(int) (*sandboxtypes.TemplateImageJobInfo, error) {
			return nil, errors.New("job info lookup failed")
		},
		func() (bool, error) { return true, nil })

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	_, err := SubmitSandboxSnapshot(rootCtx, "req-pending", "sb-1", "node-1", "10.0.0.1", "snap", "")
	root.End()
	flush()

	if err == nil || !strings.Contains(err.Error(), "job info lookup failed") {
		t.Fatalf("SubmitSandboxSnapshot error = %v, want the lookup failure preserved", err)
	}
	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	if prepare.Status().Code != codes.Error {
		t.Errorf("prepare span status = %v, want Error on a failed job-info lookup", prepare.Status().Code)
	}
	if prepare.EndTime().IsZero() {
		t.Error("prepare span was left open after the job-info lookup failure")
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateCapture); got != 0 {
		t.Errorf("failed job-info lookup emitted %d capture span(s), want 0", got)
	}
}

func TestPrepareSpanEndedWhenPreCaptureStepPanics(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{})
	sentinel := errors.New("job info lookup exploded")
	stubSnapshotCreateSubmitResolution(t,
		func(int) (*sandboxtypes.TemplateImageJobInfo, error) { panic(sentinel) },
		func() (bool, error) { return true, nil })

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	recovered := recoverSnapshotStagePanic(t, func() {
		_, _ = SubmitSandboxSnapshot(rootCtx, "req-pending", "sb-1", "node-1", "10.0.0.1", "snap", "")
	})
	root.End()
	flush()

	if recovered != sentinel {
		t.Fatalf("recovered = %#v, want the original panic value preserved", recovered)
	}
	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	if prepare.Status().Code != codes.Error {
		t.Errorf("prepare span status = %v, want Error on a pre-capture panic", prepare.Status().Code)
	}
	if prepare.EndTime().IsZero() {
		t.Error("prepare span was left open after a pre-capture panic")
	}
}

func TestPrepareStaysSuccessfulWhenCapturePanics(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{commitPanic: "boom"})
	stubSnapshotCreateSubmitResolution(t, pendingThenReadyInfo, func() (bool, error) { return true, nil })

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	recovered := recoverSnapshotStagePanic(t, func() {
		_, _ = SubmitSandboxSnapshot(rootCtx, "req-pending", "sb-1", "node-1", "10.0.0.1", "snap", "")
	})
	root.End()
	flush()

	if recovered != "boom" {
		t.Fatalf("recovered = %#v, want the original panic value preserved", recovered)
	}
	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	if prepare.Status().Code == codes.Error {
		t.Error("a panic after capture began must not retroactively mark prepare Error")
	}
	if prepare.EndTime().IsZero() {
		t.Error("prepare span must be ended")
	}
}

func TestPrepareSpanEndsBeforeCaptureWhenClaimLoses(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	rec, flush := installSpanRecorder(t)
	stubSnapshotCreateRun(t, snapshotCreateStubs{})
	stubSnapshotCreateSubmitResolution(t, pendingThenReadyInfo, func() (bool, error) { return false, nil })

	rootCtx, root := telemetry.Start(context.Background(), "test.root")
	if _, err := SubmitSandboxSnapshot(rootCtx, "req-pending", "sb-1", "node-1", "10.0.0.1", "snap", ""); err != nil {
		t.Fatalf("SubmitSandboxSnapshot: %v", err)
	}
	root.End()
	flush()

	prepare := spanNamed(t, rec, telemetry.SpanSnapshotCreatePrepare)
	if prepare.EndTime().IsZero() {
		t.Error("prepare span was left open when the claim was lost")
	}
	if prepare.Status().Code == codes.Error {
		t.Error("prepare must stay successful when a peer already claimed the job")
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateCapture); got != 0 {
		t.Errorf("lost claim emitted %d capture span(s), want 0", got)
	}
	if got := spanCount(rec, telemetry.SpanSnapshotCreateRegister); got != 0 {
		t.Errorf("lost claim emitted %d register span(s), want 0", got)
	}
}
