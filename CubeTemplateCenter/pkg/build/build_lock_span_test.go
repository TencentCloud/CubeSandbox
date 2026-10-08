// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/lock"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

const lockTestDigest = "sha256:build-lock-span-test"

func buildLockTestRequest(templateID string) *types.CreateTemplateFromImageReq {
	noCA := false
	return &types.CreateTemplateFromImageReq{
		Request:        &types.Request{RequestID: "req-" + templateID},
		TemplateID:     templateID,
		InstanceType:   "cubebox",
		SourceImageRef: "example.invalid/img:1",
		WithCubeCA:     &noCA,
	}
}

func stubBuildPreamble(t *testing.T) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(image.EnsureArtifactBuildPreflight, func(context.Context) error { return nil })
	// Native export skips the pre-lock Redis flush.
	patches.ApplyFunc(image.PrepareSource, func(context.Context, image.SourceSpec) (*image.PreparedSource, error) {
		return &image.PreparedSource{Digest: lockTestDigest, ExportMode: image.ExportModeNative}, nil
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
}

func stubRunBuildLocked(entered chan<- time.Time) func(context.Context, string, *types.CreateTemplateFromImageReq, string, string, *image.PreparedSource, *Reporter, []byte, string, *templatecenter.EnvdInjectionPayload, *pullProgressSink, bool, bool, *s3store.Client, *cubelog.Entry) error {
	return func(context.Context, string, *types.CreateTemplateFromImageReq, string, string, *image.PreparedSource, *Reporter, []byte, string, *templatecenter.EnvdInjectionPayload, *pullProgressSink, bool, bool, *s3store.Client, *cubelog.Entry) error {
		if entered != nil {
			entered <- time.Now()
		}
		return nil
	}
}

func TestBuildLockWaitSpanEndsBeforeLockedWork(t *testing.T) {
	rec, flush := setupTracer(t)
	stubBuildPreamble(t)

	req := buildLockTestRequest("tpl-lock-wait")
	fingerprint := templatecenter.BuildTemplateSpecFingerprintWithEnvdSHA(req, lockTestDigest, "", "")

	entered := make(chan time.Time, 1)
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(runBuildLocked, stubRunBuildLocked(entered))

	unlock := artifactBuildLocks.Lock(fingerprint)
	const hold = 50 * time.Millisecond
	releasedAt := make(chan time.Time, 1)
	go func() {
		time.Sleep(hold)
		releasedAt <- time.Now()
		unlock()
	}()

	if err := Build(context.Background(), "job-lock-wait", req, "", "", nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	lockedAt := <-entered
	released := <-releasedAt
	flush()

	waits := rec.named(telemetry.SpanTemplateImageBuildLockWait)
	if len(waits) != 1 {
		t.Fatalf("want exactly 1 lock-wait span, got %d", len(waits))
	}
	wait := waits[0]
	if !wait.StartTime().Before(released) || wait.EndTime().Before(released) {
		t.Errorf("lock-wait span [%s, %s] does not cover the lock being held until %s",
			wait.StartTime(), wait.EndTime(), released)
	}
	if wait.EndTime().After(lockedAt) {
		t.Errorf("lock-wait span ends at %s, after locked work began at %s", wait.EndTime(), lockedAt)
	}
	if got := rec.named(telemetry.SpanTemplateImageBuild); len(got) != 1 {
		t.Fatalf("want 1 build span, got %d", len(got))
	}
}

func TestBuildLockWaitSpanRecordsReuseInsteadOfRunningBuild(t *testing.T) {
	rec, flush := setupTracer(t)
	stubBuildPreamble(t)

	req := buildLockTestRequest("tpl-lock-reuse")

	savedGetDB := getDB
	getDB = func() *gorm.DB { return &gorm.DB{} }
	t.Cleanup(func() { getDB = savedGetDB })

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(lock.WithBuildLock, func(context.Context, *gorm.DB, string, func() error) error {
		return lock.ErrBuildInProgress
	})
	patches.ApplyFunc(reuseExistingArtifact, func(context.Context, *gorm.DB, string, bool, *s3store.Client) (*models.RootfsArtifact, bool) {
		return &models.RootfsArtifact{ArtifactID: "artifact-reused"}, true
	})
	var built int32
	patches.ApplyFunc(runBuildLocked, func(context.Context, string, *types.CreateTemplateFromImageReq, string, string, *image.PreparedSource, *Reporter, []byte, string, *templatecenter.EnvdInjectionPayload, *pullProgressSink, bool, bool, *s3store.Client, *cubelog.Entry) error {
		atomic.AddInt32(&built, 1)
		return nil
	})
	reused := ""
	patches.ApplyFunc(reportExistingArtifact, func(_ context.Context, _ string, existing *models.RootfsArtifact, _ string, _ *image.PreparedSource, _ *Reporter, _ []byte, _ string, _ bool, _ *s3store.Client, _ *cubelog.Entry) error {
		reused = existing.ArtifactID
		return nil
	})

	if err := Build(context.Background(), "job-lock-reuse", req, "", "", nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	flush()

	if reused != "artifact-reused" {
		t.Errorf("reused artifact = %q, want the sibling's artifact", reused)
	}
	if n := atomic.LoadInt32(&built); n != 0 {
		t.Errorf("the locked build ran %d time(s) even though a sibling artifact was reused", n)
	}
	waits := rec.named(telemetry.SpanTemplateImageBuildLockWait)
	if len(waits) != 1 {
		t.Fatalf("want exactly 1 lock-wait span, got %d", len(waits))
	}
	if !attrIsTrue(waits[0], telemetry.AttrReused) {
		t.Errorf("wait span must carry %s=true", telemetry.AttrReused)
	}
	if got := waits[0].EndTime().Sub(waits[0].StartTime()); got <= 0 {
		t.Errorf("wait span has a non-positive duration %s", got)
	}
}
