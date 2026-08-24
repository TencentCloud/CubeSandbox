// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package telemetry provides logging and tracing initialization for eviction-webhook.
package telemetry

import (
	"context"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// InitLogger initializes a Zap logger with the specified debug level.
func InitLogger(debug bool) (*zap.Logger, error) {
	var config zap.Config

	if debug {
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		config = zap.NewProductionConfig()
		config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}

	// Configure output
	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.CallerKey = "caller"

	return config.Build()
}

// WithTraceID adds a trace ID to the logger.
func WithTraceID(logger *zap.Logger, traceID string) *zap.Logger {
	return logger.With(zap.String("TraceID", traceID))
}

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
