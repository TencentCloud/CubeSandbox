// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
)

// newVerifyJWTManager builds a JWTManager with the same secret/TTLs as
// newAuthRouter (handler_test.go) so tokens minted here verify there.
func newVerifyJWTManager() *auth.JWTManager {
	return auth.NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
}

func TestAuthVerify_ValidAccessToken_200(t *testing.T) {
	r, _ := newAuthRouter(t)
	jm := newVerifyJWTManager()

	token, err := jm.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("generate access token: %v", err)
	}

	w := doRequest(t, r, "POST", "/api/v1/auth/verify", "", token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Auth-User"); got != "admin" {
		t.Errorf("X-Auth-User = %q, want %q", got, "admin")
	}
	if got := w.Body.String(); got != "{}\n" && got != "{}" {
		t.Errorf("body = %q, want empty JSON object", got)
	}
}

func TestAuthVerify_RefreshToken_401(t *testing.T) {
	r, _ := newAuthRouter(t)
	jm := newVerifyJWTManager()

	refreshToken, _, err := jm.GenerateRefreshToken("admin")
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}

	// A long-lived refresh token must not be accepted as an access token.
	w := doRequest(t, r, "POST", "/api/v1/auth/verify", "", refreshToken)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (refresh token)", w.Code)
	}
	if got := w.Header().Get("X-Auth-User"); got != "" {
		t.Errorf("X-Auth-User = %q, want empty on 401", got)
	}
}

func TestAuthVerify_InvalidToken_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/verify", "", "garbage")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (invalid token)", w.Code)
	}
}

func TestAuthVerify_TokenSignedWithOtherSecret_401(t *testing.T) {
	r, _ := newAuthRouter(t)
	other := auth.NewJWTManager("a-different-secret-altogether!!", 15*time.Minute, 168*time.Hour)
	token, err := other.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("generate access token: %v", err)
	}

	w := doRequest(t, r, "POST", "/api/v1/auth/verify", "", token)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (wrong secret)", w.Code)
	}
}

func TestAuthVerify_NoAuthorizationHeader_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/verify", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no header)", w.Code)
	}
}

func TestAuthVerify_APIKeyNotSupported_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	// CubeOps only recognises its own JWTs; X-API-Key is rejected.
	req := httptest.NewRequest("POST", "/api/v1/auth/verify", nil)
	req.Header.Set("X-API-Key", "some-api-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (X-API-Key unsupported)", w.Code)
	}
}
