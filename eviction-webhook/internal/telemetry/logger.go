// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package telemetry provides logging and tracing initialization for eviction-webhook.
package telemetry

import (
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
