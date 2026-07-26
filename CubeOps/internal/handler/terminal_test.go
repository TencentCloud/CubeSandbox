// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
)

func TestTerminalTicketIsSingleUseAndBoundToSandbox(t *testing.T) {
	store := newTerminalTicketStore()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	ticket := terminalTicket{
		SandboxID: "sandbox-a",
		CreatedBy: "sam",
		ExpiresAt: now.Add(time.Minute),
	}

	token, err := store.issue(ticket)
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	if _, err := store.claim(token, "sandbox-b"); err == nil {
		t.Fatal("ticket must not be valid for another sandbox")
	}
	if _, err := store.claim(token, "sandbox-a"); err == nil {
		t.Fatal("sandbox mismatch must consume the ticket")
	}

	token, err = store.issue(ticket)
	if err != nil {
		t.Fatalf("issue second ticket: %v", err)
	}
	if _, err := store.claim(token, "sandbox-a"); err != nil {
		t.Fatalf("claim valid ticket: %v", err)
	}
	if _, err := store.claim(token, "sandbox-a"); err == nil {
		t.Fatal("ticket must be single use")
	}
}

func TestTerminalTicketExpires(t *testing.T) {
	store := newTerminalTicketStore()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	token, err := store.issue(terminalTicket{
		SandboxID: "sandbox-a",
		CreatedBy: "sam",
		ExpiresAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := store.claim(token, "sandbox-a"); err == nil {
		t.Fatal("expired ticket should be rejected")
	}
}

func TestTerminalTicketClaimPrunesExpiredTickets(t *testing.T) {
	store := newTerminalTicketStore()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	expired, err := store.issue(terminalTicket{
		SandboxID: "sandbox-a",
		CreatedBy: "sam",
		ExpiresAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("issue expired ticket candidate: %v", err)
	}
	valid, err := store.issue(terminalTicket{
		SandboxID: "sandbox-a",
		CreatedBy: "sam",
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("issue valid ticket: %v", err)
	}

	now = now.Add(2 * time.Second)
	if _, err := store.claim(valid, "sandbox-a"); err != nil {
		t.Fatalf("claim valid ticket: %v", err)
	}
	if _, ok := store.tickets[expired]; ok {
		t.Fatal("claim should prune expired pending tickets")
	}
}

func TestTerminalTicketRateLimitPerUser(t *testing.T) {
	store := newTerminalTicketStore()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	for i := 0; i < terminalMaxTicketsPerWindow; i++ {
		token, err := store.issue(terminalTicket{
			SandboxID: "sandbox-a",
			CreatedBy: "sam",
			ExpiresAt: now.Add(terminalTicketTTL),
		})
		if err != nil {
			t.Fatalf("issue ticket %d: %v", i, err)
		}
		if _, err := store.claim(token, "sandbox-a"); err != nil {
			t.Fatalf("claim ticket %d: %v", i, err)
		}
	}

	if _, err := store.issue(terminalTicket{
		SandboxID: "sandbox-a",
		CreatedBy: "sam",
		ExpiresAt: now.Add(terminalTicketTTL),
	}); err == nil {
		t.Fatal("ticket creation above the per-user rate should be rejected")
	}

	now = now.Add(terminalTicketRateWindow + time.Nanosecond)
	if _, err := store.issue(terminalTicket{
		SandboxID: "sandbox-a",
		CreatedBy: "sam",
		ExpiresAt: now.Add(terminalTicketTTL),
	}); err != nil {
		t.Fatalf("ticket rate should reset after the window: %v", err)
	}
}

func TestTerminalSessionLimiterPerUser(t *testing.T) {
	limiter := newTerminalSessionLimiter()
	releases := make([]func(), 0, terminalMaxSessionsPerUser)
	for i := 0; i < terminalMaxSessionsPerUser; i++ {
		release, err := limiter.acquire("sam", fmt.Sprintf("sandbox-%d", i))
		if err != nil {
			t.Fatalf("acquire session %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	if _, err := limiter.acquire("sam", "sandbox-extra"); err == nil {
		t.Fatal("session above the per-user limit should be rejected")
	}

	releases[0]()
	releases[0]()
	if _, err := limiter.acquire("sam", "sandbox-extra"); err != nil {
		t.Fatalf("released session should restore capacity: %v", err)
	}
}

func TestTerminalSessionLimiterPerSandbox(t *testing.T) {
	limiter := newTerminalSessionLimiter()
	for i := 0; i < terminalMaxSessionsPerSandbox; i++ {
		if _, err := limiter.acquire(fmt.Sprintf("user-%d", i), "sandbox-a"); err != nil {
			t.Fatalf("acquire session %d: %v", i, err)
		}
	}
	if _, err := limiter.acquire("user-extra", "sandbox-a"); err == nil {
		t.Fatal("session above the per-sandbox limit should be rejected")
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

func TestValidateTerminalTicketRequest(t *testing.T) {
	valid := &terminalTicketRequest{
		User: "root",
		CWD:  "/workspace",
		Envs: map[string]string{"TERM": "xterm-256color"},
	}
	if err := validateTerminalTicketRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	for name, request := range map[string]*terminalTicketRequest{
		"unsupported user": {User: "nobody"},
		"relative cwd":     {CWD: "workspace"},
		"bad env key":      {Envs: map[string]string{"BAD=KEY": "value"}},
		"large rows":       {Rows: terminalMaxDimension + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateTerminalTicketRequest(request); err == nil {
				t.Fatal("expected validation error")
			}
		})
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

func TestCubeMasterTerminalURL(t *testing.T) {
	got, err := cubeMasterTerminalURL("https://master.example.com:8443/api?token=hidden")
	if err != nil {
		t.Fatalf("cubeMasterTerminalURL: %v", err)
	}
	if got != "wss://master.example.com:8443/cube/sandbox/terminal" {
		t.Fatalf("unexpected terminal URL %q", got)
	}
	if _, err := cubeMasterTerminalURL("ftp://master.example.com"); err == nil {
		t.Fatal("unsupported scheme should be rejected")
	}
}

func TestDecodeTerminalSandboxDetail(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"ret": map[string]interface{}{"ret_code": 0},
		"data": []interface{}{
			map[string]interface{}{
				"sandbox_id": "sandbox-a",
				"status":     1,
				"containers": []interface{}{
					map[string]interface{}{"container_id": "main", "status": 1},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
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
	gateway := NewTerminalGateway(cm, "http://master:8089", "shared-secret")
	router := gin.New()
	group := router.Group("/api/v1/sdk", func(c *gin.Context) {
		c.Set("username", "sam")
		c.Next()
	})
	gateway.RegisterAuthed(group)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/sdk/sandboxes/sandbox-a/terminal/tickets",
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
	if body.WebSocketURL == "" || body.ExpiresAt == "" {
		t.Fatalf("missing terminal connection metadata: %+v", body)
	}
	if _, err := gateway.tickets.claim(body.Ticket, "sandbox-a"); err != nil {
		t.Fatalf("issued ticket cannot be claimed: %v", err)
	}
}
