// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

func TestTerminalTicketAndWebSocketLifecycle(t *testing.T) {
	cm := &fakeCM{getSandbox: func(context.Context, string, string) (json.RawMessage, error) {
		return json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sb-123","status":1}]}`), nil
	}}
	backend := newFakeTerminalBackend()
	jm := auth.NewJWTManager("terminal-test-secret", time.Minute, time.Hour)
	h := NewTerminalHandler(cm, jm, backend)

	router := gin.New()
	authed := router.Group("/api/v1", auth.Middleware(jm))
	h.RegisterAuthed(authed.Group("/sdk"))
	h.RegisterPublic(router.Group("/api/v1/sdk"))
	srv := httptest.NewServer(router)
	defer srv.Close()

	ticket := requestTerminalTicket(t, srv.URL, jm, "sb-123")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/sdk/sandboxes/sb-123/terminal/ws"
	dialer := websocket.Dialer{Subprotocols: []string{terminalProtocol, terminalTicketPrefix + ticket}}
	header := http.Header{"Origin": []string{srv.URL}}
	conn, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Dial: %v, HTTP %d: %s", err, resp.StatusCode, body)
		}
		t.Fatalf("Dial: %v", err)
	}
	if conn.Subprotocol() != terminalProtocol {
		t.Fatalf("subprotocol = %q", conn.Subprotocol())
	}

	var ready terminalServerMessage
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if ready.Type != "ready" || ready.PID != 77 {
		t.Fatalf("ready = %#v", ready)
	}
	messageType, output, err := conn.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || string(output) != "welcome\r\n" {
		t.Fatalf("output type=%d body=%q err=%v", messageType, output, err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo ok\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := conn.WriteJSON(terminalClientMessage{Type: "resize", Rows: 40, Cols: 120}); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := conn.WriteJSON(terminalClientMessage{Type: "close"}); err != nil {
		t.Fatalf("write close: %v", err)
	}
	_ = conn.Close()

	select {
	case <-backend.session.killed:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal session was not killed on disconnect")
	}
	backend.session.mu.Lock()
	if !bytes.Equal(backend.session.input, []byte("echo ok\n")) {
		t.Errorf("input = %q", backend.session.input)
	}
	if backend.session.size != (service.TerminalSize{Rows: 40, Cols: 120}) {
		t.Errorf("resize = %#v", backend.session.size)
	}
	backend.session.mu.Unlock()

	// The same ticket cannot establish another session even before its TTL.
	_, replayResp, replayErr := dialer.Dial(wsURL, header)
	if replayErr == nil || replayResp == nil || replayResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticket replay: err=%v status=%v", replayErr, responseStatus(replayResp))
	}
}

func TestTerminalTicketRejectsPausedSandbox(t *testing.T) {
	cm := &fakeCM{getSandbox: func(context.Context, string, string) (json.RawMessage, error) {
		return json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sb-paused","status":5}]}`), nil
	}}
	jm := auth.NewJWTManager("terminal-test-secret", time.Minute, time.Hour)
	h := NewTerminalHandler(cm, jm, newFakeTerminalBackend())
	router := gin.New()
	h.RegisterAuthed(router.Group("/api/v1/sdk", auth.Middleware(jm)))
	srv := httptest.NewServer(router)
	defer srv.Close()

	access, _ := jm.GenerateAccessToken("admin")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sdk/sandboxes/sb-paused/terminal/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestTerminalOpenTimeoutCancelsBackend(t *testing.T) {
	cm := &fakeCM{getSandbox: func(context.Context, string, string) (json.RawMessage, error) {
		return json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sb-slow","status":1}]}`), nil
	}}
	backend := &blockingTerminalBackend{cancelled: make(chan struct{})}
	jm := auth.NewJWTManager("terminal-test-secret", time.Minute, time.Hour)
	h := NewTerminalHandler(cm, jm, backend)
	h.openTimeout = 20 * time.Millisecond
	router := gin.New()
	h.RegisterAuthed(router.Group("/api/v1/sdk", auth.Middleware(jm)))
	h.RegisterPublic(router.Group("/api/v1/sdk"))
	srv := httptest.NewServer(router)
	defer srv.Close()

	ticket := requestTerminalTicket(t, srv.URL, jm, "sb-slow")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/sdk/sandboxes/sb-slow/terminal/ws"
	dialer := websocket.Dialer{Subprotocols: []string{terminalProtocol, terminalTicketPrefix + ticket}}
	conn, resp, err := dialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	if err != nil {
		t.Fatalf("Dial: %v, status=%v", err, responseStatus(resp))
	}
	defer conn.Close()
	var message terminalServerMessage
	if err := conn.ReadJSON(&message); err != nil {
		t.Fatalf("read timeout message: %v", err)
	}
	if message.Type != "error" || !strings.Contains(message.Message, "timed out") {
		t.Fatalf("timeout message = %#v", message)
	}
	select {
	case <-backend.cancelled:
	case <-time.After(time.Second):
		t.Fatal("backend context was not cancelled after opening timeout")
	}
}

func TestTerminalPongsDoNotResetIdleTimeout(t *testing.T) {
	cm := &fakeCM{getSandbox: func(context.Context, string, string) (json.RawMessage, error) {
		return json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sb-idle","status":1}]}`), nil
	}}
	backend := newFakeTerminalBackend()
	jm := auth.NewJWTManager("terminal-test-secret", time.Minute, time.Hour)
	h := NewTerminalHandler(cm, jm, backend)
	h.idleTimeout = 75 * time.Millisecond
	h.idleCheck = 10 * time.Millisecond
	h.pingInterval = 10 * time.Millisecond
	h.pongTimeout = 250 * time.Millisecond
	router := gin.New()
	h.RegisterAuthed(router.Group("/api/v1/sdk", auth.Middleware(jm)))
	h.RegisterPublic(router.Group("/api/v1/sdk"))
	srv := httptest.NewServer(router)
	defer srv.Close()

	ticket := requestTerminalTicket(t, srv.URL, jm, "sb-idle")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/sdk/sandboxes/sb-idle/terminal/ws"
	dialer := websocket.Dialer{Subprotocols: []string{terminalProtocol, terminalTicketPrefix + ticket}}
	conn, resp, err := dialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	if err != nil {
		t.Fatalf("Dial: %v, status=%v", err, responseStatus(resp))
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))

	var ready terminalServerMessage
	if err := conn.ReadJSON(&ready); err != nil || ready.Type != "ready" {
		t.Fatalf("ready = %#v, err=%v", ready, err)
	}
	if messageType, _, err := conn.ReadMessage(); err != nil || messageType != websocket.BinaryMessage {
		t.Fatalf("read initial output: type=%d err=%v", messageType, err)
	}
	// ReadMessage processes server pings and Gorilla automatically answers
	// with pongs. Those liveness frames must not count as user activity.
	var timeout terminalServerMessage
	if err := conn.ReadJSON(&timeout); err != nil {
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || !strings.Contains(closeErr.Text, "inactivity") {
			t.Fatalf("read idle timeout: %v", err)
		}
	} else if timeout.Type != "error" || !strings.Contains(timeout.Message, "inactivity") {
		t.Fatalf("idle timeout message = %#v", timeout)
	} else {
		// Process the following close frame so the server can complete the
		// bounded WebSocket close handshake before cleaning up the PTY.
		_, _, err := conn.ReadMessage()
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || !strings.Contains(closeErr.Text, "inactivity") {
			t.Fatalf("idle close frame: %v", err)
		}
	}
	select {
	case <-backend.session.killed:
	case <-time.After(time.Second):
		t.Fatal("idle terminal session was not killed")
	}
}

func TestTerminalReadDeadlineReapsUnresponsivePeer(t *testing.T) {
	cm := &fakeCM{getSandbox: func(context.Context, string, string) (json.RawMessage, error) {
		return json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sb-unresponsive","status":1}]}`), nil
	}}
	backend := newFakeTerminalBackend()
	jm := auth.NewJWTManager("terminal-test-secret", time.Minute, time.Hour)
	h := NewTerminalHandler(cm, jm, backend)
	h.pingInterval = time.Hour
	h.pongTimeout = 40 * time.Millisecond
	router := gin.New()
	h.RegisterAuthed(router.Group("/api/v1/sdk", auth.Middleware(jm)))
	h.RegisterPublic(router.Group("/api/v1/sdk"))
	srv := httptest.NewServer(router)
	defer srv.Close()

	ticket := requestTerminalTicket(t, srv.URL, jm, "sb-unresponsive")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/sdk/sandboxes/sb-unresponsive/terminal/ws"
	dialer := websocket.Dialer{Subprotocols: []string{terminalProtocol, terminalTicketPrefix + ticket}}
	conn, resp, err := dialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	if err != nil {
		t.Fatalf("Dial: %v, status=%v", err, responseStatus(resp))
	}
	defer conn.Close()
	// Do not read from or write to the socket. The server-side read deadline
	// must release the PTY and limiter slot without relying on OS TCP timeout.
	select {
	case <-backend.session.killed:
	case <-time.After(time.Second):
		t.Fatal("unresponsive terminal peer was not reaped by the read deadline")
	}
}

func TestTerminalTicketReportsMalformedSandboxDetailAsBadGateway(t *testing.T) {
	cm := &fakeCM{getSandbox: func(context.Context, string, string) (json.RawMessage, error) {
		return json.RawMessage(`{"ret":{"ret_code":0},"data":{"sandbox_id":"sb-malformed","status":1}}`), nil
	}}
	jm := auth.NewJWTManager("terminal-test-secret", time.Minute, time.Hour)
	h := NewTerminalHandler(cm, jm, newFakeTerminalBackend())
	router := gin.New()
	h.RegisterAuthed(router.Group("/api/v1/sdk", auth.Middleware(jm)))
	srv := httptest.NewServer(router)
	defer srv.Close()

	access, _ := jm.GenerateAccessToken("admin")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sdk/sandboxes/sb-malformed/terminal/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestTerminalSameOriginAndProtocolParsing(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://cube.example/ws", nil)
	req.Host = "cube.example"
	req.Header.Set("Origin", "https://cube.example")
	if !terminalSameOrigin(req) {
		t.Fatal("same host origin was rejected")
	}
	req.Header.Set("Origin", "https://evil.example")
	if terminalSameOrigin(req) {
		t.Fatal("cross-site origin was accepted")
	}
	req.Host = "cube.example:12088"
	req.Header.Set("Origin", "http://cube.example:12088")
	if !terminalSameOrigin(req) {
		t.Fatal("same origin with a non-default port was rejected")
	}
	req.Header.Set("Origin", "http://cube.example:12089")
	if terminalSameOrigin(req) {
		t.Fatal("origin with a different port was accepted")
	}
	if got := terminalTicketFromProtocols("cube-terminal, cube-ticket.abc.def"); got != "abc.def" {
		t.Fatalf("ticket = %q", got)
	}
}

func requestTerminalTicket(t *testing.T, baseURL string, jm *auth.JWTManager, sandboxID string) string {
	t.Helper()
	access, err := jm.GenerateAccessToken("admin")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/sdk/sandboxes/"+sandboxID+"/terminal/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("ticket status = %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.Ticket == "" {
		t.Fatalf("ticket response: %#v, %v", result, err)
	}
	return result.Ticket
}

func responseStatus(resp *http.Response) any {
	if resp == nil {
		return nil
	}
	return resp.StatusCode
}

type fakeTerminalBackend struct {
	session *fakeTerminalSession
}

type blockingTerminalBackend struct {
	cancelled chan struct{}
}

func (b *blockingTerminalBackend) Open(ctx context.Context, _ string, _ service.TerminalSize) (service.TerminalSession, error) {
	<-ctx.Done()
	close(b.cancelled)
	return nil, ctx.Err()
}

func newFakeTerminalBackend() *fakeTerminalBackend {
	s := &fakeTerminalSession{
		output: make(chan []byte, 1),
		done:   make(chan service.TerminalExit),
		killed: make(chan struct{}),
	}
	s.output <- []byte("welcome\r\n")
	return &fakeTerminalBackend{session: s}
}

func (b *fakeTerminalBackend) Open(context.Context, string, service.TerminalSize) (service.TerminalSession, error) {
	return b.session, nil
}

type fakeTerminalSession struct {
	mu       sync.Mutex
	input    []byte
	size     service.TerminalSize
	output   chan []byte
	done     chan service.TerminalExit
	killed   chan struct{}
	killOnce sync.Once
}

func (s *fakeTerminalSession) PID() int                          { return 77 }
func (s *fakeTerminalSession) Output() <-chan []byte             { return s.output }
func (s *fakeTerminalSession) Done() <-chan service.TerminalExit { return s.done }
func (s *fakeTerminalSession) Close() error                      { return nil }
func (s *fakeTerminalSession) SendInput(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.input = append(s.input, data...)
	return nil
}
func (s *fakeTerminalSession) Resize(_ context.Context, size service.TerminalSize) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.size = size
	return nil
}
func (s *fakeTerminalSession) Kill(context.Context) error {
	s.killOnce.Do(func() { close(s.killed) })
	return nil
}
