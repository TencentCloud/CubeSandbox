// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
)

func TestTerminalTicketWorksAcrossGatewayReplicas(t *testing.T) {
	secret := []byte("shared-jwt-secret")
	claims := validTerminalClaims(time.Now().Add(time.Minute))
	token, err := signTerminalTicket(secret, claims)
	if err != nil {
		t.Fatalf("sign ticket: %v", err)
	}

	parsed, err := parseTerminalTicket(secret, token)
	if err != nil {
		t.Fatalf("parse ticket on another replica: %v", err)
	}
	if parsed.SandboxID != claims.SandboxID || parsed.ContainerID != claims.ContainerID {
		t.Fatalf("unexpected parsed ticket: %+v", parsed)
	}
	if _, err := parseTerminalTicket([]byte("different-secret"), token); err == nil {
		t.Fatal("ticket signed by another deployment must be rejected")
	}
}

func TestTerminalTicketExpiresAndChecksAudience(t *testing.T) {
	secret := []byte("shared-jwt-secret")
	expired := validTerminalClaims(time.Now().Add(-time.Minute))
	expired.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
	expired.NotBefore = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
	token, err := signTerminalTicket(secret, expired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseTerminalTicket(secret, token); err == nil {
		t.Fatal("expired ticket should be rejected")
	}

	wrongAudience := validTerminalClaims(time.Now().Add(time.Minute))
	wrongAudience.Audience = jwt.ClaimStrings{"another-service"}
	token, err = signTerminalTicket(secret, wrongAudience)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseTerminalTicket(secret, token); err == nil {
		t.Fatal("ticket with another audience should be rejected")
	}
}

func TestRunCubeOpsTerminalRelayRecoversPanic(t *testing.T) {
	reason := runCubeOpsTerminalRelay("test", func() string {
		panic("relay failed")
	})
	if reason != terminalCloseRelayFailure {
		t.Fatalf("unexpected panic close reason %q", reason)
	}
}

func TestTerminalTicketFromProtocols(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://cube.test/terminal", nil)
	request.Header.Set("Sec-WebSocket-Protocol", terminalWSProtocol+", signed.ticket")
	if got, err := terminalTicketFromProtocols(request); err != nil || got != "signed.ticket" {
		t.Fatalf("terminalTicketFromProtocols() = %q, %v", got, err)
	}

	request.Header.Set("Sec-WebSocket-Protocol", "other-protocol, signed.ticket")
	if _, err := terminalTicketFromProtocols(request); err == nil {
		t.Fatal("unexpected WebSocket protocol should be rejected")
	}
}

func TestTerminalDimensionsUseEnvdLimits(t *testing.T) {
	rows, cols, err := terminalDimensions(1, terminalMaximumEnvdCols)
	if err != nil || rows != 1 || cols != terminalMaximumEnvdCols {
		t.Fatalf("terminalDimensions() = %d x %d, %v", rows, cols, err)
	}
	rows, cols, err = terminalDimensions(0, 0)
	if err != nil || rows != terminalDefaultRows || cols != terminalDefaultCols {
		t.Fatalf("default terminalDimensions() = %d x %d, %v", rows, cols, err)
	}
	if _, _, err := terminalDimensions(terminalMaximumEnvdRows+1, 80); err == nil {
		t.Fatal("oversized terminal dimensions should be rejected")
	}
}

func TestSelectTerminalContainer(t *testing.T) {
	detail := &translator.CMSandboxDetailItem{
		SandboxID: "sandbox-a",
		Status:    1,
		Containers: []translator.CMSandboxContainer{
			{ContainerID: "main", Status: 1, Type: "sandbox"},
			{ContainerID: "sidecar", Status: 2, Type: "sidecar"},
		},
	}

	if got, err := selectTerminalContainer(detail, "main"); err != nil || got != "main" {
		t.Fatalf("select requested running container: got %q, err %v", got, err)
	}
	if _, err := selectTerminalContainer(detail, "sidecar"); err == nil {
		t.Fatal("stopped container should be rejected")
	}
	if _, err := selectTerminalContainer(detail, "missing"); err == nil {
		t.Fatal("missing container should be rejected")
	}
	detail.Containers[1].Status = 1
	if _, err := selectTerminalContainer(detail, "sidecar"); err == nil {
		t.Fatal("container without envd PTY should be rejected")
	}

	detail.Containers = detail.Containers[:1]
	if got, err := selectTerminalContainer(detail, ""); err != nil || got != "main" {
		t.Fatalf("single running container should be selected: got %q, err %v", got, err)
	}

	detail.Containers = nil
	if got, err := selectTerminalContainer(detail, ""); err != nil || got != "sandbox-a" {
		t.Fatalf("legacy detail should fall back to sandbox id: got %q, err %v", got, err)
	}
}

func TestTerminalOriginAllowed(t *testing.T) {
	for name, test := range map[string]struct {
		host   string
		origin string
		want   bool
	}{
		"same origin":        {"cube.example.com", "https://cube.example.com", true},
		"cross origin":       {"cube.example.com", "https://evil.example.com", false},
		"loopback dev":       {"127.0.0.1:3000", "http://localhost:5173", true},
		"loopback lookalike": {"127.0.0.1:3000", "http://localhost.evil.example", false},
		"missing production": {"cube.example.com", "", false},
		"missing loopback":   {"localhost:3000", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/terminal", nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if got := terminalOriginAllowed(request); got != test.want {
				t.Fatalf("terminalOriginAllowed() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDecodeTerminalSandboxDetail(t *testing.T) {
	raw := json.RawMessage(`{
		"ret":{"ret_code":0},
		"data":[{
			"sandbox_id":"sandbox-a",
			"status":1,
			"containers":[{"container_id":"main","status":1}]
		}]
	}`)
	detail, err := decodeTerminalSandboxDetail(raw)
	if err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.SandboxID != "sandbox-a" || len(detail.Containers) != 1 {
		t.Fatalf("unexpected detail: %+v", detail)
	}
}

func TestCreateTerminalTicketHandler(t *testing.T) {
	cm := &fakeCM{
		getSandbox: func(_ context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
			if sandboxID != "sandbox-a" || instanceType != sdkInstanceType {
				t.Fatalf("unexpected CubeMaster lookup: %q %q", sandboxID, instanceType)
			}
			return json.RawMessage(`{
				"ret":{"ret_code":0},
				"data":[{
					"sandbox_id":"sandbox-a",
					"status":1,
					"containers":[{"container_id":"main","status":1,"type":"sandbox"}]
				}]
			}`), nil
		},
	}
	gateway := NewTerminalGateway(cm, "shared-secret", "cube.test")
	router := gin.New()
	group := router.Group("/api/v1", func(c *gin.Context) {
		c.Set("username", "sam")
		c.Next()
	})
	gateway.RegisterAuthed(group)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/terminal/sandboxes/sandbox-a/tickets",
		bytes.NewBufferString(`{"containerID":"main","rows":24,"cols":80}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body terminalTicketResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Ticket == "" || body.ContainerID != "main" {
		t.Fatalf("unexpected ticket response: %+v", body)
	}
	if strings.Contains(body.WebSocketURL, body.Ticket) || strings.Contains(body.WebSocketURL, "ticket=") {
		t.Fatalf("terminal ticket must not be embedded in WebSocket URL: %q", body.WebSocketURL)
	}
	claims, err := parseTerminalTicket([]byte("shared-secret"), body.Ticket)
	if err != nil {
		t.Fatalf("issued ticket cannot be validated by another replica: %v", err)
	}
	if claims.CreatedBy != "sam" || claims.SandboxID != "sandbox-a" {
		t.Fatalf("unexpected ticket claims: %+v", claims)
	}
}

func TestCreateTerminalTicketRateLimitRunsBeforeCubeMasterLookup(t *testing.T) {
	lookups := 0
	cm := &fakeCM{
		getSandbox: func(_ context.Context, _, _ string) (json.RawMessage, error) {
			lookups++
			return json.RawMessage(`{
				"ret":{"ret_code":0},
				"data":[{
					"sandbox_id":"sandbox-a",
					"status":1,
					"containers":[{"container_id":"main","status":1,"type":"sandbox"}]
				}]
			}`), nil
		},
	}
	gateway := NewTerminalGateway(cm, "shared-secret", "cube.test")
	gateway.ticketLimiter = newTerminalTicketLimiter(1, time.Minute)
	router := gin.New()
	group := router.Group("/api/v1", func(c *gin.Context) {
		c.Set("username", "sam")
		c.Next()
	})
	gateway.RegisterAuthed(group)

	send := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/terminal/sandboxes/sandbox-a/tickets",
			bytes.NewBufferString(`{"containerID":"main","rows":24,"cols":80}`),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	if response := send(); response.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body = %s", response.Code, response.Body.String())
	}
	response := send()
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("limited status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", response.Header().Get("Retry-After"))
	}
	if lookups != 1 {
		t.Fatalf("CubeMaster lookups = %d, want 1", lookups)
	}
}

func validTerminalClaims(expiresAt time.Time) terminalTicketClaims {
	now := time.Now()
	return terminalTicketClaims{
		SandboxID:   "sandbox-a",
		ContainerID: "main",
		CreatedBy:   "sam",
		Rows:        24,
		Cols:        80,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    terminalTicketIssuer,
			Audience:  jwt.ClaimStrings{terminalTicketAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
}
