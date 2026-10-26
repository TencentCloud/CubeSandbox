// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

// These tests verify markForwardBuildJobFailed's core invariant: it must mark
// the job FAILED using a context derived from context.Background(), NOT from
// the caller's deadline-bound callCtx. Otherwise, by the time the submit's 60s
// deadline fires, the FAILED write is dropped and the job stays PENDING.
//
// They stub the LOWEST seam (updateTemplateImageJobFn), so the real
// markForwardBuildJobFailed runs and the ctx it actually produces for the DB
// write is what gets captured. Stubbing markForwardBuildJobFailed itself would
// record the caller's ctx and prove nothing -- that was the bug in the first
// version of these tests.

// captureUpdateJob swaps the DB-write seam for a recorder and returns the
// captured call, restoring the original when the test ends.
//
// errAtCall records ctx.Err() synchronously INSIDE the stub, at the moment
// updateTemplateImageJobFn is invoked. This matters because
// markForwardBuildJobFailed defers its own cancel() on the context it
// builds; by the time the function returns to the test, that deferred
// cancel has already fired, so captured.ctx.Err() checked AFTER the call
// would always read "context canceled" regardless of whether the write
// actually happened on a live context. errAtCall captures the true
// liveness at write-time instead.
func captureUpdateJob(t *testing.T) *struct {
	ctx       context.Context
	jobID     string
	values    map[string]any
	called    bool
	errAtCall error
} {
	t.Helper()
	captured := &struct {
		ctx       context.Context
		jobID     string
		values    map[string]any
		called    bool
		errAtCall error
	}{}
	orig := updateTemplateImageJobFn
	updateTemplateImageJobFn = func(ctx context.Context, jobID string, values map[string]any) error {
		captured.ctx = ctx
		captured.jobID = jobID
		captured.values = values
		captured.called = true
		captured.errAtCall = ctx.Err()
		return nil
	}
	t.Cleanup(func() { updateTemplateImageJobFn = orig })
	return captured
}

// A caller ctx that is already past its deadline: the exact shape of the bug
// being guarded against. The real markForwardBuildJobFailed must still hand a
// LIVE ctx to the DB write.
func TestForwardBuildJobFailedUsesFreshContext(t *testing.T) {
	captured := captureUpdateJob(t)

	parentCtx, parentCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	<-parentCtx.Done() // ensure the deadline has fired
	defer parentCancel()

	markForwardBuildJobFailed(parentCtx, "job-1", "boom")

	if !captured.called {
		t.Fatal("markForwardBuildJobFailed did not invoke the DB write")
	}
	if captured.ctx == nil {
		t.Fatal("no ctx captured from the DB write")
	}
	if captured.errAtCall != nil {
		t.Fatalf("markForwardBuildJobFailed must use a fresh context for the DB write, got err=%v", captured.errAtCall)
	}
	if captured.jobID != "job-1" {
		t.Fatalf("jobID = %q, want job-1", captured.jobID)
	}
	if captured.values["status"] != "FAILED" || captured.values["error_message"] != "boom" {
		t.Fatalf("values = %v, want status=FAILED error_message=boom", captured.values)
	}
}

// The DB-write ctx must not inherit values or cancellation from the caller's
// ctx. This pins the structural choice so a future refactor cannot silently
// reintroduce inheritance.
func TestForwardBuildJobFailedContextIsIndependent(t *testing.T) {
	captured := captureUpdateJob(t)

	type key struct{}
	parentCtx, parentCancel := context.WithTimeout(
		context.WithValue(context.Background(), key{}, "v"), 5*time.Second)
	defer parentCancel()

	markForwardBuildJobFailed(parentCtx, "job-2", "x")

	if !captured.called {
		t.Fatal("markForwardBuildJobFailed did not invoke the DB write")
	}
	if v, ok := captured.ctx.Value(key{}).(string); ok && v == "v" {
		t.Fatalf("DB-write ctx must not inherit caller values; got %q", v)
	}
	if captured.errAtCall != nil {
		t.Fatalf("DB-write ctx should be live, got err=%v", captured.errAtCall)
	}
}

// Sanity: with a healthy caller ctx, the write still happens with a live ctx.
func TestForwardBuildJobFailedHealthyCaller(t *testing.T) {
	captured := captureUpdateJob(t)

	markForwardBuildJobFailed(context.Background(), "job-3", "ok")

	if !captured.called {
		t.Fatal("expected the DB write to be invoked")
	}
	if captured.errAtCall != nil {
		t.Fatalf("DB-write ctx should be live, got err=%v", captured.errAtCall)
	}
}

// P2-11 regression: the templatecenter_enabled switch was removed from the
// codebase (remoteTemplateBuildEnabled always returns true; there is no
// local build fallback anymore), but the "missing endpoint" error message
// used to still say "templatecenter_enabled=true but ..." -- a stale,
// misleading reference to a switch that no longer exists. The message must
// name the actual missing env var instead.
func TestForwardBuildJobToTemplateCenterMissingEndpointMessageIsNotStale(t *testing.T) {
	t.Setenv(config.EnvTemplateCenterAddr, "")
	captured := captureUpdateJob(t)

	forwardBuildJobToTemplateCenter("job-missing-endpoint", nil, "", nil)

	if !captured.called {
		t.Fatal("expected the DB write to be invoked")
	}
	msg, _ := captured.values["error_message"].(string)
	if strings.Contains(msg, "templatecenter_enabled") {
		t.Fatalf("error_message must not reference the removed templatecenter_enabled switch, got %q", msg)
	}
	if !strings.Contains(msg, config.EnvTemplateCenterAddr) {
		t.Fatalf("error_message must name the missing env var %s, got %q", config.EnvTemplateCenterAddr, msg)
	}
}

// Same regression, for the redo forwarding path.
func TestForwardRedoBuildJobToTemplateCenterMissingEndpointMessageIsNotStale(t *testing.T) {
	t.Setenv(config.EnvTemplateCenterAddr, "")
	captured := captureUpdateJob(t)

	forwardRedoBuildJobToTemplateCenter("job-missing-endpoint-redo", "")

	if !captured.called {
		t.Fatal("expected the DB write to be invoked")
	}
	msg, _ := captured.values["error_message"].(string)
	if strings.Contains(msg, "templatecenter_enabled") {
		t.Fatalf("error_message must not reference the removed templatecenter_enabled switch, got %q", msg)
	}
	if !strings.Contains(msg, config.EnvTemplateCenterAddr) {
		t.Fatalf("error_message must name the missing env var %s, got %q", config.EnvTemplateCenterAddr, msg)
	}
}
