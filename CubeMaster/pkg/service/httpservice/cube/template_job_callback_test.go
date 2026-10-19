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

func TestBuiltCallbackRejectsConflictingMetadataBeforePersistence(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "fixture-token")
	persisted := false
	original := applyTemplateImageJobBuiltReport
	applyTemplateImageJobBuiltReport = func(context.Context, string, map[string]any) (bool, error) { persisted = true; return true, nil }
	t.Cleanup(func() { applyTemplateImageJobBuiltReport = original })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "job_id", Value: "job-1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/template/jobs/job-1/status", strings.NewReader(`{"status":"BUILT","rootfs_path":"/new.ext4","ext4_path":"/old.ext4"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set(constants.TemplateCallbackTokenHeader, "fixture-token")
	handleTemplateJobStatusCallback(c)
	if recorder.Code != http.StatusBadRequest || persisted {
		t.Fatalf("code=%d persisted=%v body=%s", recorder.Code, persisted, recorder.Body.String())
	}
}

func TestBuiltCallbackPreservesExactPayloadForReplay(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "fixture-token")
	const body = `{"status":"BUILT","artifact_id":"rfs-1","rootfs_size_bytes":9007199254740993,"pull_total_bytes":9007199254740993,"future_field":{"value":1}}`
	var values map[string]any
	original := applyTemplateImageJobBuiltReport
	applyTemplateImageJobBuiltReport = func(_ context.Context, _ string, got map[string]any) (bool, error) {
		values = got
		return false, nil // Already claimed: no asynchronous continuation.
	}
	t.Cleanup(func() { applyTemplateImageJobBuiltReport = original })
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "job_id", Value: "job-1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/template/jobs/job-1/status", strings.NewReader(body))
	c.Request.Header.Set(constants.TemplateCallbackTokenHeader, "fixture-token")
	handleTemplateJobStatusCallback(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if values["result_json"] != body || values["pull_total_bytes"] != int64(9007199254740993) {
		t.Fatalf("callback changed report bytes or counter: %v", values)
	}
	var replay templatecenter.RemoteBuildResult
	if err := json.Unmarshal([]byte(values["result_json"].(string)), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.RootfsSizeBytes != 9007199254740993 {
		t.Fatalf("replayed size=%d", replay.RootfsSizeBytes)
	}
}
