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
	SpanTemplateImageSubmit        = "cubemaster.template.image.submit"
	SpanTemplateImageDispatch      = "cubemaster.template.image.dispatch"
	SpanTemplateSubmitAttempt      = "cubemaster.template.image.dispatch.attempt"
	SpanTemplateDispatchBackoff    = "cubemaster.template.image.dispatch.backoff"
	SpanTemplateImageCallback      = "cubemaster.template.image.callback"
	SpanTemplateImageRegister      = "cubemaster.template.image.register"
	SpanTemplateImageComplete      = "cubemaster.template.image.complete"
	SpanTemplateDistribute         = "cubemaster.template.image.distribute"
	SpanTemplateReplicate          = "cubemaster.template.image.replicate"
	SpanTemplateNodeSlot           = "cubemaster.template.image.node.slot"
	SpanTemplateNodeImage          = "cubemaster.template.image.node.create_image"
	SpanTemplateNodeSnapshot       = "cubemaster.template.image.node.appsnapshot"
	SpanTemplateRegistry           = "cubemaster.template.image.registry"
	SpanTemplateFinalize           = "cubemaster.template.image.finalize"
	SpanTemplateImageReconcile     = "cubemaster.template.image.reconcile"
	SpanTemplateImageCleanupMaster = "cubemaster.template.image.artifact.cleanup"

	SpanTemplateCommitSubmit   = "cubemaster.template.commit.submit"
	SpanTemplateCommitRun      = "cubemaster.template.commit.run"
	SpanTemplateCommitSnapshot = "cubemaster.template.commit.snapshot"
	SpanTemplateCommitRegister = "cubemaster.template.commit.register"
	SpanTemplateCommitCleanup  = "cubemaster.template.commit.cleanup"

	SpanSnapshotCreatePrepare  = "cubemaster.snapshot.create.prepare"
	SpanSnapshotCreateCapture  = "cubemaster.snapshot.create.capture"
	SpanSnapshotCreateRegister = "cubemaster.snapshot.create.register"
	SpanSnapshotCreateCleanup  = "cubemaster.snapshot.create.cleanup"

	SpanSnapshotRollbackPrepare  = "cubemaster.snapshot.rollback.prepare"
	SpanSnapshotRollbackRestore  = "cubemaster.snapshot.rollback.restore"
	SpanSnapshotRollbackRegister = "cubemaster.snapshot.rollback.register"

	SpanSandboxPausePrepare   = "cubemaster.sandbox.pause.prepare"
	SpanSandboxPauseRuntime   = "cubemaster.sandbox.pause.runtime"
	SpanSandboxPauseFinalize  = "cubemaster.sandbox.pause.finalize"
	SpanSandboxResumePrepare  = "cubemaster.sandbox.resume.prepare"
	SpanSandboxResumeRuntime  = "cubemaster.sandbox.resume.runtime"
	SpanSandboxResumeFinalize = "cubemaster.sandbox.resume.finalize"

	SpanTemplateImageBuild            = "cubetemplatecenter.template.image.build"
	SpanTemplateImagePrepareSource    = "cubetemplatecenter.template.image.prepare_source"
	SpanTemplateImageBuildLockWait    = "cubetemplatecenter.template.image.build_lock.wait"
	SpanTemplateImageRootfs           = "cubetemplatecenter.template.image.rootfs"
	SpanTemplateImageExt4             = "cubetemplatecenter.template.image.ext4"
	SpanTemplateImageStreamExt4       = "cubetemplatecenter.template.image.stream_ext4"
	SpanTemplateArtifactPublish       = "cubetemplatecenter.template.image.artifact.publish"
	SpanTemplateArtifactCallback      = "cubetemplatecenter.template.image.artifact.callback"
	SpanTemplateArtifactCleanup       = "cubetemplatecenter.template.image.artifact.cleanup"
	SpanTemplateArtifactReportAttempt = "cubetemplatecenter.template.image.artifact.report.attempt"
	SpanTemplateArtifactReportBackoff = "cubetemplatecenter.template.image.artifact.report.backoff"
)

const (
	AttrRequestID       = "cube.request_id"
	AttrSandboxID       = "cube.sandbox_id"
	AttrSnapshotID      = "cube.snapshot_id"
	AttrTemplateID      = "cube.template_id"
	AttrNodeID          = "cube.node_id"
	AttrNodeIP          = "cube.node_ip"
	AttrInstanceType    = "cube.instance_type"
	AttrRetCode         = "cube.ret_code"
	AttrAttempt         = "cube.attempt"
	AttrWaitMS          = "cube.wait_ms"
	AttrCacheHit        = "cube.template.cache_hit"
	AttrLocalitySkipped = "cube.template.locality_skipped"
	AttrTable           = "db.table"
	AttrOperation       = "db.operation"
	AttrJobID           = "cube.job_id"
	AttrArtifactID      = "cube.artifact_id"
	AttrReused          = "cube.reused"
	AttrExportMode      = "cube.template.export_mode"
	AttrPullDeferred    = "cube.template.pull_deferred"
	AttrDuplicate       = "cube.dispatch.duplicate"
	AttrStorageBackend  = "cube.artifact.storage_backend"
	AttrBackend         = "cube.backend"
	AttrFallback        = "cube.artifact.fallback"
	AttrAction          = "cube.action"
	AttrCrossNode       = "cube.cross_node"
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

func InjectHTTP(ctx context.Context, header http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
}

// DetachTrace copies the trace parent while retaining base cancellation.
func DetachTrace(base, traceFrom context.Context) context.Context {
	return trace.ContextWithSpanContext(base, trace.SpanContextFromContext(traceFrom))
}
