// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"testing"
	"time"
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
func captureUpdateJob(t *testing.T) *struct {
	ctx    context.Context
	jobID  string
	values map[string]any
	called bool
} {
	t.Helper()
	captured := &struct {
		ctx    context.Context
		jobID  string
		values map[string]any
		called bool
	}{}
	orig := updateTemplateImageJobFn
	updateTemplateImageJobFn = func(ctx context.Context, jobID string, values map[string]any) error {
		captured.ctx = ctx
		captured.jobID = jobID
		captured.values = values
		captured.called = true
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
	if err := captured.ctx.Err(); err != nil {
		t.Fatalf("markForwardBuildJobFailed must use a fresh context for the DB write, got err=%v", err)
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
	if captured.ctx.Err() != nil {
		t.Fatalf("DB-write ctx should be live, got err=%v", captured.ctx.Err())
	}
}

// Sanity: with a healthy caller ctx, the write still happens with a live ctx.
func TestForwardBuildJobFailedHealthyCaller(t *testing.T) {
	captured := captureUpdateJob(t)

	markForwardBuildJobFailed(context.Background(), "job-3", "ok")

	if !captured.called {
		t.Fatal("expected the DB write to be invoked")
	}
	if captured.ctx.Err() != nil {
		t.Fatalf("DB-write ctx should be live, got err=%v", captured.ctx.Err())
	}
}
