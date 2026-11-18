// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func attrString(attrs []attribute.KeyValue, key string) string {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

func TestTemplateJobBuiltCallbackJoinsBuildTraceAndDetachesContinuation(t *testing.T) {
	rec, flush := setupSpanRecorder(t)
	t.Setenv(constants.TemplateCallbackTokenEnv, "s3cret")

	oldApply := applyTemplateImageJobBuiltReport
	oldPrepare := prepareTemplateImageJobAfterRemoteBuildCallback
	oldContinue := continueTemplateImageJobAfterRemoteBuild
	applyTemplateImageJobBuiltReport = func(context.Context, string, map[string]any) (bool, error) { return true, nil }
	prepareTemplateImageJobAfterRemoteBuildCallback = func(context.Context, string, *templatecenter.RemoteBuildResult) (*templatecenter.RemoteBuildContinuation, error) {
		return &templatecenter.RemoteBuildContinuation{}, nil
	}
	resumeCh := make(chan context.Context, 1)
	continueTemplateImageJobAfterRemoteBuild = func(ctx context.Context, _ *templatecenter.RemoteBuildContinuation) error {
		resumeCh <- ctx
		return nil
	}
	t.Cleanup(func() {
		applyTemplateImageJobBuiltReport = oldApply
		prepareTemplateImageJobAfterRemoteBuildCallback = oldPrepare
		continueTemplateImageJobAfterRemoteBuild = oldContinue
	})

	tcCtx, tcSpan := telemetry.Start(context.Background(), "tc.build")
	defer tcSpan.End()

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	body := `{"status":"BUILT","phase":"READY","artifact_id":"rfs-1"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/template/jobs/job-1/status", strings.NewReader(body)).WithContext(requestCtx)
	c.Request.Header.Set(constants.TemplateCallbackTokenHeader, "s3cret")
	c.Request.Header.Set("Content-Type", "application/json")
	telemetry.InjectHTTP(tcCtx, c.Request.Header)
	c.Params = gin.Params{{Key: "job_id", Value: "job-1"}}

	handleTemplateJobStatusCallback(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resumeCtx context.Context
	select {
	case resumeCtx = <-resumeCh:
	case <-time.After(3 * time.Second):
		t.Fatal("the continuation never ran")
	}

	cancelRequest()
	flush()

	callbacks := rec.named(telemetry.SpanTemplateImageCallback)
	if len(callbacks) != 1 {
		t.Fatalf("want 1 callback span, got %d", len(callbacks))
	}
	cb := callbacks[0]
	if cb.Parent().SpanID() != tcSpan.SpanContext().SpanID() {
		t.Errorf("callback parent = %s, want the TC build span %s",
			cb.Parent().SpanID(), tcSpan.SpanContext().SpanID())
	}
	if cb.SpanContext().TraceID() != tcSpan.SpanContext().TraceID() {
		t.Error("callback did not join the TC build trace")
	}
	if got := attrString(cb.Attributes(), telemetry.AttrJobID); got != "job-1" {
		t.Errorf("%s = %q, want job-1", telemetry.AttrJobID, got)
	}

	got := trace.SpanContextFromContext(resumeCtx)
	if !got.IsValid() || got.TraceID() != tcSpan.SpanContext().TraceID() {
		t.Errorf("continuation span context = %s, want the TC trace %s",
			got.TraceID(), tcSpan.SpanContext().TraceID())
	}
	if got.SpanID() != cb.SpanContext().SpanID() {
		t.Errorf("continuation parent = %s, want the callback span %s",
			got.SpanID(), cb.SpanContext().SpanID())
	}
	if err := resumeCtx.Err(); err != nil {
		t.Errorf("the answered request canceled the continuation: %v", err)
	}
}
