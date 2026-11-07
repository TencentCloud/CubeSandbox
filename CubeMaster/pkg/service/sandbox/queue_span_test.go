// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
)

type queueSpanRecorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *queueSpanRecorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *queueSpanRecorder) Shutdown(context.Context) error { return nil }

func (r *queueSpanRecorder) snapshot() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdktrace.ReadOnlySpan, len(r.spans))
	copy(out, r.spans)
	return out
}

func installQueueSpanRecorder(t *testing.T) (*queueSpanRecorder, func()) {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := &queueSpanRecorder{}
	shutdown, err := telemetry.SetupWithExporter(rec, "cube-test")
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

func TestQueueSpanEndsExactlyOnceWithoutWaitAttribute(t *testing.T) {
	rec, flush := installQueueSpanRecorder(t)

	c := &createSandboxContext{}
	_, c.queueSpan = telemetry.Start(context.Background(), telemetry.SpanCreateQueue)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.endQueueSpan()
		}()
	}
	wg.Wait()
	flush()

	spans := rec.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want the queue span ended exactly once, got %d span(s)", len(spans))
	}
	for _, kv := range spans[0].Attributes() {
		if string(kv.Key) == telemetry.AttrWaitMS {
			t.Fatalf("queue span must not carry %s: its duration already measures the wait", telemetry.AttrWaitMS)
		}
	}
	(&createSandboxContext{}).endQueueSpan()
}
