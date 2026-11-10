// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"go.opentelemetry.io/otel/codes"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
)

func TestArtifactPublishFallbackKeepsBuildSuccessfulAndPublishFailed(t *testing.T) {
	rec, flush := setupTracer(t)

	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(master.Close)
	t.Setenv(tcconfig.EnvMasterEndpoint, master.URL)
	base, max := reportBaseBackoff, reportMaxBackoff
	reportBaseBackoff, reportMaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { reportBaseBackoff, reportMaxBackoff = base, max })

	// Fail at the S3 PUT while retaining the S3 backend.
	client, err := s3store.NewClient(s3store.Config{
		Driver: "s3", Endpoint: "http://127.0.0.1:1", Bucket: "bucket", AccessKey: "a", SecretKey: "s",
	})
	if err != nil {
		t.Fatalf("s3store.NewClient: %v", err)
	}

	// Keep the source readable so the failure reaches S3.
	ext4Path := filepath.Join(t.TempDir(), "artifact.ext4")
	ext4Bytes := []byte("fake ext4 payload")
	if err := os.WriteFile(ext4Path, ext4Bytes, 0o600); err != nil {
		t.Fatalf("write fake artifact: %v", err)
	}

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(image.BuildExt4, func(context.Context, *image.PreparedSource, image.BuildOptions) (image.BuildResult, error) {
		return image.BuildResult{Ext4Path: ext4Path, SHA256: "sha256:x", SizeBytes: int64(len(ext4Bytes))}, nil
	})

	ctx, buildSpan := telemetry.Start(context.Background(), telemetry.SpanTemplateImageBuild)
	err = runBuildLocked(ctx, "job-fallback", buildLockTestRequest("tpl-fallback"), "artifact-fallback", "fp-fallback",
		&image.PreparedSource{Digest: lockTestDigest, ExportMode: image.ExportModeDocker},
		NewReporter(), nil, "", nil, nil, true, true, client, log.G(ctx))
	telemetry.End(buildSpan, err)
	if err != nil {
		t.Fatalf("runBuildLocked: %v (the s3->local fallback must keep the build successful)", err)
	}
	flush()

	builds := rec.named(telemetry.SpanTemplateImageBuild)
	if len(builds) != 1 {
		t.Fatalf("want 1 build span, got %d", len(builds))
	}
	if builds[0].Status().Code != codes.Unset {
		t.Errorf("build span status = %v, want Unset", builds[0].Status().Code)
	}

	pubs := rec.named(telemetry.SpanTemplateArtifactPublish)
	if len(pubs) != 1 {
		t.Fatalf("want 1 publish span, got %d", len(pubs))
	}
	pub := pubs[0]
	if pub.Status().Code != codes.Error {
		t.Errorf("publish span status = %v, want Error: the upload itself failed", pub.Status().Code)
	}
	if !attrIsTrue(pub, telemetry.AttrFallback) {
		t.Errorf("publish span must carry %s=true", telemetry.AttrFallback)
	}
	if pub.Parent().SpanID() != builds[0].SpanContext().SpanID() {
		t.Errorf("publish span parent = %s, want the build span %s",
			pub.Parent().SpanID(), builds[0].SpanContext().SpanID())
	}
}
