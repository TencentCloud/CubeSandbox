// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

const dbErrorDetail = "dial tcp 10.0.0.5:3306: connect: connection refused"

type outageUserStore struct{ fakeUserStore }

func (o *outageUserStore) GetUserPassword(_ context.Context, _ string) (string, error) {
	return "", errors.New(dbErrorDetail)
}

func (o *outageUserStore) IsRefreshTokenRevoked(_ context.Context, _ string) (bool, error) {
	return false, errors.New(dbErrorDetail)
}

func newOutageRouter(t *testing.T) *gin.Engine {
	t.Helper()
	jm := auth.NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
	svc := service.NewAuthService(&outageUserStore{}, jm)
	h := auth.NewHandler(svc)

	r := gin.New()
	h.RegisterPublic(r.Group("/api/v1"))
	h.RegisterAuthed(r.Group("/api/v1", auth.Middleware(jm)))
	return r
}

func TestLoginOutageDoesNotLeakDatabaseDetailsToTheCaller(t *testing.T) {
	r := newOutageRouter(t)

	w := doRequest(t, r, "POST", "/api/v1/auth/login",
		`{"username":"admin","password":"s3cret"}`, "")

	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{dbErrorDetail, "10.0.0.5", "3306", "dial tcp", "connection refused"} {
		if strings.Contains(body, secret) {
			t.Errorf("500 body leaks %q to an unauthenticated caller: %s", secret, body)
		}
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("500 body = %s, want a generic message", body)
	}
	if strings.Contains(body, "required") {
		t.Errorf("the request never reached the database path: %s", body)
	}
}

func TestChangePasswordOutageDoesNotLeakDatabaseDetailsToTheCaller(t *testing.T) {
	jm := auth.NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
	token, err := jm.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}

	r := newOutageRouter(t)
	w := doRequest(t, r, "POST", "/api/v1/auth/change-password",
		`{"old_password":"s3cret","new_password":"n3wpass"}`, token)

	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{dbErrorDetail, "10.0.0.5", "3306", "dial tcp", "connection refused"} {
		if strings.Contains(body, secret) {
			t.Errorf("500 body leaks %q to the caller: %s", secret, body)
		}
	}
}

func TestRefreshOutageDoesNotLeakDatabaseDetailsToTheCaller(t *testing.T) {
	jm := auth.NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
	refresh, _, err := jm.GenerateRefreshToken("admin")
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}

	r := newOutageRouter(t)
	w := doRequest(t, r, "POST", "/api/v1/auth/refresh",
		`{"refreshToken":"`+refresh+`"}`, "")

	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{dbErrorDetail, "10.0.0.5", "3306", "dial tcp", "connection refused"} {
		if strings.Contains(body, secret) {
			t.Errorf("500 body leaks %q to the caller: %s", secret, body)
		}
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("500 body = %s, want a generic message", body)
	}
}
