// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package telemetry provides tracing for sandbox creation.
package telemetry

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

const Scope = "github.com/tencentcloud/CubeSandbox/Cubelet"

const (
	EnvEndpoint    = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvServiceName = "OTEL_SERVICE_NAME"

	defaultServiceName = "cubelet"
)

const SuccessCode = int(errorcode.ErrorCode_Success)

const (
	SpanCreate           = "cubelet.create"
	SpanCreateLock       = "cubelet.create.lifecycle_lock"
	SpanWorkflow         = "cubelet.workflow"
	SpanStep             = "cubelet.workflow.step"
	SpanAction           = "cubelet.workflow.action"
	SpanProbe            = "cubelet.probe"
	SpanRuntimeContainer = "cubelet.runtime.container"
	SpanRuntimeTask      = "cubelet.runtime.task"
	SpanRuntimeStart     = "cubelet.runtime.start"
	SpanImageCreate      = "cubelet.image.create"
	SpanImageAppSnapshot = "cubelet.image.app_snapshot"
)

const (
	AttrRequestID    = "cube.request_id"
	AttrSandboxID    = "cube.sandbox_id"
	AttrTemplateID   = "cube.template_id"
	AttrArtifactID   = "cube.artifact_id"
	AttrInstanceType = "cube.instance_type"
	AttrRetCode      = "cube.ret_code"
	AttrWorkflow     = "cube.workflow"
	AttrStep         = "cube.step"
	AttrAction       = "cube.action"
)

type Shutdown func(context.Context) error

func Setup(ctx context.Context, service string) (Shutdown, error) {
	endpoint := strings.TrimSpace(os.Getenv(EnvEndpoint))
	if endpoint == "" {
		return noopShutdown, nil
	}
	if service == "" {
		service = defaultServiceName
	}
	if v := strings.TrimSpace(os.Getenv(EnvServiceName)); v != "" {
		service = v
	}
	var opts []otlptracegrpc.Option
	if strings.HasPrefix(strings.ToLower(endpoint), "http://") {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return SetupWithExporter(exp, service)
}

func SetupWithExporter(exp sdktrace.SpanExporter, service string) (Shutdown, error) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}

func noopShutdown(context.Context) error { return nil }

func Tracer() trace.Tracer { return otel.Tracer(Scope) }

// Start starts a span; nil contexts use Background.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	return Tracer().Start(ctx, name, opts...)
}

// StartIfTraced starts a span only inside an existing trace.
func StartIfTraced(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, trace.SpanFromContext(ctx)
	}
	return Start(ctx, name, opts...)
}

// DetachTrace carries only the trace parent from traceFrom into base.
func DetachTrace(base, traceFrom context.Context) context.Context {
	return trace.ContextWithSpanContext(base, trace.SpanContextFromContext(traceFrom))
}

// Error text can include credentials, so spans record only a generic status.
const statusError = "error"

func End(span trace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.SetStatus(codes.Error, statusError)
	}
	span.End()
}

func EndWithCode(span trace.Span, code int) {
	if span == nil {
		return
	}
	if code != SuccessCode {
		span.SetAttributes(attribute.Int(AttrRetCode, code))
		span.SetStatus(codes.Error, statusError)
	}
	span.End()
}

// mdCarrier preserves the lower-case keys required by gRPC metadata.
type mdCarrier metadata.MD

func (c mdCarrier) Get(key string) string {
	if vs := metadata.MD(c).Get(key); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func (c mdCarrier) Set(key, value string) { metadata.MD(c).Set(key, value) }

func (c mdCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

func ExtractGRPC(ctx context.Context) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || md == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, mdCarrier(md))
}
