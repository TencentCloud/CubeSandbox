// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

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

func setupTracer(t *testing.T) (*spanRecorder, func()) {
	t.Helper()
	rec := &spanRecorder{}
	shutdown, err := telemetry.SetupWithExporter(rec, "cubetemplatecenter-test")
	if err != nil {
		t.Fatalf("SetupWithExporter: %v", err)
	}
	var once sync.Once
	flush := func() {
		once.Do(func() { _ = shutdown(context.Background()) })
	}
	t.Cleanup(flush)
	return rec, flush
}

func attrIsTrue(span sdktrace.ReadOnlySpan, key string) bool {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsBool()
		}
	}
	return false
}

func TestSubmitContextCarriesTraceWithoutRequestCancellation(t *testing.T) {
	setupTracer(t)

	dispatchCtx, dispatchSpan := telemetry.Start(context.Background(), telemetry.SpanTemplateImageDispatch)
	defer dispatchSpan.End()
	requestCtx, cancelRequest := context.WithCancel(dispatchCtx)

	buildCtxCh := make(chan context.Context, 1)
	e := NewExecutor(0)
	e.lookupJob = func(context.Context, string, *types.CreateTemplateFromImageReq) error { return nil }
	e.build = func(ctx context.Context, _ string, _ *types.CreateTemplateFromImageReq, _, _ string, _ []byte) error {
		buildCtxCh <- ctx
		<-ctx.Done() // only Shutdown may unblock this
		return ctx.Err()
	}
	t.Cleanup(e.Shutdown)

	if err := e.SubmitContext(requestCtx, "job-1", req(), "", "", nil); err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	buildCtx := <-buildCtxCh

	cancelRequest()
	time.Sleep(10 * time.Millisecond) // let a wrong cancellation propagate

	if err := buildCtx.Err(); err != nil {
		t.Fatalf("the answered submit request canceled the build: %v", err)
	}
	got := trace.SpanContextFromContext(buildCtx)
	if got.TraceID() != dispatchSpan.SpanContext().TraceID() || got.SpanID() != dispatchSpan.SpanContext().SpanID() {
		t.Errorf("build parent = %s/%s, want the dispatch span %s/%s",
			got.TraceID(), got.SpanID(), dispatchSpan.SpanContext().TraceID(), dispatchSpan.SpanContext().SpanID())
	}

	done := make(chan struct{})
	go func() {
		e.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not cancel the in-flight build")
	}
	if buildCtx.Err() == nil {
		t.Fatal("the build must be cancelled by Shutdown")
	}
}
