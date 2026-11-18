// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/tcclient"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func attrBool(t *testing.T, attrs []attribute.KeyValue, key string) bool {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsBool()
		}
	}
	t.Fatalf("span has no attribute %q", key)
	return false
}

func attrInt(t *testing.T, attrs []attribute.KeyValue, key string) int64 {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsInt64()
		}
	}
	t.Fatalf("span has no attribute %q", key)
	return 0
}

type spanRecorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *spanRecorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *spanRecorder) Shutdown(context.Context) error { return nil }

func (r *spanRecorder) named(name string) []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []sdktrace.ReadOnlySpan
	for _, s := range r.spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func setupSpanRecorder(t *testing.T) (*spanRecorder, func()) {
	t.Helper()
	rec := &spanRecorder{}
	shutdown, err := telemetry.SetupWithExporter(rec, "cubemaster-test")
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
	t.Cleanup(flush)
	return rec, flush
}

func TestForwardBuildJobToTemplateCenterDispatchContinuesSubmitTrace(t *testing.T) {
	rec, flush := setupSpanRecorder(t)
	t.Setenv(config.EnvTemplateCenterAddr, "http://tc.invalid:8089")
	stubSubmitBuildJob(t, func(int) error { return nil })

	submitCtx, submitSpan := telemetry.Start(context.Background(), telemetry.SpanTemplateImageSubmit)
	submitSpan.End()

	forwardBuildJobToTemplateCenter("job-1", &types.CreateTemplateFromImageReq{TemplateID: "tpl-1"}, "", nil, trace.SpanContextFromContext(submitCtx))
	flush()

	dispatch := rec.named(telemetry.SpanTemplateImageDispatch)
	if len(dispatch) != 1 {
		t.Fatalf("want 1 dispatch span, got %d", len(dispatch))
	}
	if dispatch[0].Parent().SpanID() != submitSpan.SpanContext().SpanID() {
		t.Errorf("dispatch parent = %s, want the submit span %s",
			dispatch[0].Parent().SpanID(), submitSpan.SpanContext().SpanID())
	}
	if dispatch[0].SpanContext().TraceID() != submitSpan.SpanContext().TraceID() {
		t.Error("dispatch did not continue the submit trace")
	}

	attempts := rec.named(telemetry.SpanTemplateSubmitAttempt)
	if len(attempts) != 1 {
		t.Fatalf("want 1 attempt span, got %d", len(attempts))
	}
	if attempts[0].Parent().SpanID() != dispatch[0].SpanContext().SpanID() {
		t.Errorf("attempt parent = %s, want the dispatch span %s",
			attempts[0].Parent().SpanID(), dispatch[0].SpanContext().SpanID())
	}
}

func TestSubmitBuildJobWithRetryTreatsDuplicate409AsSuccess(t *testing.T) {
	rec, flush := setupSpanRecorder(t)
	stubSubmitBuildJob(t, func(int) error {
		return &tcclient.StatusError{StatusCode: http.StatusConflict, Body: "already in flight"}
	})

	if err := submitBuildJobWithRetry(context.Background(), "http://tc.invalid", "job-2", nil, "", "", nil); err != nil {
		t.Fatalf("409 must be treated as success, got %v", err)
	}
	flush()

	attempts := rec.named(telemetry.SpanTemplateSubmitAttempt)
	if len(attempts) != 1 {
		t.Fatalf("want 1 attempt span, got %d", len(attempts))
	}
	if attempts[0].Status().Code != codes.Unset {
		t.Errorf("duplicate attempt status = %v, want Unset", attempts[0].Status().Code)
	}
	if !attrBool(t, attempts[0].Attributes(), telemetry.AttrDuplicate) {
		t.Errorf("attempt must carry %s=true", telemetry.AttrDuplicate)
	}
}

func TestSubmitBuildJobWithRetrySpansAttemptsAndBackoff(t *testing.T) {
	rec, flush := setupSpanRecorder(t)
	shrinkRetryDelays(t)

	stubSubmitBuildJob(t, func(attempt int) error {
		if attempt == 1 {
			return &tcclient.StatusError{StatusCode: http.StatusServiceUnavailable, Body: "tc busy"}
		}
		return nil
	})

	if err := submitBuildJobWithRetry(context.Background(), "http://tc.invalid", "job-3", nil, "", "", nil); err != nil {
		t.Fatalf("transient failure must be retried, got %v", err)
	}
	flush()

	if got := len(rec.named(telemetry.SpanTemplateSubmitAttempt)); got != 2 {
		t.Fatalf("want 2 attempt spans, got %d", got)
	}
	backoffs := rec.named(telemetry.SpanTemplateDispatchBackoff)
	if len(backoffs) != 1 {
		t.Fatalf("want 1 backoff span, got %d", len(backoffs))
	}
	if got := attrInt(t, backoffs[0].Attributes(), telemetry.AttrAttempt); got != 1 {
		t.Errorf("%s = %d, want 1", telemetry.AttrAttempt, got)
	}
	if got := attrInt(t, backoffs[0].Attributes(), telemetry.AttrWaitMS); got != 1 {
		t.Errorf("%s = %d, want 1", telemetry.AttrWaitMS, got)
	}
}

func TestSubmitBuildJobWithRetryStopsOnPermanentRejection(t *testing.T) {
	rec, flush := setupSpanRecorder(t)
	stubSubmitBuildJob(t, func(int) error {
		return &tcclient.StatusError{StatusCode: http.StatusBadRequest, Body: "bad request"}
	})

	err := submitBuildJobWithRetry(context.Background(), "http://tc.invalid", "job-4", nil, "", "", nil)
	if err == nil {
		t.Fatal("a 400 must surface as an error")
	}
	var statusErr *tcclient.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("error = %v, want the 400 StatusError", err)
	}
	flush()

	if got := len(rec.named(telemetry.SpanTemplateSubmitAttempt)); got != 1 {
		t.Fatalf("want 1 attempt span, got %d", got)
	}
	if got := len(rec.named(telemetry.SpanTemplateDispatchBackoff)); got != 0 {
		t.Fatalf("want no backoff span, got %d", got)
	}
}
