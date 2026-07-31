// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

type fakeTerminalPTY struct {
	output   chan []byte
	input    chan []byte
	resize   chan cubesandbox.PtySize
	killed   chan struct{}
	waitErr  error
	exitCode int
	once     sync.Once
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newFakeTerminalPTY() *fakeTerminalPTY {
	return &fakeTerminalPTY{
		output: make(chan []byte, 4),
		input:  make(chan []byte, 4),
		resize: make(chan cubesandbox.PtySize, 4),
		killed: make(chan struct{}),
	}
}

func (p *fakeTerminalPTY) PID() int              { return 42 }
func (p *fakeTerminalPTY) Output() <-chan []byte { return p.output }
func (p *fakeTerminalPTY) Wait(func([]byte)) (int, error) {
	return p.exitCode, p.waitErr
}
func (p *fakeTerminalPTY) ErrorMessage() string { return "" }
func (p *fakeTerminalPTY) Disconnect() error    { return nil }
func (p *fakeTerminalPTY) SendStdin(_ context.Context, data []byte) error {
	p.input <- bytes.Clone(data)
	return nil
}
func (p *fakeTerminalPTY) Resize(_ context.Context, size cubesandbox.PtySize) error {
	p.resize <- size
	return nil
}
func (p *fakeTerminalPTY) Kill(context.Context) (bool, error) {
	p.once.Do(func() { close(p.killed) })
	return true, nil
}

type fakeTerminalFactory struct {
	mu         sync.Mutex
	ptys       []*fakeTerminalPTY
	openErr    error
	openCalls  int
	closeCalls int
	durations  []time.Duration
}

func (f *fakeTerminalFactory) Open(
	_ context.Context,
	_ string,
	_ cubesandbox.PtySize,
	maxDuration time.Duration,
) (terminalPTY, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
	f.durations = append(f.durations, maxDuration)
	if f.openErr != nil {
		return nil, f.openErr
	}
	pty := newFakeTerminalPTY()
	f.ptys = append(f.ptys, pty)
	return pty, nil
}

func (f *fakeTerminalFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func (f *fakeTerminalFactory) pty(index int) *fakeTerminalPTY {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.ptys) {
		return nil
	}
	return f.ptys[index]
}

func (f *fakeTerminalFactory) openCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openCalls
}

func terminalSandboxResponse(state int, envd bool) json.RawMessage {
	annotation := `{}`
	if envd {
		annotation = `{"cube.master.components.envd.version":"0.2.0"}`
	}
	return json.RawMessage(`{
		"ret":{"ret_code":0},
		"data":[{
			"sandbox_id":"sandbox-1",
			"status":` + strconv.Itoa(state) + `,
			"containers":[{"container_id":"sandbox-1","status":1,"type":"sandbox"}],
			"annotations":` + annotation + `
		}]
	}`)
}

func newTerminalTestHandler(
	t *testing.T,
	state int,
	envd bool,
) (*TerminalHandler, *auth.JWTManager, *fakeTerminalFactory, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	cm := &fakeCM{getSandbox: func(context.Context, string, string) (json.RawMessage, error) {
		calls.Add(1)
		return terminalSandboxResponse(state, envd), nil
	}}
	jm := auth.NewJWTManager("terminal-test-secret-32-bytes-long!", time.Minute, time.Hour)
	factory := &fakeTerminalFactory{}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	t.Cleanup(lifecycleCancel)
	return &TerminalHandler{
		cm:              cm,
		jm:              jm,
		factory:         factory,
		runtime:         defaultTerminalRuntimeConfig(),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		usedGrants:      make(map[string]time.Time),
		activeSessions:  make(map[string]int),
	}, jm, factory, &calls
}

func newTerminalTestServer(t *testing.T, h *TerminalHandler, jm *auth.JWTManager) (*httptest.Server, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	h.RegisterPublic(api)
	authed := r.Group("/api/v1", auth.Middleware(jm))
	h.RegisterAuthed(authed)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	token, err := jm.GenerateAccessToken("operator")
	if err != nil {
		t.Fatalf("generate access token: %v", err)
	}
	return server, token
}

func createTerminalSession(t *testing.T, server *httptest.Server, accessToken string) terminalSessionResponse {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/api/v1/terminal/sandboxes/sandbox-1/sessions",
		nil,
	)
	if err != nil {
		t.Fatalf("create terminal request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create terminal session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, body)
	}
	var session terminalSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatalf("decode terminal session: %v", err)
	}
	return session
}

func dialTerminal(t *testing.T, server *httptest.Server, session terminalSessionResponse, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/terminal/sandboxes/sandbox-1/ws"
	header := http.Header{"Origin": []string{origin}}
	dialer := websocket.Dialer{
		Subprotocols: []string{session.Protocol, terminalGrantPrefix + session.Grant},
	}
	return dialer.Dial(wsURL, header)
}

func TestTerminalSessionAndWebSocketRelay(t *testing.T) {
	h, jm, factory, calls := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	session := createTerminalSession(t, server, accessToken)
	if calls.Load() != 1 {
		t.Fatalf("session creation CubeMaster calls=%d, want one", calls.Load())
	}
	if session.Protocol != terminalProtocol ||
		session.WebsocketURL != "/opsapi/v1/terminal/sandboxes/sandbox-1/ws" ||
		session.Grant == "" {
		t.Fatalf("unexpected session response: %#v", session)
	}

	conn, _, err := dialTerminal(t, server, session, server.URL)
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	defer conn.Close()
	if calls.Load() != 2 {
		t.Fatalf("CubeMaster calls=%d, want two", calls.Load())
	}
	if conn.Subprotocol() != terminalProtocol {
		t.Fatalf("subprotocol=%q", conn.Subprotocol())
	}
	if messageType, _, err := conn.ReadMessage(); err != nil || messageType != websocket.TextMessage {
		t.Fatalf("connected frame: type=%d err=%v", messageType, err)
	}
	pty := factory.pty(0)
	if pty == nil {
		t.Fatal("PTY was not opened")
	}

	pty.output <- []byte("hello")
	messageType, payload, err := conn.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || string(payload) != "hello" {
		t.Fatalf("output: type=%d payload=%q err=%v", messageType, payload, err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("whoami\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	select {
	case input := <-pty.input:
		if string(input) != "whoami\n" {
			t.Fatalf("stdin=%q", input)
		}
	case <-time.After(time.Second):
		t.Fatal("stdin was not relayed")
	}

	if err := conn.WriteJSON(terminalControl{Type: "resize", Rows: 40, Cols: 120}); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	select {
	case size := <-pty.resize:
		if size.Rows != 40 || size.Cols != 120 {
			t.Fatalf("resize=%#v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("resize was not relayed")
	}

	pty.exitCode = 7
	close(pty.output)
	_, payload, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read exit frame: %v", err)
	}
	var exit struct {
		Type     string `json:"type"`
		ExitCode int    `json:"exitCode"`
	}
	if json.Unmarshal(payload, &exit) != nil || exit.Type != "exit" || exit.ExitCode != 7 {
		t.Fatalf("exit frame=%s", payload)
	}
	select {
	case <-pty.killed:
	case <-time.After(time.Second):
		t.Fatal("PTY was not cleaned up")
	}
}

func TestTerminalRejectsInvalidTargets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int
		envd  bool
	}{
		{name: "paused", state: 5, envd: true},
		{name: "without envd", state: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, jm, _, _ := newTerminalTestHandler(t, tc.state, tc.envd)
			server, accessToken := newTerminalTestServer(t, h, jm)
			req, err := http.NewRequest(
				http.MethodPost,
				server.URL+"/api/v1/terminal/sandboxes/sandbox-1/sessions",
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+accessToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusConflict, body)
			}
		})
	}
}

func TestTerminalRejectsCrossOriginWebSocket(t *testing.T) {
	h, jm, _, _ := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	session := createTerminalSession(t, server, accessToken)
	conn, resp, err := dialTerminal(t, server, session, "https://evil.example")
	if conn != nil {
		conn.Close()
		t.Fatal("terminal upgrade unexpectedly succeeded")
	}
	if err == nil || resp == nil {
		t.Fatalf("dial error=%v response=%v", err, resp)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusForbidden, body)
	}
}

func TestTerminalRejectsGrantForAnotherSandbox(t *testing.T) {
	h, jm, _, calls := newTerminalTestHandler(t, 1, true)
	grant, _, err := jm.GenerateTerminalGrant("operator", "another-sandbox", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Sec-WebSocket-Protocol", terminalProtocol+", "+terminalGrantPrefix+grant)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "sandbox-1"}}
	h.Connect(c)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("mismatched grant queried CubeMaster")
	}
}

func TestTerminalStreamFailureReturnsErrorFrame(t *testing.T) {
	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	conn, _, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read connected frame: %v", err)
	}
	pty := factory.pty(0)
	pty.waitErr = errors.New("upstream stream reset")
	close(pty.output)
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read error frame: %v", err)
	}
	var frame struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &frame) != nil || frame.Type != "error" {
		t.Fatalf("frame=%s, want error", payload)
	}
}

func TestTerminalShutdownCleansActivePTY(t *testing.T) {
	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	session := createTerminalSession(t, server, accessToken)
	conn, _, err := dialTerminal(t, server, session, server.URL)
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read connected frame: %v", err)
	}
	pty := factory.pty(0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown terminal handler: %v", err)
	}
	select {
	case <-pty.killed:
	default:
		t.Fatal("active PTY was not killed before shutdown returned")
	}
}

func TestTerminalSessionRequiresAuthentication(t *testing.T) {
	h, jm, _, calls := newTerminalTestHandler(t, 1, true)
	server, _ := newTerminalTestServer(t, h, jm)
	resp, err := http.Post(
		server.URL+"/api/v1/terminal/sandboxes/sandbox-1/sessions",
		"application/json",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusUnauthorized)
	}
	if calls.Load() != 0 {
		t.Fatal("unauthenticated request queried CubeMaster")
	}
}

func TestTerminalRejectsExpiredGrant(t *testing.T) {
	h, jm, _, calls := newTerminalTestHandler(t, 1, true)
	server, _ := newTerminalTestServer(t, h, jm)
	grant, _, err := jm.GenerateTerminalGrant("operator", "sandbox-1", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	conn, resp, err := dialTerminal(t, server, terminalSessionResponse{
		Grant:    grant,
		Protocol: terminalProtocol,
	}, server.URL)
	if conn != nil {
		conn.Close()
		t.Fatal("expired terminal grant unexpectedly connected")
	}
	if err == nil || resp == nil {
		t.Fatalf("dial error=%v response=%v", err, resp)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusUnauthorized)
	}
	if calls.Load() != 0 {
		t.Fatal("expired grant queried CubeMaster")
	}
}

func TestTerminalRejectsReplayedGrant(t *testing.T) {
	h, jm, factory, calls := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	session := createTerminalSession(t, server, accessToken)
	first, _, err := dialTerminal(t, server, session, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, _, err := first.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	second, resp, err := dialTerminal(t, server, session, server.URL)
	if second != nil {
		second.Close()
		t.Fatal("replayed terminal grant unexpectedly connected")
	}
	if err == nil || resp == nil {
		t.Fatalf("dial error=%v response=%v", err, resp)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusUnauthorized, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("CubeMaster calls=%d, want grant and first connection only", calls.Load())
	}
	if got := factory.openCallCount(); got != 1 {
		t.Fatalf("PTY open calls=%d, want one", got)
	}
}

func TestTerminalLimitsActiveSessionsPerOperatorAndSandbox(t *testing.T) {
	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	h.runtime.maxSessions = 1
	server, accessToken := newTerminalTestServer(t, h, jm)
	first, _, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, _, err := first.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	second, resp, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if second != nil {
		second.Close()
		t.Fatal("terminal session limit was not enforced")
	}
	if err == nil || resp == nil {
		t.Fatalf("dial error=%v response=%v", err, resp)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusTooManyRequests, body)
	}
	if got := factory.openCallCount(); got != 1 {
		t.Fatalf("PTY open calls=%d, want one", got)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-factory.pty(0).killed:
	case <-time.After(time.Second):
		t.Fatal("first PTY was not cleaned up")
	}
	deadline := time.Now().Add(time.Second)
	for {
		h.lifecycleMu.Lock()
		active := h.activeSessions["operator\x00sandbox-1"]
		h.lifecycleMu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal session slot was not released")
		}
		time.Sleep(time.Millisecond)
	}
	third, _, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if err != nil {
		t.Fatalf("dial after session slot release: %v", err)
	}
	defer third.Close()
	if _, _, err := third.ReadMessage(); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalIdleTimeoutCleansPTY(t *testing.T) {
	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	h.runtime.idleTimeout = 20 * time.Millisecond
	h.runtime.pingPeriod = time.Hour
	h.runtime.maxDuration = time.Hour
	server, accessToken := newTerminalTestServer(t, h, jm)
	conn, _, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read connected frame: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read idle timeout frame: %v", err)
	}
	if !strings.Contains(string(payload), "closed after inactivity") {
		t.Fatalf("idle timeout frame=%s", payload)
	}
	pty := factory.pty(0)
	select {
	case <-pty.killed:
	case <-time.After(time.Second):
		t.Fatal("idle terminal PTY was not killed")
	}
}

func TestTerminalMalformedControlFrameClosesProtocol(t *testing.T) {
	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	conn, _, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read connected frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseProtocolError) {
		t.Fatalf("close error=%v, want protocol close", err)
	}
	select {
	case <-factory.pty(0).killed:
	case <-time.After(time.Second):
		t.Fatal("protocol-error PTY was not killed")
	}
}

func TestTerminalShutdownRejectsNewGrants(t *testing.T) {
	h, jm, factory, calls := newTerminalTestHandler(t, 1, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	server, accessToken := newTerminalTestServer(t, h, jm)
	req, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/api/v1/terminal/sandboxes/sandbox-1/sessions",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if calls.Load() != 0 {
		t.Fatal("shutting-down handler queried CubeMaster")
	}
	if factory.closeCalls != 1 {
		t.Fatalf("factory close calls=%d want=1", factory.closeCalls)
	}
}

func TestTerminalShutdownWithInvalidProxyConfiguration(t *testing.T) {
	h := NewTerminalHandler(
		&fakeCM{},
		auth.NewJWTManager("terminal-test-secret-32-bytes-long!", time.Minute, time.Hour),
		&config.Config{SandboxProxyURL: "://invalid"},
	)
	if h.factoryErr == nil {
		t.Fatal("invalid proxy configuration unexpectedly created a terminal factory")
	}
	if h.factory != nil {
		t.Fatalf("factory=%#v, want nil interface", h.factory)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with invalid proxy configuration: %v", err)
	}
}

func TestTerminalConcurrentSessionsAreIsolated(t *testing.T) {
	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	first, _, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, _, err := first.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	second, _, err := dialTerminal(
		t,
		server,
		createTerminalSession(t, server, accessToken),
		server.URL,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, _, err := second.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	firstPTY := factory.pty(0)
	secondPTY := factory.pty(1)
	firstPTY.output <- []byte("first")
	secondPTY.output <- []byte("second")
	_, firstOutput, err := first.ReadMessage()
	if err != nil || string(firstOutput) != "first" {
		t.Fatalf("first output=%q err=%v", firstOutput, err)
	}
	_, secondOutput, err := second.ReadMessage()
	if err != nil || string(secondOutput) != "second" {
		t.Fatalf("second output=%q err=%v", secondOutput, err)
	}

	if err := first.WriteMessage(websocket.BinaryMessage, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := second.WriteMessage(websocket.BinaryMessage, []byte("two")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-firstPTY.input:
		if string(got) != "one" {
			t.Fatalf("first input=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first session input not relayed")
	}
	select {
	case got := <-secondPTY.input:
		if string(got) != "two" {
			t.Fatalf("second input=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("second session input not relayed")
	}
	if factory.durations[0] != 8*time.Hour || factory.durations[1] != 8*time.Hour {
		t.Fatalf("PTY durations=%v, want production maximum duration", factory.durations)
	}
}

func TestTerminalAuditFields(t *testing.T) {
	var logs synchronizedBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	server, accessToken := newTerminalTestServer(t, h, jm)
	session := createTerminalSession(t, server, accessToken)
	claims, err := jm.VerifyTerminalGrant(session.Grant)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := dialTerminal(t, server, session, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-factory.pty(0).killed:
	case <-time.After(time.Second):
		t.Fatal("disconnected PTY was not cleaned up")
	}

	hasEnded := func() bool {
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var record map[string]interface{}
			if json.Unmarshal([]byte(line), &record) == nil &&
				record["event"] == "terminal.ended" &&
				record["session_id"] == claims.ID {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(time.Second)
	for !hasEnded() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	events := map[string]map[string]interface{}{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]interface{}
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		event, _ := record["event"].(string)
		if strings.HasPrefix(event, "terminal.") && record["session_id"] == claims.ID {
			events[event] = record
		}
	}
	for _, event := range []string{"terminal.grant", "terminal.started", "terminal.ended"} {
		record := events[event]
		if record == nil {
			t.Fatalf("missing %s audit event in %s", event, logs.String())
		}
		for _, field := range []string{
			"session_id",
			"operator",
			"sandbox_id",
			"container_id",
			"remote_addr",
			"timestamp",
			"duration_ms",
			"close_reason",
		} {
			if _, ok := record[field]; !ok {
				t.Fatalf("%s audit event missing %s: %#v", event, field, record)
			}
		}
		if record["operator"] != "operator" ||
			record["sandbox_id"] != "sandbox-1" ||
			record["container_id"] != "sandbox-1" {
			t.Fatalf("unexpected %s audit identity: %#v", event, record)
		}
	}
	if events["terminal.ended"]["close_reason"] != "client_disconnected" {
		t.Fatalf("ended audit=%#v", events["terminal.ended"])
	}
}

func TestTerminalFailureAuditFields(t *testing.T) {
	var logs synchronizedBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	h, jm, factory, _ := newTerminalTestHandler(t, 1, true)
	factory.openErr = errors.New("envd unavailable")
	server, accessToken := newTerminalTestServer(t, h, jm)
	session := createTerminalSession(t, server, accessToken)
	claims, err := jm.VerifyTerminalGrant(session.Grant)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := dialTerminal(t, server, session, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read PTY failure frame: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var record map[string]interface{}
			if json.Unmarshal([]byte(line), &record) != nil ||
				record["event"] != "terminal.failed" ||
				record["session_id"] != claims.ID {
				continue
			}
			if record["container_id"] != "sandbox-1" ||
				record["close_reason"] != "pty_start_failed" ||
				record["error"] != "envd unavailable" {
				t.Fatalf("unexpected failure audit: %#v", record)
			}
			for _, field := range []string{
				"operator",
				"sandbox_id",
				"remote_addr",
				"timestamp",
				"duration_ms",
			} {
				if _, ok := record[field]; !ok {
					t.Fatalf("failure audit missing %s: %#v", field, record)
				}
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("missing terminal.failed audit event in %s", logs.String())
}

func TestTerminalProtocolHelpers(t *testing.T) {
	header := "other.v1, cube-terminal.v1, cube-terminal.grant.abc.def"
	if !terminalProtocolRequested(header) {
		t.Fatal("application protocol was not detected")
	}
	if got := terminalGrantFromProtocols(header); got != "abc.def" {
		t.Fatalf("grant=%q", got)
	}
	if terminalProtocolRequested("cube-terminal.v10") {
		t.Fatal("prefix match was accepted")
	}
}
