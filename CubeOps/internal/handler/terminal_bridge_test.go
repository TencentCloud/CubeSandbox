// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func runningSandboxResponse() json.RawMessage {
	return json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sandbox-1","status":1,"containers":[{"container_id":"sandbox-1","type":"sandbox"},{"container_id":"sidecar-1","type":"sidecar"}]}]}`)
}

func newTerminalSessionRouter(t *testing.T, response json.RawMessage) *gin.Engine {
	t.Helper()
	cm := &fakeCM{getSandbox: func(_ context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
		if sandboxID != "sandbox-1" || instanceType != sdkInstanceType {
			t.Fatalf("GetSandbox(%q, %q)", sandboxID, instanceType)
		}
		return response, nil
	}}
	handler := NewSDKHandler(cm).WithTerminalGateway("http://cube-master:8089", "gateway-secret", "")
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("username", "alice")
		c.Next()
	})
	handler.Register(router.Group("/api/v1/sdk"))
	return router
}

func TestCreateTerminalSessionIssuesLocalGrant(t *testing.T) {
	router := newTerminalSessionRouter(t, runningSandboxResponse())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sdk/sandboxes/sandbox-1/terminal-sessions", strings.NewReader(`{"cols":80,"rows":24,"containerId":"sidecar-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		SessionID string `json:"sessionId"`
		Grant     string `json:"grant"`
		Expires   int    `json:"expiresInSeconds"`
		Protocol  string `json:"protocol"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SessionID == "" || response.Grant == "" || response.Expires != 60 || response.Protocol != terminalProtocol {
		t.Fatalf("response = %#v", response)
	}
	cookie := recorder.Header().Get("Set-Cookie")
	for _, required := range []string{"cube_terminal_", "HttpOnly", "SameSite=Strict", "Secure"} {
		if !strings.Contains(cookie, required) {
			t.Errorf("Set-Cookie %q missing %q", cookie, required)
		}
	}
}

func TestCreateTerminalSessionRejectsUnknownContainer(t *testing.T) {
	router := newTerminalSessionRouter(t, runningSandboxResponse())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sdk/sandboxes/sandbox-1/terminal-sessions", strings.NewReader(`{"cols":80,"rows":24,"containerId":"other"}`))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateTerminalSessionRejectsStoppedSandbox(t *testing.T) {
	router := newTerminalSessionRouter(t, json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sandbox-1","status":5,"containers":[{"container_id":"sandbox-1"}]}]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sdk/sandboxes/sandbox-1/terminal-sessions", strings.NewReader(`{"cols":80,"rows":24}`))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateTerminalSessionRejectsOversizedBody(t *testing.T) {
	router := newTerminalSessionRouter(t, runningSandboxResponse())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sdk/sandboxes/sandbox-1/terminal-sessions", strings.NewReader(strings.Repeat("x", maxTerminalSessionRequestBytes+1)))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestTerminalBackendURLTargetsCubeMaster(t *testing.T) {
	got, err := terminalBackendURL("https://cube-master.example/base/")
	if err != nil {
		t.Fatal(err)
	}
	if want := "wss://cube-master.example/base/cube/sandbox/terminal"; got != want {
		t.Fatalf("terminalBackendURL = %q, want %q", got, want)
	}
}

func TestTerminalGatewayRelaysOpenAndBinaryFrames(t *testing.T) {
	backendDone := make(chan error, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(terminalGatewayTokenHeader); got != "gateway-secret" {
			backendDone <- &testTerminalError{"gateway token", got}
			return
		}
		connection, err := terminalUpgrader.Upgrade(w, r, nil)
		if err != nil {
			backendDone <- err
			return
		}
		defer connection.Close()
		var open map[string]any
		if err := connection.ReadJSON(&open); err != nil {
			backendDone <- err
			return
		}
		if open["type"] != "open" || open["sandboxId"] != "sandbox-1" || open["containerId"] != "container-1" {
			backendDone <- &testTerminalError{"open frame", open}
			return
		}
		if err := connection.WriteJSON(map[string]any{"v": 1, "type": "ready", "sessionId": "session-1"}); err != nil {
			backendDone <- err
			return
		}
		messageType, data, err := connection.ReadMessage()
		if err == nil {
			err = connection.WriteMessage(messageType, data)
		}
		backendDone <- err
	}))
	defer backend.Close()

	gateway := newTerminalGateway(backend.URL, "gateway-secret", "")
	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := terminalUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		lease := &terminalSessionLease{principal: "alice", sessionID: "session-1", target: terminalTarget{sandboxID: "sandbox-1", containerID: "container-1", cols: 80, rows: 24}}
		_ = gateway.relay(r, connection, lease)
	}))
	defer frontend.Close()

	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{terminalProtocol}
	client, _, err := dialer.Dial("ws"+strings.TrimPrefix(frontend.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, ready, err := client.ReadMessage()
	if err != nil || !strings.Contains(string(ready), `"type":"ready"`) {
		t.Fatalf("ready = %q, err=%v", ready, err)
	}
	payload := []byte{0, 1, 2, 255}
	if err := client.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	messageType, echoed, err := client.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || string(echoed) != string(payload) {
		t.Fatalf("echo type=%d data=%v err=%v", messageType, echoed, err)
	}
	if err := <-backendDone; err != nil {
		t.Fatal(err)
	}
}

type testTerminalError struct {
	field string
	value any
}

func (err *testTerminalError) Error() string {
	return err.field + " mismatch"
}
