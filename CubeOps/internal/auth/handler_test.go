// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeUserStore is a tiny in-memory user store for auth handler tests.
type fakeUserStore struct {
	passwords     map[string]string
	refreshTokens map[string]string
	revoked       map[string]bool
}

func (f *fakeUserStore) GetUserPassword(_ context.Context, username string) (string, error) {
	pw, ok := f.passwords[username]
	if !ok {
		return "", nil // missing user → empty stored → service maps to ErrInvalidCredentials
	}
	return pw, nil
}

func (f *fakeUserStore) SetUserPassword(_ context.Context, username, hash string) error {
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	f.passwords[username] = hash
	return nil
}

func (f *fakeUserStore) CreateRefreshToken(_ context.Context, tokenID, username string) error {
	if f.refreshTokens == nil {
		f.refreshTokens = map[string]string{}
	}
	f.refreshTokens[tokenID] = username
	return nil
}

func (f *fakeUserStore) IsRefreshTokenRevoked(_ context.Context, tokenID string) (bool, error) {
	if f.revoked == nil {
		return false, nil
	}
	return f.revoked[tokenID], nil
}

func (f *fakeUserStore) RevokeRefreshToken(_ context.Context, tokenID string) error {
	if f.revoked == nil {
		f.revoked = map[string]bool{}
	}
	f.revoked[tokenID] = true
	return nil
}

func (f *fakeUserStore) RevokeAllRefreshTokensForUser(_ context.Context, username string) error {
	for tid, u := range f.refreshTokens {
		if u == username {
			f.RevokeRefreshToken(nil, tid)
		}
	}
	return nil
}

func newAuthRouter(t *testing.T) (*gin.Engine, *fakeUserStore) {
	t.Helper()
	store := &fakeUserStore{passwords: map[string]string{}}
	hash, _ := crypto.HashPassword("s3cret")
	store.passwords["admin"] = hash

	jm := auth.NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
	svc := service.NewAuthService(store, jm)
	h := auth.NewHandler(svc)

	r := gin.New()
	public := r.Group("/api/v1")
	h.RegisterPublic(public)
	authed := r.Group("/api/v1", auth.Middleware(jm))
	h.RegisterAuthed(authed)
	return r, store
}

func doRequest(t *testing.T, r *gin.Engine, method, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- Login ---

func TestAuthLogin_Success(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"s3cret"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["accessToken"] == nil || resp["accessToken"] == "" {
		t.Error("accessToken missing or empty")
	}
	if resp["refreshToken"] == nil || resp["refreshToken"] == "" {
		t.Error("refreshToken missing or empty")
	}
	if resp["username"] != "admin" {
		t.Errorf("username = %v, want admin", resp["username"])
	}
}

func TestAuthLogin_WrongPassword_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"WRONG"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthLogin_InvalidJSON_400(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/login", `not json`, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Refresh ---

func TestAuthRefresh_Success(t *testing.T) {
	r, _ := newAuthRouter(t)
	// First login to get a real refresh token.
	w := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"s3cret"}`, "")
	var login map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &login)
	refreshToken, _ := login["refreshToken"].(string)
	if refreshToken == "" {
		t.Fatal("no refresh token from login")
	}

	// Now use it.
	w2 := doRequest(t, r, "POST", "/api/v1/auth/refresh", `{"refreshToken":"`+refreshToken+`"}`, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["accessToken"] == nil || resp["accessToken"] == "" {
		t.Error("accessToken missing")
	}
}

func TestAuthRefresh_InvalidToken_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/refresh", `{"refreshToken":"garbage"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// --- Session (requires auth middleware) ---

func TestAuthSession_NoToken_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "GET", "/api/v1/auth/session", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no token)", w.Code)
	}
}

func TestAuthSession_WithValidToken_200(t *testing.T) {
	r, _ := newAuthRouter(t)
	// Login to get access token.
	w := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"s3cret"}`, "")
	var login map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &login)
	accessToken, _ := login["accessToken"].(string)
	if accessToken == "" {
		t.Fatal("no access token")
	}

	w2 := doRequest(t, r, "GET", "/api/v1/auth/session", "", accessToken)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", resp["authenticated"])
	}
	if resp["username"] != "admin" {
		t.Errorf("username = %v, want admin", resp["username"])
	}
}

func TestAuthSession_BadBearerScheme_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	// "Basic xxx" instead of "Bearer xxx" — must be rejected.
	req := httptest.NewRequest("GET", "/api/v1/auth/session", nil)
	req.Header.Set("Authorization", "Basic abc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (bad scheme)", w.Code)
	}
}

// --- Logout ---

func TestAuthLogout_204(t *testing.T) {
	r, _ := newAuthRouter(t)
	w := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"s3cret"}`, "")
	var login map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &login)
	accessToken, _ := login["accessToken"].(string)

	w2 := doRequest(t, r, "POST", "/api/v1/auth/logout", "", accessToken)
	if w2.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w2.Code)
	}
}

// --- ChangePassword (IDOR protection) ---

func TestChangePassword_Success(t *testing.T) {
	r, _ := newAuthRouter(t)
	w := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"s3cret"}`, "")
	var login map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &login)
	accessToken, _ := login["accessToken"].(string)

	w2 := doRequest(t, r, "POST", "/api/v1/auth/change-password",
		`{"oldPassword":"s3cret","newPassword":"newpass123"}`, accessToken)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w2.Code, w2.Body.String())
	}

	// Old password should fail, new should work.
	w3 := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"s3cret"}`, "")
	if w3.Code != http.StatusUnauthorized {
		t.Errorf("old password status = %d, want 401", w3.Code)
	}
	w4 := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"newpass123"}`, "")
	if w4.Code != http.StatusOK {
		t.Errorf("new password status = %d, want 200", w4.Code)
	}
}

func TestChangePassword_WrongOld_401(t *testing.T) {
	r, _ := newAuthRouter(t)
	w := doRequest(t, r, "POST", "/api/v1/auth/login", `{"username":"admin","password":"s3cret"}`, "")
	var login map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &login)
	accessToken, _ := login["accessToken"].(string)

	w2 := doRequest(t, r, "POST", "/api/v1/auth/change-password",
		`{"oldPassword":"WRONG","newPassword":"newpass123"}`, accessToken)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w2.Code)
	}
}

func TestChangePassword_NoAuth_401(t *testing.T) {
	r, _ := newAuthRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/change-password",
		`{"oldPassword":"x","newPassword":"y"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no auth)", w.Code)
	}
}
