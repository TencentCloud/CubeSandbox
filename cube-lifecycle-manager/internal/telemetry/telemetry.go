// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package telemetry provides opt-in tracing for Lifecycle Manager.
package telemetry

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	SpanSweepKill      = "clm.sweep.kill"
	SpanCubeMasterKill = "clm.cubemaster.kill"

	AttrSandboxID    = "cube.sandbox_id"
	AttrInstanceType = "cube.instance_type"
	AttrKillReason   = "cube.kill_reason"
	AttrIdleMS       = "cube.idle_ms"
	AttrTimeoutMS    = "cube.timeout_ms"
	AttrRetCode      = "cube.ret_code"
)

// Setup installs the OTLP/gRPC provider when an endpoint is configured.
// The exporter resolves signal-specific and generic endpoint settings.
func Setup(ctx context.Context) (func(context.Context) error, error) {
	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) == "" &&
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" {
		return nil, nil
	}
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	service := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if service == "" {
		service = "cube-lifecycle-manager"
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer("github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager").Start(ctx, name, opts...)
}

func End(span trace.Span, err error) {
	if err != nil {
		// Request errors can include response bodies.
		span.SetStatus(codes.Error, "")
	}
	span.End()
}
