// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
)

func spanIntAttr(span sdktrace.ReadOnlySpan, key string) (int64, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsInt64(), true
		}
	}
	return 0, false
}

func reportBuiltTestServer(t *testing.T, requests *int32, failFirst bool) (*httptest.Server, chan string) {
	t.Helper()
	traceparents := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparents <- r.Header.Get("traceparent")
		if failFirst && atomic.AddInt32(requests, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(tcconfig.EnvMasterEndpoint, srv.URL)
	base, max := reportBaseBackoff, reportMaxBackoff
	reportBaseBackoff, reportMaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { reportBaseBackoff, reportMaxBackoff = base, max })
	return srv, traceparents
}

func builtReportArgs() (*image.BuildResult, *image.PreparedSource) {
	return &image.BuildResult{Ext4Path: "/tmp/x.ext4", SHA256: "sha256:x", SizeBytes: 1},
		&image.PreparedSource{Digest: "sha256:x", ExportMode: image.ExportModeDocker}
}

func TestArtifactBuiltCallbackSendsItsOwnSpanAsParent(t *testing.T) {
	rec, flush := setupTracer(t)
	_, traceparents := reportBuiltTestServer(t, new(int32), false)

	rootCtx, rootSpan := telemetry.Start(context.Background(), telemetry.SpanTemplateImageBuild)
	defer rootSpan.End()

	result, source := builtReportArgs()
	if err := reportArtifactBuilt(rootCtx, "job-cb", result, "rfs-1", "fp-1", source,
		NewReporter(), cube_egress_ca.Result{}, "", false, nil, "", log.G(rootCtx)); err != nil {
		t.Fatalf("reportArtifactBuilt: %v", err)
	}
	tp := <-traceparents
	flush()

	attempts := rec.named(telemetry.SpanTemplateArtifactReportAttempt)
	if len(attempts) != 1 {
		t.Fatalf("want exactly 1 attempt span, got %d", len(attempts))
	}
	cbs := rec.named(telemetry.SpanTemplateArtifactCallback)
	if len(cbs) != 1 {
		t.Fatalf("want exactly 1 callback span, got %d", len(cbs))
	}

	fields := strings.Split(tp, "-")
	if len(fields) != 4 {
		t.Fatalf("malformed traceparent %q", tp)
	}
	if fields[1] != attempts[0].SpanContext().TraceID().String() {
		t.Errorf("traceparent trace-id %s != attempt span trace %s", fields[1], attempts[0].SpanContext().TraceID())
	}
	if fields[2] != attempts[0].SpanContext().SpanID().String() {
		t.Errorf("traceparent parent-id %s != attempt span id %s", fields[2], attempts[0].SpanContext().SpanID())
	}
	if attempts[0].Parent().SpanID() != cbs[0].SpanContext().SpanID() {
		t.Errorf("attempt parent = %s, want the callback aggregate %s",
			attempts[0].Parent().SpanID(), cbs[0].SpanContext().SpanID())
	}
	if cbs[0].Parent().SpanID() != rootSpan.SpanContext().SpanID() {
		t.Errorf("callback parent = %s, want the build span %s",
			cbs[0].Parent().SpanID(), rootSpan.SpanContext().SpanID())
	}
}

func TestArtifactBuiltReportTracesAttemptsAndBackoff(t *testing.T) {
	rec, flush := setupTracer(t)
	var requests int32
	_, traceparents := reportBuiltTestServer(t, &requests, true)

	rootCtx, rootSpan := telemetry.Start(context.Background(), telemetry.SpanTemplateImageBuild)
	defer rootSpan.End()

	result, source := builtReportArgs()
	if err := reportArtifactBuilt(rootCtx, "job-retry", result, "rfs-2", "fp-2", source,
		NewReporter(), cube_egress_ca.Result{}, "", false, nil, "", log.G(rootCtx)); err != nil {
		t.Fatalf("reportArtifactBuilt: %v", err)
	}
	tp1 := <-traceparents
	tp2 := <-traceparents
	flush()

	if n := atomic.LoadInt32(&requests); n != 2 {
		t.Fatalf("want 2 attempts, got %d", n)
	}
	attempts := rec.named(telemetry.SpanTemplateArtifactReportAttempt)
	if len(attempts) != 2 {
		t.Fatalf("want 2 attempt spans, got %d", len(attempts))
	}
	backoffs := rec.named(telemetry.SpanTemplateArtifactReportBackoff)
	if len(backoffs) != 1 {
		t.Fatalf("want 1 backoff span, got %d", len(backoffs))
	}
	cbs := rec.named(telemetry.SpanTemplateArtifactCallback)
	if len(cbs) != 1 {
		t.Fatalf("want 1 callback aggregate, got %d", len(cbs))
	}

	for i, tp := range []string{tp1, tp2} {
		fields := strings.Split(tp, "-")
		if len(fields) != 4 {
			t.Fatalf("malformed traceparent %q", tp)
		}
		if fields[2] != attempts[i].SpanContext().SpanID().String() {
			t.Errorf("request %d traceparent parent-id %s != attempt span id %s",
				i+1, fields[2], attempts[i].SpanContext().SpanID())
		}
	}
	if attempts[0].Status().Code != codes.Error {
		t.Errorf("failed attempt status = %v, want Error", attempts[0].Status().Code)
	}
	if attempts[1].Status().Code != codes.Unset {
		t.Errorf("retried attempt status = %v, want Unset", attempts[1].Status().Code)
	}
	if got, _ := spanIntAttr(attempts[0], telemetry.AttrAttempt); got != 1 {
		t.Errorf("first attempt %s = %d, want 1", telemetry.AttrAttempt, got)
	}
	if got, _ := spanIntAttr(attempts[1], telemetry.AttrAttempt); got != 2 {
		t.Errorf("second attempt %s = %d, want 2", telemetry.AttrAttempt, got)
	}
	if got, ok := spanIntAttr(backoffs[0], telemetry.AttrWaitMS); !ok || got <= 0 {
		t.Errorf("backoff span %s = %d (ok=%v), want the wait actually spent", telemetry.AttrWaitMS, got, ok)
	}
	for i, a := range attempts {
		if a.Parent().SpanID() != cbs[0].SpanContext().SpanID() {
			t.Errorf("attempt %d parent = %s, want the callback aggregate %s",
				i+1, a.Parent().SpanID(), cbs[0].SpanContext().SpanID())
		}
	}
}

func TestNonTerminalReportTracesNoAttemptSpan(t *testing.T) {
	rec, flush := setupTracer(t)
	reportBuiltTestServer(t, new(int32), false)

	ctx, span := telemetry.Start(context.Background(), telemetry.SpanTemplateImageBuild)
	defer span.End()

	if err := NewReporter().Report(ctx, "job-heartbeat", map[string]any{
		"status":   "RUNNING",
		"phase":    "BUILDING_EXT4",
		"progress": 40,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	flush()

	if n := len(rec.named(telemetry.SpanTemplateArtifactReportAttempt)); n != 0 {
		t.Errorf("heartbeat produced %d attempt span(s), want 0", n)
	}
	if n := len(rec.named(telemetry.SpanTemplateArtifactReportBackoff)); n != 0 {
		t.Errorf("heartbeat produced %d backoff span(s), want 0", n)
	}
}
