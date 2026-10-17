// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

func callbackAuthRequest(tokenHeader string) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/template/jobs/job-1/status", nil)
	if tokenHeader != "" {
		c.Request.Header.Set(constants.TemplateCallbackTokenHeader, tokenHeader)
	}
	return c
}

// Without CUBE_TEMPLATE_CALLBACK_TOKEN the endpoint stays open for
// rolling-upgrade compatibility with an older TC.
func TestTemplateCallbackAuthorizedWhenTokenUnset(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "")
	if !templateCallbackAuthorized(callbackAuthRequest("")) {
		t.Fatal("expected request to be allowed when the callback token is not configured")
	}
}

// Once the token is configured, a missing or mismatched header is rejected.
func TestTemplateCallbackAuthorizedRequiresMatchingToken(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "s3cret")

	if templateCallbackAuthorized(callbackAuthRequest("")) {
		t.Fatal("missing header must be rejected when the token is configured")
	}
	if templateCallbackAuthorized(callbackAuthRequest("wrong")) {
		t.Fatal("mismatched token must be rejected")
	}
	if !templateCallbackAuthorized(callbackAuthRequest("s3cret")) {
		t.Fatal("matching token must be accepted")
	}
}

// The callback handler answers 401 before touching the payload when the token
// check fails.
func TestTemplateJobStatusCallbackRejectsBadToken(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "s3cret")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/template/jobs/job-1/status", nil)
	c.Params = gin.Params{{Key: "job_id", Value: "job-1"}}

	handleTemplateJobStatusCallback(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestTemplateJobBuiltCallbackRetryDoesNotStartSecondContinuation(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "s3cret")
	var updateCalls atomic.Int32
	var prepareCalls atomic.Int32
	oldApply := applyTemplateImageJobBuiltReport
	oldPrepare := prepareTemplateImageJobAfterRemoteBuildCallback
	applyTemplateImageJobBuiltReport = func(context.Context, string, map[string]any) (bool, error) {
		return updateCalls.Add(1) == 1, nil
	}
	prepareTemplateImageJobAfterRemoteBuildCallback = func(context.Context, string, *templatecenter.RemoteBuildResult) (*templatecenter.RemoteBuildContinuation, error) {
		prepareCalls.Add(1)
		return nil, nil
	}
	t.Cleanup(func() {
		applyTemplateImageJobBuiltReport = oldApply
		prepareTemplateImageJobAfterRemoteBuildCallback = oldPrepare
	})

	body := `{"status":"BUILT","phase":"READY","artifact_id":"rfs-1"}`
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/internal/template/jobs/job-1/status", strings.NewReader(body))
		c.Request.Header.Set(constants.TemplateCallbackTokenHeader, "s3cret")
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "job_id", Value: "job-1"}}

		handleTemplateJobStatusCallback(c)
		if w.Code != http.StatusOK {
			t.Fatalf("callback %d status = %d, want 200", i+1, w.Code)
		}
	}
	if got := prepareCalls.Load(); got != 1 {
		t.Fatalf("prepare calls = %d, want 1", got)
	}
}

// TestRemoteBuildResultFromPayloadParsesRootfsFormat verifies the BUILT callback
// payload's rootfs_format (reported by TC) is decoded into RemoteBuildResult so
// the resume path can fill ImageSpec.StorageMedia from the result.
func TestRemoteBuildResultFromPayloadParsesRootfsFormat(t *testing.T) {
	payload := map[string]any{
		"status":            "BUILT",
		"artifact_id":       "rfs-1",
		"ext4_path":         "/data/artifacts/rfs-1/rfs-1.ext4",
		"ext4_sha256":       "sha256:abc",
		"ext4_size_bytes":   json.Number("4096"),
		"master_node_ip":    "http://master:8080",
		"image_config_json": `{}`,
		"rootfs_format":     "erofs",
	}
	got := remoteBuildResultFromPayload(payload)
	if got.RootfsFormat != "erofs" {
		t.Fatalf("RootfsFormat = %q, want %q", got.RootfsFormat, "erofs")
	}
	if got.ArtifactID != "rfs-1" {
		t.Fatalf("ArtifactID = %q, want %q", got.ArtifactID, "rfs-1")
	}

	// Absent rootfs_format (older TC) decodes to empty, which Validate treats as
	// ext4 downstream — proving the rolling-upgrade path.
	got = remoteBuildResultFromPayload(map[string]any{"artifact_id": "rfs-2"})
	if got.RootfsFormat != "" {
		t.Fatalf("absent rootfs_format must decode to empty, got %q", got.RootfsFormat)
	}
}
