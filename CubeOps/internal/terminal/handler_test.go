// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
)

// fakeCM answers GetSandbox with a CubeMaster-shaped envelope whose status
// drives the running/paused check in CreateTicket.
type fakeCM struct {
	status int // CubeMaster status int: 1=running, 5=paused
	err    error
	empty  bool
}

func (f *fakeCM) GetSandbox(_ context.Context, sandboxID, _ string) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return json.RawMessage(`{"ret":{"ret_code":200},"data":[]}`), nil
	}
	return json.RawMessage(fmt.Sprintf(
		`{"ret":{"ret_code":200},"data":[{"sandbox_id":%q,"status":%d,"containers":[]}]}`,
		sandboxID, f.status)), nil
}

// newTestServer wires the terminal handler into a gin engine on an httptest
// server, mirroring the production route layout: the ticket endpoint behind
// auth, the WebSocket endpoint public.
func newTestServer(t *testing.T, cm SandboxInfoClient, idleTimeout time.Duration) (*httptest.Server, *Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(cm, "test-secret", "cube.app", idleTimeout)
	t.Cleanup(h.registry.Stop)

	r := gin.New()
	public := r.Group("/api/v1")
	h.RegisterPublic(public)
	// Stand-in for the JWT middleware: the username the ticket records.
	authed := r.Group("/api/v1/sdk", func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.ContextWithUsername(c.Request.Context(), "alice"))
		c.Next()
	})
	h.RegisterAuthed(authed)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, h
}

// createTicket calls the ticket endpoint and returns the response status and
// the ticket string (empty when the request failed).
func createTicket(t *testing.T, srv *httptest.Server, sandboxID string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/v1/sdk/sandboxes/"+sandboxID+"/terminal", "application/json", nil)
	if err != nil {
		t.Fatalf("ticket request: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Ticket string `json:"ticket"`
		WSPath string `json:"wsPath"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Ticket
}

// dialWS opens the terminal WebSocket with the given query parameters.
func dialWS(t *testing.T, srv *httptest.Server, params url.Values) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/terminal/ws?" + params.Encode()
	return websocket.DefaultDialer.Dial(wsURL, nil)
}

func readMsg(t *testing.T, conn *websocket.Conn) wsMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read ws message: %v", err)
	}
	return msg
}

func TestCreateTicketRequiresRunningSandbox(t *testing.T) {
	tests := []struct {
		name       string
		cm         *fakeCM
		wantStatus int
	}{
		{"running", &fakeCM{status: 1}, http.StatusOK},
		{"paused", &fakeCM{status: 5}, http.StatusConflict},
		{"pausing", &fakeCM{status: 4}, http.StatusConflict},
		{"missing", &fakeCM{empty: true}, http.StatusNotFound},
		// A deleted sandbox surfaces as a CubeMaster business error, which
		// must still read as 404 rather than a gateway failure.
		{"deleted", &fakeCM{err: &cubemaster.CMError{RetCode: 130404, RetMsg: "sandbox id not found"}}, http.StatusNotFound},
		{"cubemaster down", &fakeCM{err: fmt.Errorf("boom")}, http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t, tc.cm, DefaultIdleTimeout)
			status, ticket := createTicket(t, srv, "sbx-1")
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK && ticket == "" {
				t.Fatal("no ticket in a 200 response")
			}
		})
	}
}

func TestWSRejectsBadTicket(t *testing.T) {
	newFakeEnvd(t)
	srv, _ := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)

	for _, ticket := range []string{"", "garbage"} {
		_, resp, err := dialWS(t, srv, url.Values{"ticket": {ticket}})
		if err == nil {
			t.Fatalf("dial with ticket %q succeeded, want rejection", ticket)
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("ticket %q: status = %v, want 401", ticket, resp)
		}
	}
}

// A ticket may open exactly one WebSocket; replaying it must fail.
func TestWSTicketIsSingleUse(t *testing.T) {
	newFakeEnvd(t)
	srv, _ := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)
	_, ticket := createTicket(t, srv, "sbx-1")

	conn, _, err := dialWS(t, srv, url.Values{"ticket": {ticket}})
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer conn.Close()

	_, resp, err := dialWS(t, srv, url.Values{"ticket": {ticket}})
	if err == nil {
		t.Fatal("replayed ticket was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %v, want 401", resp)
	}
}

// End-to-end: browser input reaches envd, PTY output reaches the browser,
// resize is forwarded, and closing the terminal kills the shell.
func TestWSBridgesInputAndOutput(t *testing.T) {
	f := newFakeEnvd(t)
	f.emit = func(w *streamWriter) {
		w.start(4242)
		w.data("$ ")
		w.blockUntilDone()
	}
	srv, _ := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)
	_, ticket := createTicket(t, srv, "sbx-1")

	conn, _, err := dialWS(t, srv, url.Values{
		"ticket": {ticket}, "cols": {"100"}, "rows": {"30"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if ready := readMsg(t, conn); ready.Type != "ready" || ready.PID != 4242 {
		t.Fatalf("first message = %+v, want ready/pid 4242", ready)
	}
	out := readMsg(t, conn)
	if out.Type != "output" {
		t.Fatalf("second message type = %q, want output", out.Type)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(out.Data); string(decoded) != "$ " {
		t.Fatalf("output = %q, want %q", decoded, "$ ")
	}

	if err := conn.WriteJSON(wsMessage{
		Type: "input", Data: base64.StdEncoding.EncodeToString([]byte("whoami\n")),
	}); err != nil {
		t.Fatalf("write input: %v", err)
	}
	waitFor(t, "input to reach envd", func() bool {
		inputs, _, _, _ := f.snapshot()
		return len(inputs) == 1 && inputs[0] == "whoami\n"
	})

	if err := conn.WriteJSON(wsMessage{Type: "resize", Cols: 120, Rows: 40}); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	waitFor(t, "resize to reach envd", func() bool {
		_, resizes, _, _ := f.snapshot()
		return len(resizes) == 1 && resizes[0] == "40x120"
	})

	// A ping is answered without disturbing the PTY.
	if err := conn.WriteJSON(wsMessage{Type: "ping"}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if pong := readMsg(t, conn); pong.Type != "pong" {
		t.Fatalf("ping answer = %q, want pong", pong.Type)
	}

	// Explicit close kills the shell rather than leaking it.
	if err := conn.WriteJSON(wsMessage{Type: "close"}); err != nil {
		t.Fatalf("write close: %v", err)
	}
	waitFor(t, "SIGKILL to reach envd", func() bool {
		_, _, _, signals := f.snapshot()
		return signals == 1
	})
}

// The initial window size from the query string is what the PTY is created
// with — a mis-sized shell garbles full-screen programs.
func TestWSStartUsesRequestedSize(t *testing.T) {
	f := newFakeEnvd(t)
	var gotBody string
	var gotContentType string
	f.srv.Config.Handler = wrapCapture(f.srv.Config.Handler, &gotContentType, &gotBody)
	f.emit = func(w *streamWriter) {
		w.start(1)
		w.blockUntilDone()
	}
	srv, _ := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)
	_, ticket := createTicket(t, srv, "sbx-1")

	conn, _, err := dialWS(t, srv, url.Values{
		"ticket": {ticket}, "cols": {"133"}, "rows": {"44"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMsg(t, conn) // ready

	if !strings.Contains(gotBody, `"rows":44`) || !strings.Contains(gotBody, `"cols":133`) {
		t.Fatalf("Start payload = %s, want rows 44 / cols 133", gotBody)
	}
}

// When the shell exits the client gets an exit frame with the code, and the
// session is dropped from the registry.
func TestWSReportsPTYExit(t *testing.T) {
	f := newFakeEnvd(t)
	f.emit = func(w *streamWriter) {
		w.start(11)
		w.data("bye\n")
		w.end(0)
	}
	srv, h := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)
	_, ticket := createTicket(t, srv, "sbx-1")

	conn, _, err := dialWS(t, srv, url.Values{"ticket": {ticket}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var exit *wsMessage
	for i := 0; i < 5 && exit == nil; i++ {
		msg := readMsg(t, conn)
		if msg.Type == "exit" {
			exit = &msg
		}
	}
	if exit == nil {
		t.Fatal("never received an exit message")
	}
	if exit.ExitCode == nil || *exit.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want 0", exit.ExitCode)
	}
	waitFor(t, "session to be removed", func() bool {
		return h.registry.CountForSandbox("sbx-1") == 0
	})
}

// Two terminals on the same sandbox are independent sessions.
func TestWSMultipleSessionsPerSandbox(t *testing.T) {
	f := newFakeEnvd(t)
	pid := 0
	f.emit = func(w *streamWriter) {
		pid++
		w.start(1000 + pid)
		w.blockUntilDone()
	}
	srv, h := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)

	var conns []*websocket.Conn
	for i := 0; i < 2; i++ {
		_, ticket := createTicket(t, srv, "sbx-1")
		conn, _, err := dialWS(t, srv, url.Values{"ticket": {ticket}})
		if err != nil {
			t.Fatalf("dial #%d: %v", i, err)
		}
		defer conn.Close()
		if ready := readMsg(t, conn); ready.Type != "ready" {
			t.Fatalf("dial #%d: first message = %q, want ready", i, ready.Type)
		}
		conns = append(conns, conn)
	}
	waitFor(t, "two live sessions", func() bool {
		return h.registry.CountForSandbox("sbx-1") == 2
	})
	_ = conns
}

// A PID the registry does not know must not be attachable — otherwise any
// ticket holder could hijack an arbitrary process in the sandbox.
func TestWSReconnectRejectsUnknownPID(t *testing.T) {
	newFakeEnvd(t)
	srv, _ := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)
	_, ticket := createTicket(t, srv, "sbx-1")

	conn, _, err := dialWS(t, srv, url.Values{"ticket": {ticket}, "pid": {"9999"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := readMsg(t, conn)
	if msg.Type != "error" || !strings.Contains(msg.Message, "not found") {
		t.Fatalf("message = %+v, want an error about a missing session", msg)
	}
}

// After an abnormal socket drop the PTY survives and can be reattached via
// envd Connect using the recorded PID.
func TestWSReconnectToDetachedSession(t *testing.T) {
	f := newFakeEnvd(t)
	f.emit = func(w *streamWriter) {
		w.start(555)
		w.blockUntilDone()
	}
	srv, h := newTestServer(t, &fakeCM{status: 1}, DefaultIdleTimeout)
	_, ticket := createTicket(t, srv, "sbx-1")

	conn, _, err := dialWS(t, srv, url.Values{"ticket": {ticket}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if ready := readMsg(t, conn); ready.PID != 555 {
		t.Fatalf("pid = %d, want 555", ready.PID)
	}
	// Rip the socket away without a close frame.
	_ = conn.UnderlyingConn().Close()

	waitFor(t, "session to detach", func() bool {
		h.registry.mu.Lock()
		defer h.registry.mu.Unlock()
		for _, s := range h.registry.sessions {
			if s.PID == 555 && !s.attached {
				return true
			}
		}
		return false
	})

	_, ticket2 := createTicket(t, srv, "sbx-1")
	conn2, _, err := dialWS(t, srv, url.Values{"ticket": {ticket2}, "pid": {"555"}})
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer conn2.Close()
	if ready := readMsg(t, conn2); ready.Type != "ready" || ready.PID != 555 {
		t.Fatalf("reconnect message = %+v, want ready/pid 555", ready)
	}
}
