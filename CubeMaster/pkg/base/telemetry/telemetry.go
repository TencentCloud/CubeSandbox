// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package telemetry provides tracing for sandbox creation.
package telemetry

import (
	"context"
	"net/http"
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
)

const Scope = "github.com/tencentcloud/CubeSandbox/CubeMaster"

const (
	EnvEndpoint    = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvServiceName = "OTEL_SERVICE_NAME"

	defaultServiceName = "cubemaster"
)

const SuccessCode = 200

const (
	SpanCreate           = "cubemaster.create"
	SpanCreateTemplate   = "cubemaster.create.template"
	SpanTemplateRequest  = "cubemaster.create.template.request"
	SpanTemplateCache    = "cubemaster.create.template.cache"
	SpanTemplateWait     = "cubemaster.create.template.wait"
	SpanTemplateDB       = "cubemaster.create.template.db"
	SpanTemplateLocality = "cubemaster.create.template.locality"
	SpanTemplateKind     = "cubemaster.create.template.kind"
	SpanTemplateBind     = "cubemaster.create.template.bind"
	SpanCreateQueue      = "cubemaster.create.queue"
	SpanCreateSchedule   = "cubemaster.create.schedule"
	SpanCreateCubelet    = "cubemaster.create.cubelet"
	SpanCreateBackoff    = "cubemaster.create.backoff"
	SpanCreateRedis      = "cubemaster.create.post_redis"
	SpanCreateSpec       = "cubemaster.create.post_spec"
	SpanRegisterRef      = "cubemaster.create.register_ref"
)

const (
	AttrRequestID       = "cube.request_id"
	AttrSandboxID       = "cube.sandbox_id"
	AttrTemplateID      = "cube.template_id"
	AttrNodeID          = "cube.node_id"
	AttrInstanceType    = "cube.instance_type"
	AttrRetCode         = "cube.ret_code"
	AttrAttempt         = "cube.attempt"
	AttrWaitMS          = "cube.wait_ms"
	AttrCacheHit        = "cube.template.cache_hit"
	AttrLocalitySkipped = "cube.template.locality_skipped"
	AttrTable           = "db.table"
	AttrOperation       = "db.operation"
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

// InjectGRPC copies outgoing metadata before adding trace context.
func InjectGRPC(ctx context.Context) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	otel.GetTextMapPropagator().Inject(ctx, mdCarrier(md))
	return metadata.NewOutgoingContext(ctx, md)
}

func ExtractHTTP(ctx context.Context, header http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(header))
}
