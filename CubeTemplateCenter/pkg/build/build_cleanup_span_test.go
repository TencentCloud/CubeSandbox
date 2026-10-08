// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func TestSourceCleanupSpanCoversOnlyCleanup(t *testing.T) {
	rec, flush := setupTracer(t)

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(image.EnsureArtifactBuildPreflight, func(context.Context) error { return nil })

	cleanedAt := make(chan time.Time, 1)
	patches.ApplyFunc(image.PrepareSource, func(context.Context, image.SourceSpec) (*image.PreparedSource, error) {
		return &image.PreparedSource{
			Digest:     lockTestDigest,
			ExportMode: image.ExportModeNative,
			Cleanup: func(context.Context) {
				time.Sleep(20 * time.Millisecond)
				cleanedAt <- time.Now()
			},
		}, nil
	})

	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(master.Close)
	t.Setenv(tcconfig.EnvMasterEndpoint, master.URL)
	t.Setenv("CUBE_ARTIFACT_STORE_BACKEND", "s3")
	base, max := reportBaseBackoff, reportMaxBackoff
	reportBaseBackoff, reportMaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { reportBaseBackoff, reportMaxBackoff = base, max })

	lockedDone := make(chan time.Time, 1)
	patches.ApplyFunc(runBuildLocked, func(context.Context, string, *types.CreateTemplateFromImageReq, string, string, *image.PreparedSource, *Reporter, []byte, string, *templatecenter.EnvdInjectionPayload, *pullProgressSink, bool, bool, *s3store.Client, *cubelog.Entry) error {
		time.Sleep(30 * time.Millisecond)
		lockedDone <- time.Now()
		return nil
	})

	if err := Build(context.Background(), "job-cleanup-span", buildLockTestRequest("tpl-cleanup-span"), "", "", nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	locked := <-lockedDone
	cleaned := <-cleanedAt
	flush()

	cleans := rec.named(telemetry.SpanTemplateArtifactCleanup)
	if len(cleans) != 1 {
		t.Fatalf("want exactly 1 cleanup span, got %d", len(cleans))
	}
	clean := cleans[0]
	if clean.StartTime().Before(locked) {
		t.Errorf("cleanup span opens at %s, before the locked build returned at %s: it is measuring build work",
			clean.StartTime(), locked)
	}
	if clean.EndTime().Before(cleaned) {
		t.Errorf("cleanup span closes at %s, before the cleanup itself finished at %s", clean.EndTime(), cleaned)
	}

	builds := rec.named(telemetry.SpanTemplateImageBuild)
	if len(builds) != 1 {
		t.Fatalf("want 1 build span, got %d", len(builds))
	}
	if clean.SpanContext().TraceID() != builds[0].SpanContext().TraceID() {
		t.Errorf("cleanup span trace %s differs from the build trace %s",
			clean.SpanContext().TraceID(), builds[0].SpanContext().TraceID())
	}
	if clean.Parent().SpanID() != builds[0].SpanContext().SpanID() {
		t.Errorf("cleanup span parent = %s, want the build span %s",
			clean.Parent().SpanID(), builds[0].SpanContext().SpanID())
	}
}
