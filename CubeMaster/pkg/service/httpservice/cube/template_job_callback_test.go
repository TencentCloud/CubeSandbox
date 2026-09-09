// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
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
