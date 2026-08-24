// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestInitLoggerProduction(t *testing.T) {
	logger, err := InitLogger(false)
	require.NoError(t, err)
	require.NotNil(t, logger)
	// Sync on /dev/stdout returns "invalid argument" in non-TTY environments; that is expected.
	_ = logger.Sync()
}

func TestInitLoggerDebug(t *testing.T) {
	logger, err := InitLogger(true)
	require.NoError(t, err)
	require.NotNil(t, logger)
	_ = logger.Sync()
}

func TestWithTraceID(t *testing.T) {
	logger, err := InitLogger(false)
	require.NoError(t, err)

	enriched := WithTraceID(logger, "trace-abc-123")
	require.NotNil(t, enriched)

	// Verify the logger can log without panicking.
	enriched.Info("test message with trace", zap.String("Key", "value"))
}

func TestInitTracing(t *testing.T) {
	provider, err := InitTracing("eviction-webhook")
	require.NoError(t, err)
	require.NotNil(t, provider)
}

func TestGetTracer(t *testing.T) {
	provider, err := InitTracing("eviction-webhook")
	require.NoError(t, err)

	tracer := GetTracer(provider, "test-tracer")
	require.NotNil(t, tracer)
}

func TestTracerStartReturnsSpan(t *testing.T) {
	provider, err := InitTracing("eviction-webhook")
	require.NoError(t, err)

	tracer := GetTracer(provider, "test-tracer")
	ctx, span := tracer.Start(context.Background(), "test-operation")
	require.NotNil(t, ctx)
	require.NotNil(t, span)

	// End must not panic.
	span.End()
}
