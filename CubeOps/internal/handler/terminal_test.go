// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

type terminalFakeCM struct {
	status          int
	containerStatus int
	includeSidecar  bool
}

func (f terminalFakeCM) GetSandbox(context.Context, string, string) (json.RawMessage, error) {
	containers := []interface{}{map[string]interface{}{
		"name":         "main",
		"container_id": "sandbox-1",
		"status":       f.containerStatus,
		"type":         "sandbox",
		"envd_port":    49983,
	}}
	if f.includeSidecar {
		containers = append(containers, map[string]interface{}{
			"name":         "worker",
			"container_id": "container-2",
			"status":       1,
			"type":         "container",
			"envd_port":    49984,
		})
	}
	payload := map[string]interface{}{
		"ret": map[string]interface{}{"ret_code": 0, "ret_msg": "ok"},
		"data": []interface{}{map[string]interface{}{
			"sandbox_id": "sandbox-1",
			"status":     f.status,
			"containers": containers,
		}},
	}
	raw, _ := json.Marshal(payload)
	return raw, nil
}

func connectFrame(payload string) []byte {
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func terminalTestEnvd(t *testing.T) (*httptest.Server, <-chan [2]int) {
	t.Helper()
	resizes := make(chan [2]int, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/process.Process/Start":
			w.Header().Set("Content-Type", "application/connect+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(connectFrame(`{"event":{"start":{"pid":321}}}`))
			output := base64.StdEncoding.EncodeToString([]byte("\x1b[32mready\x1b[0m\r\n"))
			_, _ = w.Write(connectFrame(`{"event":{"data":{"pty":"` + output + `"}}}`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/process.Process/Update":
			var request struct {
				PTY struct {
					Size struct {
						Rows int `json:"rows"`
						Cols int `json:"cols"`
					} `json:"size"`
				} `json:"pty"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			resizes <- [2]int{request.PTY.Size.Rows, request.PTY.Size.Cols}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	return server, resizes
}

func terminalTestRouter(t *testing.T, cm terminalFakeCM, envdURL string, idle time.Duration) (*gin.Engine, *service.TerminalService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	terminals, err := service.NewTerminalService(envdURL, "cube.test", time.Minute, time.Second, 8, 4)
	if err != nil {
		t.Fatalf("NewTerminalService: %v", err)
	}
	handler := NewTerminalHandler(cm, terminals, idle)
	router := gin.New()
	public := router.Group("/api/v1/sdk")
	handler.RegisterPublic(public)
	authed := router.Group("/api/v1/sdk", func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer valid" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Set("username", "alice")
		c.Next()
	})
	handler.RegisterAuthed(authed)
	return router, terminals
}

func requestTerminalTicket(t *testing.T, baseURL string, auth bool) (int, map[string]interface{}) {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/api/v1/sdk/sandboxes/sandbox-1/terminal/tickets",
		bytes.NewBufferString(`{"containerID":"sandbox-1","rows":24,"cols":80}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if auth {
		request.Header.Set("Authorization", "Bearer valid")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]interface{}
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response.StatusCode, body
}

func TestTerminalTicketRequiresAuthenticationAndRunningTarget(t *testing.T) {
	envd, _ := terminalTestEnvd(t)
	defer envd.Close()

	router, terminals := terminalTestRouter(t, terminalFakeCM{status: 1, containerStatus: 1}, envd.URL, time.Minute)
	defer terminals.Close()
	server := httptest.NewServer(router)
	defer server.Close()

	if status, _ := requestTerminalTicket(t, server.URL, false); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ticket status = %d", status)
	}
	if status, body := requestTerminalTicket(t, server.URL, true); status != http.StatusCreated || body["ticket"] == "" {
		t.Fatalf("authenticated ticket status/body = %d %#v", status, body)
	}

	stoppedRouter, stoppedTerminals := terminalTestRouter(t, terminalFakeCM{status: 5, containerStatus: 5}, envd.URL, time.Minute)
	defer stoppedTerminals.Close()
	stoppedServer := httptest.NewServer(stoppedRouter)
	defer stoppedServer.Close()
	if status, _ := requestTerminalTicket(t, stoppedServer.URL, true); status != http.StatusConflict {
		t.Fatalf("stopped sandbox ticket status = %d", status)
	}
}

func TestTerminalTargetSelectsSidecarEndpoint(t *testing.T) {
	handler := &TerminalHandler{
		cm: terminalFakeCM{status: 1, containerStatus: 1, includeSidecar: true},
	}
	target, err := handler.validateTarget(context.Background(), "sandbox-1", "container-2")
	if err != nil {
		t.Fatalf("validateTarget: %v", err)
	}
	if target.ContainerID != "container-2" || target.Name != "worker" || target.EnvdPort != 49984 {
		t.Fatalf("sidecar target = %#v", target)
	}
}

func TestTerminalWebSocketEstablishResizeAndDisconnect(t *testing.T) {
	envd, resizes := terminalTestEnvd(t)
	defer envd.Close()
	router, terminals := terminalTestRouter(t, terminalFakeCM{status: 1, containerStatus: 1}, envd.URL, time.Minute)
	defer terminals.Close()
	server := httptest.NewServer(router)
	defer server.Close()

	status, ticketBody := requestTerminalTicket(t, server.URL, true)
	if status != http.StatusCreated {
		t.Fatalf("ticket status = %d", status)
	}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/api/v1/sdk/sandboxes/sandbox-1/terminal/ws?ticket=" + ticketBody["ticket"].(string)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close()

	messageType, payload, err := conn.ReadMessage()
	if err != nil || messageType != websocket.TextMessage || !bytes.Contains(payload, []byte(`"ready"`)) {
		t.Fatalf("ready message = type %d payload %s err %v", messageType, payload, err)
	}
	messageType, payload, err = conn.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || !bytes.Contains(payload, []byte("ready")) {
		t.Fatalf("PTY output = type %d payload %q err %v", messageType, payload, err)
	}

	if err := conn.WriteJSON(map[string]interface{}{"type": "resize", "rows": 40, "cols": 120}); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-resizes:
		if size != [2]int{40, 120} {
			t.Fatalf("resize = %v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("resize was not forwarded to envd")
	}

	if err := conn.WriteJSON(map[string]interface{}{"type": "disconnect"}); err != nil {
		t.Fatal(err)
	}
	_, payload, err = conn.ReadMessage()
	if err != nil || !bytes.Contains(payload, []byte(`"client_disconnect"`)) {
		t.Fatalf("disconnect response = %s err %v", payload, err)
	}
}

func TestTerminalWebSocketClosesIdleSession(t *testing.T) {
	envd, _ := terminalTestEnvd(t)
	defer envd.Close()
	router, terminals := terminalTestRouter(
		t,
		terminalFakeCM{status: 1, containerStatus: 1},
		envd.URL,
		30*time.Millisecond,
	)
	defer terminals.Close()
	server := httptest.NewServer(router)
	defer server.Close()

	status, ticketBody := requestTerminalTicket(t, server.URL, true)
	if status != http.StatusCreated {
		t.Fatalf("ticket status = %d", status)
	}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/api/v1/sdk/sandboxes/sandbox-1/terminal/ws?ticket=" + ticketBody["ticket"].(string)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))

	// Consume the ready control frame and initial PTY output. With no further
	// input or output, the server must actively terminate the session.
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read PTY output: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil || messageType != websocket.TextMessage ||
		!bytes.Contains(payload, []byte(`"idle_timeout"`)) {
		t.Fatalf("idle response = type %d payload %s err %v", messageType, payload, err)
	}
}

func TestTerminalInputLimiterBoundsEventsAndBytes(t *testing.T) {
	now := time.Now()
	var events terminalInputLimiter
	for i := 0; i < terminalRateEvents; i++ {
		if !events.allow(1, now) {
			t.Fatalf("event %d was rejected before the configured limit", i)
		}
	}
	if events.allow(1, now) {
		t.Fatal("event rate limit was not enforced")
	}
	if !events.allow(1, now.Add(terminalRateWindow)) {
		t.Fatal("event rate limit did not reset after the window")
	}

	var bytes terminalInputLimiter
	if !bytes.allow(terminalRateBytes, now) {
		t.Fatal("byte limit rejected an input exactly at the limit")
	}
	if bytes.allow(1, now) {
		t.Fatal("byte rate limit was not enforced")
	}
}
