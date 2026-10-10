// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telemetry"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type scopeSpanRecorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *scopeSpanRecorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *scopeSpanRecorder) Shutdown(context.Context) error { return nil }

func (r *scopeSpanRecorder) snapshot() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdktrace.ReadOnlySpan, len(r.spans))
	copy(out, r.spans)
	return out
}

func installScopeRecorder(t *testing.T) (*scopeSpanRecorder, func()) {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := &scopeSpanRecorder{}
	shutdown, err := telemetry.SetupWithExporter(rec, "cubelet-test")
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
	t.Cleanup(func() {
		flush()
		otel.SetTracerProvider(prev)
	})
	return rec, flush
}

func newScopeEngine(do string) *Engine {
	e := &Engine{}
	e.AddFlow(do, &Workflow{Name: do, Limiter: semaphore.NewLimiter(8)})
	return e
}

type stubFlow struct{ id string }

func (f stubFlow) ID() string                                     { return f.id }
func (f stubFlow) Init(context.Context, *InitInfo) error          { return nil }
func (f stubFlow) Create(context.Context, *CreateContext) error   { return nil }
func (f stubFlow) Destroy(context.Context, *DestroyContext) error { return nil }
func (f stubFlow) CleanUp(context.Context, *CleanContext) error   { return nil }

func newStepEngine(do string) *Engine {
	e := &Engine{}
	wf := &Workflow{Name: do, Limiter: semaphore.NewLimiter(8)}
	wf.AppendStep(&Step{Name: "step-1", Actions: []Flow{stubFlow{id: "action-1"}}})
	e.AddFlow(do, wf)
	return e
}

func runFlow(t *testing.T, e *Engine, do string, ctx context.Context) error {
	t.Helper()
	switch do {
	case flow_init:
		return e.Init(ctx, &InitInfo{})
	case flow_create:
		return e.Create(ctx, &CreateContext{})
	case flow_destroy:
		return e.Destroy(ctx, &DestroyContext{})
	case flow_cleanup:
		return e.CleanUp(ctx, &CleanContext{})
	default:
		t.Fatalf("unknown flow %q", do)
		return nil
	}
}

func tracedCtx() context.Context {
	return CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: "req-1"})
}

func TestRunOpensNoSpanOutsideATrace(t *testing.T) {
	rec, flush := installScopeRecorder(t)

	ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: "req-1"})
	if err := newScopeEngine(flow_init).Init(ctx, &InitInfo{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	flush()

	if spans := rec.snapshot(); len(spans) != 0 {
		t.Fatalf("init outside a trace must open no span, got %d", len(spans))
	}
}

func TestRunKeepsCreateSpansUnderTheParent(t *testing.T) {
	rec, flush := installScopeRecorder(t)

	ctx, parent := telemetry.Start(context.Background(), telemetry.SpanCreate)
	parent.End()
	if err := newScopeEngine(flow_create).Create(ctx, &CreateContext{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	flush()

	spans := rec.snapshot()
	var wf sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == telemetry.SpanWorkflow {
			wf = span
		}
	}
	if wf == nil {
		t.Fatalf("create must still open a %s span, got %d span(s)", telemetry.SpanWorkflow, len(spans))
	}
	if wf.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("workflow parent span id = %s, want %s", wf.Parent().SpanID(), parent.SpanContext().SpanID())
	}
}

func TestRunIgnoresUnrelatedFlowsWithAValidParent(t *testing.T) {
	for _, do := range []string{flow_init, flow_destroy, flow_cleanup} {
		t.Run(do, func(t *testing.T) {
			rec, flush := installScopeRecorder(t)

			ctx, parent := telemetry.Start(tracedCtx(), telemetry.SpanCreate)
			if err := runFlow(t, newStepEngine(do), do, ctx); err != nil {
				t.Fatalf("%s: %v", do, err)
			}
			if !parent.IsRecording() {
				t.Fatal("engine ended the caller's span")
			}
			parent.End()
			flush()

			spans := rec.snapshot()
			if len(spans) != 1 || spans[0].SpanContext().SpanID() != parent.SpanContext().SpanID() {
				t.Fatalf("%s outside the create chain must open no span, got %d", do, len(spans))
			}
			if code := spans[0].Status().Code; code == codes.Error {
				t.Errorf("engine marked the caller's span failed: %v", code)
			}
		})
	}
}

func TestFailoverDestroyStaysInTheCreateTrace(t *testing.T) {
	rec, flush := installScopeRecorder(t)

	parentCtx, parent := telemetry.Start(context.Background(), telemetry.SpanCreate)
	parent.End()
	ctx := CubeLog.WithRequestTrace(constants.WithFailoverOperation(parentCtx),
		&CubeLog.RequestTrace{RequestID: "req-1"})

	if err := newStepEngine(flow_destroy).Destroy(ctx, &DestroyContext{}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	flush()

	spans := rec.snapshot()
	names := map[string]bool{}
	for _, span := range spans {
		if span.SpanContext().TraceID() != parent.SpanContext().TraceID() {
			t.Errorf("span %s trace = %s, want %s", span.Name(), span.SpanContext().TraceID(), parent.SpanContext().TraceID())
		}
		names[span.Name()] = true
	}
	for _, want := range []string{telemetry.SpanWorkflow, telemetry.SpanStep, telemetry.SpanAction} {
		if !names[want] {
			t.Errorf("failover destroy is missing the %s span, got %v", want, names)
		}
	}
}
