// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

type fakeTerminalGrantService struct {
	response  *service.TerminalGrantResponse
	err       *service.TerminalError
	principal service.TerminalPrincipal
	request   service.TerminalGrantRequest
	calls     int
}

func (f *fakeTerminalGrantService) IssueTerminalGrant(_ context.Context, principal service.TerminalPrincipal, request service.TerminalGrantRequest) (*service.TerminalGrantResponse, *service.TerminalError) {
	f.calls++
	f.principal = principal
	f.request = request
	return f.response, f.err
}

func TestTerminalGrantHandlerRequiresJWTAndUsesVerifiedClaims(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeTerminalGrantService{response: &service.TerminalGrantResponse{
		Token: "one-time-grant", WSURL: "/opsapi/v1/terminal/ws", SessionID: "session-a",
		SandboxID: "sandbox-a", ContainerID: "container-a", ExpiresAt: time.Now().Add(time.Minute),
	}}
	router, token := terminalGrantTestRouter(t, fake)
	body := []byte(`{"sandboxId":"sandbox-a","cols":80,"rows":24}`)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/terminal/grants", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing JWT status = %d, want 401", recorder.Code)
	}
	if fake.calls != 0 {
		t.Fatalf("service calls = %d, want 0", fake.calls)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/terminal/grants", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("valid grant status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if fake.calls != 1 || fake.principal.UserID != "admin" || fake.principal.Role != "admin" {
		t.Fatalf("verified principal = %+v calls=%d", fake.principal, fake.calls)
	}
	if fake.request.SandboxID != "sandbox-a" || fake.request.Cols != 80 || fake.request.Rows != 24 {
		t.Fatalf("decoded request = %+v", fake.request)
	}
	var response service.TerminalGrantResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Token != fake.response.Token || response.WSURL != fake.response.WSURL {
		t.Fatalf("response = %+v", response)
	}
}

func TestTerminalGrantHandlerRejectsNonCanonicalJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"sandboxId":"sandbox-a","cols":80,"rows":24,"shell":"/bin/sh"}`},
		{name: "multiple values", body: `{"sandboxId":"sandbox-a","cols":80,"rows":24} {}`},
		{name: "oversized", body: `{"sandboxId":"` + string(bytes.Repeat([]byte{'a'}, maxTerminalGrantRequestBytes)) + `","cols":80,"rows":24}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeTerminalGrantService{}
			router, token := terminalGrantTestRouter(t, fake)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/terminal/grants", bytes.NewBufferString(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || recorder.Body.String() != "{\"error\":\"PROTOCOL_ERROR\"}" {
				t.Fatalf("status/body = %d %q", recorder.Code, recorder.Body.String())
			}
			if fake.calls != 0 {
				t.Fatalf("service calls = %d, want 0", fake.calls)
			}
		})
	}
}

func TestTerminalGrantHandlerMapsStableServiceErrors(t *testing.T) {
	tests := []struct {
		name       string
		serviceErr *service.TerminalError
	}{
		{name: "forbidden", serviceErr: &service.TerminalError{Status: http.StatusForbidden, Code: "FORBIDDEN"}},
		{name: "missing target", serviceErr: &service.TerminalError{Status: http.StatusNotFound, Code: "TARGET_NOT_FOUND"}},
		{name: "non-running target", serviceErr: &service.TerminalError{Status: http.StatusConflict, Code: "TARGET_NOT_RUNNING"}},
		{name: "limit", serviceErr: &service.TerminalError{Status: http.StatusTooManyRequests, Code: "LIMIT_EXCEEDED"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeTerminalGrantService{err: test.serviceErr}
			router, token := terminalGrantTestRouter(t, fake)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/terminal/grants", bytes.NewBufferString(`{"sandboxId":"sandbox-a","cols":80,"rows":24}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.serviceErr.Status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.serviceErr.Status)
			}
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["error"] != test.serviceErr.Code {
				t.Fatalf("body = %#v", body)
			}
		})
	}
}

func terminalGrantTestRouter(t *testing.T, fake terminalGrantService) (*gin.Engine, string) {
	t.Helper()
	manager := auth.NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, time.Hour)
	token, err := manager.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	router := gin.New()
	authed := router.Group("/api/v1", auth.Middleware(manager))
	NewTerminalGrantHandler(fake).Register(authed)
	return router, token
}
