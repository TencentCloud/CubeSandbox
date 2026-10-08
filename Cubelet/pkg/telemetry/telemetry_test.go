// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type recordingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error { return nil }

func (e *recordingExporter) snapshot() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]sdktrace.ReadOnlySpan, len(e.spans))
	copy(out, e.spans)
	return out
}

func setupTest(t *testing.T) (*recordingExporter, func()) {
	t.Helper()
	exp := &recordingExporter{}
	shutdown, err := SetupWithExporter(exp, "cubelet-test")
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
	return exp, flush
}

func attr(t *testing.T, span sdktrace.ReadOnlySpan, key string) attribute.Value {
	t.Helper()
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	t.Fatalf("span %q has no attribute %q", span.Name(), key)
	return attribute.Value{}
}

func TestExtractGRPCContinuesTraceFromMetadata(t *testing.T) {
	exp, flush := setupTest(t)

	md := metadata.Pairs("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := ExtractGRPC(metadata.NewIncomingContext(context.Background(), md))
	_, span := Start(ctx, SpanCreate, trace.WithSpanKind(trace.SpanKindServer))
	EndWithCode(span, SuccessCode)
	flush()

	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	got := spans[0]
	if got.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", got.SpanKind())
	}
	if got.Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("parent trace id = %s, want the incoming traceparent trace id", got.Parent().TraceID())
	}
}

func TestExtractGRPCKeepsActiveSpanAsParent(t *testing.T) {
	exp, flush := setupTest(t)

	remote := metadata.Pairs("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	outerCtx := ExtractGRPC(metadata.NewIncomingContext(context.Background(), remote))
	callerCtx, caller := Start(outerCtx, SpanImageAppSnapshot)
	innerCtx := ExtractGRPC(metadata.NewIncomingContext(callerCtx, remote))
	_, inner := Start(innerCtx, SpanImageCreate)
	inner.End()
	caller.End()
	flush()

	for _, span := range exp.snapshot() {
		if span.Name() != SpanImageCreate {
			continue
		}
		if span.Parent().SpanID() != caller.SpanContext().SpanID() {
			t.Fatalf("in-process child parent = %s, want the active AppSnapshot span %s",
				span.Parent().SpanID(), caller.SpanContext().SpanID())
		}
		return
	}
	t.Fatal("inner span was not exported")
}

func TestEndWithCodeMarksBusinessFailure(t *testing.T) {
	exp, flush := setupTest(t)

	_, bad := Start(context.Background(), SpanCreate)
	EndWithCode(bad, int(errorcode.ErrorCode_CreateStorageFailed))
	_, ok := Start(context.Background(), SpanCreate)
	EndWithCode(ok, SuccessCode)
	flush()

	spans := exp.snapshot()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	failed, passed := 0, 0
	for _, span := range spans {
		if span.Status().Code == codes.Error {
			failed++
			if got := attr(t, span, AttrRetCode).AsInt64(); got != int64(errorcode.ErrorCode_CreateStorageFailed) {
				t.Errorf("%s = %d, want CreateStorageFailed", AttrRetCode, got)
			}
			continue
		}
		passed++
		if span.Status().Code != codes.Unset {
			t.Errorf("success rsp status = %v, want Unset", span.Status().Code)
		}
	}
	if failed != 1 || passed != 1 {
		t.Fatalf("want 1 failed and 1 successful span, got %d and %d", failed, passed)
	}
}

func TestEndMarksFailureWithoutLeakingErrorText(t *testing.T) {
	exp, flush := setupTest(t)

	_, span := Start(context.Background(), SpanCreate)
	End(span, errors.New("link reset"))
	flush()

	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("error span status = %v, want Error", spans[0].Status().Code)
	}
	if desc := spans[0].Status().Description; desc != statusError {
		t.Errorf("span status description = %q, want the generic %q", desc, statusError)
	}
	if events := spans[0].Events(); len(events) != 0 {
		t.Errorf("error must not be recorded as a span event, got %v", events)
	}
}

func TestSetupDisabledKeepsTracingOff(t *testing.T) {
	t.Setenv(EnvEndpoint, "")
	otel.SetTracerProvider(trace.NewNoopTracerProvider())

	shutdown, err := Setup(context.Background(), "cubelet-test")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown: %v", err)
	}

	if _, span := Start(context.Background(), SpanCreate); span.IsRecording() {
		t.Fatal("tracing must stay off when OTEL_EXPORTER_OTLP_ENDPOINT is unset")
	}
}

func TestStartIfTracedSuppressesSpanWithoutParent(t *testing.T) {
	exp, flush := setupTest(t)

	_, span := StartIfTraced(context.Background(), SpanWorkflow)
	span.SetAttributes(attribute.String(AttrStep, "noop"))
	if span.IsRecording() {
		t.Error("no parent span context: span must not record")
	}
	span.End()
	flush()

	if spans := exp.snapshot(); len(spans) != 0 {
		t.Fatalf("want no spans outside a trace, got %d", len(spans))
	}
}

func TestStartIfTracedKeepsSpanUnderParent(t *testing.T) {
	exp, flush := setupTest(t)

	ctx, parent := Start(context.Background(), SpanCreate)
	_, child := StartIfTraced(ctx, SpanWorkflow)
	child.End()
	parent.End()
	flush()

	for _, span := range exp.snapshot() {
		if span.Name() != SpanWorkflow {
			continue
		}
		if span.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("child parent span id = %s, want %s", span.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		return
	}
	t.Fatal("child span was not exported")
}

func TestDetachTraceKeepsParentButNotCancellation(t *testing.T) {
	exp, flush := setupTest(t)

	parentCtx, parent := Start(context.Background(), SpanCreate)
	parent.End()
	createCtx, cancel := context.WithCancel(parentCtx)
	cancel()

	base, baseCancel := context.WithTimeout(context.Background(), time.Minute)
	defer baseCancel()

	ctx := DetachTrace(base, createCtx)
	if err := ctx.Err(); err != nil {
		t.Fatalf("detached ctx inherited the create cancellation: %v", err)
	}
	_, child := Start(ctx, SpanWorkflow)
	child.End()
	flush()

	for _, span := range exp.snapshot() {
		if span.Name() != SpanWorkflow {
			continue
		}
		if span.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("cleanup parent span id = %s, want the create span %s",
				span.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		if span.SpanContext().TraceID() != parent.SpanContext().TraceID() {
			t.Error("cleanup did not continue the create trace")
		}
		return
	}
	t.Fatal("cleanup span was not exported")
}
