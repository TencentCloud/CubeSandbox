// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
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
	shutdown, err := SetupWithExporter(exp, "cubemaster-test")
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

func TestSetupDisabledKeepsTracingOff(t *testing.T) {
	t.Setenv(EnvEndpoint, "")
	otel.SetTracerProvider(trace.NewNoopTracerProvider())

	shutdown, err := Setup(context.Background(), "cubemaster-test")
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

func TestEndWithCodeMarksBusinessFailure(t *testing.T) {
	exp, flush := setupTest(t)

	_, ok := Start(context.Background(), SpanCreate)
	EndWithCode(ok, SuccessCode)

	_, bad := Start(context.Background(), SpanCreateCubelet)
	EndWithCode(bad, 500)

	flush()
	spans := exp.snapshot()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}

	for _, span := range spans {
		if span.Name() == SpanCreateCubelet {
			if span.Status().Code != codes.Error {
				t.Errorf("failed create span status = %v, want Error", span.Status().Code)
			}
			if got := attr(t, span, AttrRetCode).AsInt64(); got != 500 {
				t.Errorf("%s = %d, want 500", AttrRetCode, got)
			}
			continue
		}
		if span.Status().Code != codes.Unset {
			t.Errorf("successful span status = %v, want Unset", span.Status().Code)
		}
	}
}

func TestGinMiddlewareContinuesTraceAndNamesRoute(t *testing.T) {
	exp, flush := setupTest(t)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinMiddleware())
	r.POST("/cube/sandbox", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/cube/sandbox", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/cube/sandbox/list", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	r.ServeHTTP(httptest.NewRecorder(), req)

	for _, other := range []struct{ method, path string }{
		{http.MethodGet, "/cube/sandbox"},
		{http.MethodPost, "/cube/sandbox/list"},
	} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(other.method, other.path, nil))
	}
	flush()

	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want only the create request spanned, got %d spans", len(spans))
	}
	span := spans[0]
	if span.Name() != "POST /cube/sandbox" {
		t.Errorf("span name = %q, want the route pattern", span.Name())
	}
	if span.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", span.SpanKind())
	}
	if got := span.Parent().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("parent trace id = %s, want the incoming traceparent trace id", got)
	}
	if got := attr(t, span, "http.response.status_code").AsInt64(); got != http.StatusOK {
		t.Errorf("http.response.status_code = %d, want 200", got)
	}
}

func TestInjectGRPCWritesTraceparentForDownstream(t *testing.T) {
	exp, flush := setupTest(t)

	ctx, parent := Start(context.Background(), SpanCreate)
	parent.End()

	out := InjectGRPC(ctx)
	md, ok := metadata.FromOutgoingContext(out)
	if !ok {
		t.Fatal("InjectGRPC produced no outgoing metadata")
	}
	if len(md.Get("traceparent")) == 0 {
		t.Fatal("InjectGRPC wrote no traceparent")
	}

	childCtx := otel.GetTextMapPropagator().Extract(context.Background(), mdCarrier(md))
	_, child := Start(childCtx, SpanRegisterRef)
	child.End()

	flush()
	spans := exp.snapshot()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	for _, span := range spans {
		if span.Name() != SpanRegisterRef {
			continue
		}
		if span.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("child parent span id = %s, want %s", span.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		if span.SpanContext().TraceID() != parent.SpanContext().TraceID() {
			t.Error("child did not continue the parent trace")
		}
		return
	}
	t.Fatal("child span was not exported")
}

func TestStartIfTracedSuppressesSpanWithoutParent(t *testing.T) {
	exp, flush := setupTest(t)

	_, span := StartIfTraced(context.Background(), SpanTemplateDB)
	span.SetAttributes(attribute.String(AttrTable, "t_cube_snapshot"))
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
	_, child := StartIfTraced(ctx, SpanTemplateDB)
	child.End()
	parent.End()
	flush()

	for _, span := range exp.snapshot() {
		if span.Name() != SpanTemplateDB {
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
	requestCtx, cancel := context.WithCancel(parentCtx)
	cancel()

	base, baseCancel := context.WithTimeout(context.Background(), time.Minute)
	defer baseCancel()

	ctx := DetachTrace(base, requestCtx)
	if err := ctx.Err(); err != nil {
		t.Fatalf("detached ctx inherited the request cancellation: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("detached ctx dropped the base deadline")
	}
	_, child := StartIfTraced(ctx, SpanTemplateDB)
	child.End()
	flush()

	for _, span := range exp.snapshot() {
		if span.Name() != SpanTemplateDB {
			continue
		}
		if span.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("detached child parent span id = %s, want the request span %s",
				span.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		if span.SpanContext().TraceID() != parent.SpanContext().TraceID() {
			t.Error("detached child did not continue the request trace")
		}
		return
	}
	t.Fatal("detached child span was not exported")
}
