// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"gorm.io/gorm"
)

type jobWriteLog struct {
	mu     sync.Mutex
	writes []map[string]any
}

func (l *jobWriteLog) record(fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes = append(l.writes, fields)
}

func (l *jobWriteLog) finalStatus() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.writes) - 1; i >= 0; i-- {
		if status, ok := l.writes[i]["status"].(string); ok {
			return status
		}
	}
	return ""
}

func (l *jobWriteLog) finalErrorMessage() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.writes) - 1; i >= 0; i-- {
		if msg, ok := l.writes[i]["error_message"].(string); ok {
			return msg
		}
	}
	return ""
}

type commitJobStubs struct {
	commit    func(nodeIP string) (*cubeboxv1.CommitSandboxResponse, error)
	create    func(context.Context, string, *sandboxtypes.CreateCubeSandboxReq, string, string) error
	cleanup   func(context.Context, string, *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error)
	updateJob func(fields map[string]any) error
}

func successCleanup() *cubeboxv1.CleanupTemplateResponse {
	return &cubeboxv1.CleanupTemplateResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success}}
}

func stubCommitJob(t *testing.T, stubs commitJobStubs) *jobWriteLog {
	t.Helper()
	if stubs.commit == nil {
		stubs.commit = okCommit
	}
	if stubs.create == nil {
		stubs.create = func(context.Context, string, *sandboxtypes.CreateCubeSandboxReq, string, string) error { return nil }
	}
	if stubs.cleanup == nil {
		stubs.cleanup = func(context.Context, string, *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
			return successCleanup(), nil
		}
	}
	writes := &jobWriteLog{}
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(updateTemplateImageJob, func(_ context.Context, _ string, fields map[string]any) error {
		writes.record(fields)
		if stubs.updateJob != nil {
			return stubs.updateJob(fields)
		}
		return nil
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(cubelet.CommitSandbox, func(_ context.Context, ep string, _ *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
		return stubs.commit(ep)
	})
	patches.ApplyFunc(cubelet.CleanupTemplate, func(ctx context.Context, ep string, req *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		return stubs.cleanup(ctx, ep, req)
	})
	patches.ApplyFunc(createDefinition, stubs.create)
	patches.ApplyFunc(setTemplateRequestCache, func(string, *sandboxtypes.CreateCubeSandboxReq) error { return nil })
	patches.ApplyFunc(setTemplateLocalityCache, func(string, []ReplicaStatus) {})
	patches.ApplyFunc(UpsertReplica, func(context.Context, string, string, ReplicaStatus) error { return nil })
	patches.ApplyFunc(UpdateDefinitionStatus, func(context.Context, string, string, string) error { return nil })
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList { return nil })
	patches.ApplyFunc(localcache.RegisterTemplateReplica, func(string, string, int64) {})
	return writes
}

func okCommit(nodeIP string) (*cubeboxv1.CommitSandboxResponse, error) {
	return &cubeboxv1.CommitSandboxResponse{
		Ret:          &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
		SnapshotPath: "/data/snap/" + nodeIP,
	}, nil
}

func spanNamed(t *testing.T, rec *spanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.snapshot() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %s span, got %d", name, len(found))
	}
	return found[0]
}

func spanCount(rec *spanRecorder, name string) int {
	count := 0
	for _, s := range rec.snapshot() {
		if s.Name() == name {
			count++
		}
	}
	return count
}

func TestCommitRunSpanHierarchyOnSuccess(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	writes := stubCommitJob(t, commitJobStubs{})

	createReq, storedReq := newCommitFixtureRequests()
	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
	flush()

	if got := writes.finalStatus(); got != JobStatusReady {
		t.Errorf("final job status = %q, want %q", got, JobStatusReady)
	}

	runSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitRun)
	if runSpan.Parent().SpanID() != rootSpan.SpanContext().SpanID() {
		t.Errorf("run span parent = %s, want root %s", runSpan.Parent().SpanID(), rootSpan.SpanContext().SpanID())
	}
	if runSpan.Status().Code == codes.Error {
		t.Errorf("run span status = %v, want non-error", runSpan.Status().Code)
	}
	for key, want := range map[string]string{
		telemetry.AttrJobID:      "job-1",
		telemetry.AttrRequestID:  "req-1",
		telemetry.AttrSandboxID:  "sb-1",
		telemetry.AttrTemplateID: "tpl-commit-1",
	} {
		assertSpanStrAttr(t, runSpan, key, want)
	}

	snapshotSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitSnapshot)
	if snapshotSpan.Parent().SpanID() != runSpan.SpanContext().SpanID() {
		t.Errorf("snapshot span parent = %s, want run span %s", snapshotSpan.Parent().SpanID(), runSpan.SpanContext().SpanID())
	}
	if snapshotSpan.Status().Code == codes.Error {
		t.Errorf("snapshot span status = %v, want non-error", snapshotSpan.Status().Code)
	}

	registerSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitRegister)
	if registerSpan.Parent().SpanID() != runSpan.SpanContext().SpanID() {
		t.Errorf("register span parent = %s, want run span %s", registerSpan.Parent().SpanID(), runSpan.SpanContext().SpanID())
	}
	if registerSpan.Status().Code == codes.Error {
		t.Errorf("register span status = %v, want non-error", registerSpan.Status().Code)
	}
}

func TestCommitSpansFlagBusinessAndTransportFailures(t *testing.T) {
	cases := []struct {
		name          string
		commit        func(string) (*cubeboxv1.CommitSandboxResponse, error)
		wantRetCode   bool
		wantRunStatus codes.Code
	}{
		{
			name: "cubelet business ret",
			commit: func(string) (*cubeboxv1.CommitSandboxResponse, error) {
				return &cubeboxv1.CommitSandboxResponse{
					Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Conflict, RetMsg: "busy"},
				}, nil
			},
			wantRetCode:   true,
			wantRunStatus: codes.Error,
		},
		{
			name: "cubelet transport error",
			commit: func(string) (*cubeboxv1.CommitSandboxResponse, error) {
				return nil, errors.New("connection refused")
			},
			wantRunStatus: codes.Error,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := installSpanRecorder(t)
			writes := stubCommitJob(t, commitJobStubs{commit: tc.commit})

			createReq, storedReq := newCommitFixtureRequests()
			rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
			defer rootSpan.End()

			runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
			flush()

			if got := writes.finalStatus(); got != JobStatusFailed {
				t.Errorf("final job status = %q, want %q", got, JobStatusFailed)
			}
			runSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitRun)
			if runSpan.Status().Code != tc.wantRunStatus {
				t.Errorf("run span status = %v, want %v", runSpan.Status().Code, tc.wantRunStatus)
			}
			snapshotSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitSnapshot)
			if snapshotSpan.Status().Code != codes.Error {
				t.Errorf("snapshot span status = %v, want Error", snapshotSpan.Status().Code)
			}
			_, haveRetCode := attrOf(snapshotSpan, telemetry.AttrRetCode)
			if haveRetCode != tc.wantRetCode {
				t.Errorf("snapshot span %s present = %t, want %t", telemetry.AttrRetCode, haveRetCode, tc.wantRetCode)
			}
			if got := spanCount(rec, telemetry.SpanTemplateCommitRegister); got != 0 {
				t.Errorf("register spans = %d, want 0 (fails before registration)", got)
			}
		})
	}
}

func TestCommitPanicFinalizesSpans(t *testing.T) {
	cases := []struct {
		name       string
		stubs      commitJobStubs
		wantStatus map[string]codes.Code
	}{
		{
			name: "panic while the snapshot span is still open",
			stubs: commitJobStubs{
				commit: func(string) (*cubeboxv1.CommitSandboxResponse, error) { panic("rpc exploded") },
			},
			wantStatus: map[string]codes.Code{
				telemetry.SpanTemplateCommitRun:      codes.Error,
				telemetry.SpanTemplateCommitSnapshot: codes.Error,
			},
		},
		{
			name: "panic during registration",
			stubs: commitJobStubs{
				create: func(context.Context, string, *sandboxtypes.CreateCubeSandboxReq, string, string) error {
					panic("definition exploded")
				},
			},
			wantStatus: map[string]codes.Code{
				telemetry.SpanTemplateCommitRun:      codes.Error,
				telemetry.SpanTemplateCommitSnapshot: codes.Unset,
				telemetry.SpanTemplateCommitRegister: codes.Error,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := installSpanRecorder(t)
			writes := stubCommitJob(t, tc.stubs)

			createReq, storedReq := newCommitFixtureRequests()
			rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
			defer rootSpan.End()

			runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
			flush()

			if got := writes.finalStatus(); got != JobStatusFailed {
				t.Errorf("final job status = %q, want %q", got, JobStatusFailed)
			}
			for name, want := range tc.wantStatus {
				span := spanNamed(t, rec, name)
				if span.Status().Code != want {
					t.Errorf("%s status = %v, want %v", name, span.Status().Code, want)
				}
			}
		})
	}
}

func TestCommitPhaseUpdatesOccurInsideTheirSpans(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	var snapshotWriteAt, registerWriteAt time.Time
	stubCommitJob(t, commitJobStubs{
		updateJob: func(fields map[string]any) error {
			if fields["status"] == JobStatusRunning && fields["phase"] == JobPhaseSnapshotting {
				snapshotWriteAt = time.Now()
			}
			if fields["phase"] == JobPhaseRegistering && fields["progress"] == 70 {
				registerWriteAt = time.Now()
			}
			return nil
		},
	})

	createReq, storedReq := newCommitFixtureRequests()
	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
	flush()

	if snapshotWriteAt.IsZero() {
		t.Fatal("initial RUNNING/SNAPSHOTTING job write was not recorded")
	}
	if registerWriteAt.IsZero() {
		t.Fatal("REGISTERING/progress70 job write was not recorded")
	}
	snapshotSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitSnapshot)
	if snapshotWriteAt.Before(snapshotSpan.StartTime()) || snapshotWriteAt.After(snapshotSpan.EndTime()) {
		t.Errorf("initial job write at %s is outside snapshot span [%s, %s]", snapshotWriteAt, snapshotSpan.StartTime(), snapshotSpan.EndTime())
	}
	registerSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitRegister)
	if registerWriteAt.Before(registerSpan.StartTime()) || registerWriteAt.After(registerSpan.EndTime()) {
		t.Errorf("registering job write at %s is outside register span [%s, %s]", registerWriteAt, registerSpan.StartTime(), registerSpan.EndTime())
	}
}

func TestCommitCleanupSpanCoversRollback(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	writes := stubCommitJob(t, commitJobStubs{
		create: func(context.Context, string, *sandboxtypes.CreateCubeSandboxReq, string, string) error {
			return errors.New("definition write failed")
		},
	})

	createReq, storedReq := newCommitFixtureRequests()
	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
	flush()

	if got := writes.finalStatus(); got != JobStatusFailed {
		t.Errorf("final job status = %q, want %q", got, JobStatusFailed)
	}
	runSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitRun)
	if runSpan.Status().Code != codes.Error {
		t.Errorf("run span status = %v, want Error", runSpan.Status().Code)
	}
	registerSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitRegister)
	cleanupSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitCleanup)
	if cleanupSpan.Parent().SpanID() != registerSpan.SpanContext().SpanID() {
		t.Errorf("cleanup span parent = %s, want register span %s", cleanupSpan.Parent().SpanID(), registerSpan.SpanContext().SpanID())
	}
	assertSpanStrAttr(t, cleanupSpan, telemetry.AttrTemplateID, "tpl-commit-1")
}

func TestCommitCleanupSpanFlagsBusinessRetWithoutChangingRollback(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	writes := stubCommitJob(t, commitJobStubs{
		create: func(context.Context, string, *sandboxtypes.CreateCubeSandboxReq, string, string) error {
			return errors.New("definition write failed")
		},
		cleanup: func(context.Context, string, *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
			return &cubeboxv1.CleanupTemplateResponse{
				Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Conflict, RetMsg: "busy"},
			}, nil
		},
	})

	createReq, storedReq := newCommitFixtureRequests()
	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
	flush()

	cleanupSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitCleanup)
	if cleanupSpan.Status().Code != codes.Error {
		t.Errorf("cleanup span status = %v, want Error for a non-success Ret", cleanupSpan.Status().Code)
	}
	if msg := writes.finalErrorMessage(); !strings.Contains(msg, "definition write failed") {
		t.Errorf("job error_message = %q, want the original cause", msg)
	}
}

func TestSubmitSpanCoversEarlyRejection(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	origDB := store.db
	store.db = &gorm.DB{}
	t.Cleanup(func() { store.db = origDB })

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	if _, err := SubmitTemplateCommit(rootCtx, "   ", "sb-1", "node-a", "10.0.0.1", "tpl-out", nil); err == nil {
		t.Fatal("SubmitTemplateCommit() = nil, want requestID guard error")
	}
	flush()

	submitSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitSubmit)
	if submitSpan.Status().Code != codes.Error {
		t.Errorf("submit span status = %v, want Error", submitSpan.Status().Code)
	}
	if submitSpan.Parent().SpanID() != rootSpan.SpanContext().SpanID() {
		t.Errorf("submit span parent = %s, want root %s", submitSpan.Parent().SpanID(), rootSpan.SpanContext().SpanID())
	}
	if kv, ok := attrOf(submitSpan, telemetry.AttrReused); !ok || kv.Value.AsBool() {
		t.Errorf("submit span %s = %v (present=%t), want false", telemetry.AttrReused, kv.Value.AsBool(), ok)
	}
	if _, ok := attrOf(submitSpan, telemetry.AttrJobID); ok {
		t.Error("a rejected submit must not report a job id")
	}
}

func TestSubmitSpanAbsentWithoutTrace(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	origDB := store.db
	store.db = &gorm.DB{}
	t.Cleanup(func() { store.db = origDB })

	if _, err := SubmitTemplateCommit(context.Background(), "   ", "sb-1", "node-a", "10.0.0.1", "tpl-out", nil); err == nil {
		t.Fatal("SubmitTemplateCommit() = nil, want requestID guard error")
	}
	flush()

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("untraced submit emitted %d span(s), want 0", len(got))
	}
}

func TestCommitJobWithoutTraceEmitsNoSpans(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubCommitJob(t, commitJobStubs{})

	createReq, storedReq := newCommitFixtureRequests()
	runTemplateCommitJob(context.Background(), "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
	flush()

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("untraced commit job emitted %d span(s), want 0", len(got))
	}
}

func spanStrings(span sdktrace.ReadOnlySpan) []string {
	out := []string{span.Status().Description}
	for _, kv := range span.Attributes() {
		out = append(out, kv.Value.Emit())
	}
	for _, ev := range span.Events() {
		out = append(out, ev.Name)
		for _, kv := range ev.Attributes {
			out = append(out, kv.Value.Emit())
		}
	}
	return out
}

func assertNoSpanContains(t *testing.T, rec *spanRecorder, needle string) {
	t.Helper()
	for _, span := range rec.snapshot() {
		for _, text := range spanStrings(span) {
			if strings.Contains(text, needle) {
				t.Errorf("span %s exported %q, which contains raw error text %q", span.Name(), text, needle)
			}
		}
	}
}

func TestCommitCleanupWriteFailureFlagsSpanWithoutLeakingError(t *testing.T) {
	const secret = "dsn=user:hunter2@db:5432"
	rec, flush := installSpanRecorder(t)
	writes := stubCommitJob(t, commitJobStubs{
		create: func(context.Context, string, *sandboxtypes.CreateCubeSandboxReq, string, string) error {
			return errors.New("definition write failed")
		},
		updateJob: func(fields map[string]any) error {
			if fields["template_status"] == StatusFailed {
				return errors.New(secret)
			}
			return nil
		},
	})

	createReq, storedReq := newCommitFixtureRequests()
	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
	flush()

	if got := writes.finalStatus(); got != JobStatusFailed {
		t.Errorf("final job status = %q, want %q", got, JobStatusFailed)
	}
	cleanupSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitCleanup)
	if cleanupSpan.Status().Code != codes.Error {
		t.Errorf("cleanup span status = %v, want Error when its DB write fails", cleanupSpan.Status().Code)
	}
	assertNoSpanContains(t, rec, secret)
}

func TestCommitReadyWriteFailureFlagsRunSpanButKeepsBusinessWrite(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	writes := stubCommitJob(t, commitJobStubs{
		updateJob: func(fields map[string]any) error {
			if fields["status"] == JobStatusReady {
				return errors.New("job row update failed")
			}
			return nil
		},
	})

	createReq, storedReq := newCommitFixtureRequests()
	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	runTemplateCommitJob(rootCtx, "job-1", "sb-1", "node-a", "10.0.0.1", createReq, storedReq)
	flush()

	if got := writes.finalStatus(); got != JobStatusReady {
		t.Errorf("recorded final job status = %q, want the business write %q to still happen", got, JobStatusReady)
	}
	runSpan := spanNamed(t, rec, telemetry.SpanTemplateCommitRun)
	if runSpan.Status().Code != codes.Error {
		t.Errorf("run span status = %v, want Error when the final job write fails", runSpan.Status().Code)
	}
}
