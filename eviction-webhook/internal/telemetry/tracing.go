// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package telemetry provides logging and tracing initialization for eviction-webhook.
package telemetry

import (
	"context"
)

// TracerProvider is a simple tracer provider interface.
type TracerProvider interface {
	Tracer(string) Tracer
}

// Tracer is a simple tracer interface.
type Tracer interface {
	Start(ctx context.Context, name string, opts ...interface{}) (context.Context, Span)
}

// Span is a simple span interface.
type Span interface {
	End()
}

// noopProvider is a no-op TracerProvider for development.
type noopProvider struct{}

func (p *noopProvider) Tracer(_ string) Tracer {
	return &noopTracer{}
}

// noopTracer is a no-op Tracer.
type noopTracer struct{}

func (n *noopTracer) Start(ctx context.Context, _ string, _ ...interface{}) (context.Context, Span) {
	return ctx, &noopSpan{}
}

type noopSpan struct{}

func (n *noopSpan) End() {}

// InitTracing initializes a tracer provider.
// Returns a no-op implementation; wire up Jaeger via environment variables in production.
func InitTracing(_ string) (TracerProvider, error) {
	return &noopProvider{}, nil
}

// GetTracer returns a tracer from the provider.
func GetTracer(provider TracerProvider, name string) Tracer {
	return provider.Tracer(name)
}
