// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	api "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
)

type fakeTerminalGRPCStream struct {
	mu       sync.Mutex
	received []*api.TerminalMessage
	sent     []*api.TerminalMessage
}

func (f *fakeTerminalGRPCStream) Send(message *api.TerminalMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, message)
	return nil
}

func (f *fakeTerminalGRPCStream) Recv() (*api.TerminalMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		return nil, io.EOF
	}
	message := f.received[0]
	f.received = f.received[1:]
	return message, nil
}

func (*fakeTerminalGRPCStream) CloseSend() error { return nil }

func TestTerminalRouteRejectsInvalidRequest(t *testing.T) {
	for name, test := range map[string]struct {
		method string
		target string
		status int
	}{
		"wrong method":      {http.MethodPost, "/cube/sandbox/terminal", http.StatusMethodNotAllowed},
		"missing sandbox":   {http.MethodGet, "/cube/sandbox/terminal?container_id=c", http.StatusBadRequest},
		"missing container": {http.MethodGet, "/cube/sandbox/terminal?sandbox_id=s", http.StatusBadRequest},
		"missing upgrade":   {http.MethodGet, "/cube/sandbox/terminal?sandbox_id=s&container_id=c", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.target, nil)
			handleTerminalAction(recorder, request, nil)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
		})
	}
}

func TestTerminalWebSocketRejectsDirectBrowserOrigin(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://master/cube/sandbox/terminal", nil)
	request.Header.Set("Origin", "https://dashboard.example.com")
	if terminalUpgrader.CheckOrigin(request) {
		t.Fatal("browser-originated CubeMaster terminal connection should be rejected")
	}
	request.Header.Del("Origin")
	if !terminalUpgrader.CheckOrigin(request) {
		t.Fatal("internal CubeAPI relay without Origin should be accepted")
	}

	recorder := httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodGet,
		"http://master/cube/sandbox/terminal?sandbox_id=sandbox&container_id=container",
		nil,
	)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Origin", "https://dashboard.example.com")
	handleTerminalAction(recorder, request, nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("browser-originated route status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestTerminalWebSocketRelaysInputResizeAndOutput(t *testing.T) {
	stream := &fakeTerminalGRPCStream{received: []*api.TerminalMessage{
		{Message: &api.TerminalMessage_Started{Started: &api.TerminalStarted{ExecId: "exec-1"}}},
		{Message: &api.TerminalMessage_Output{Output: []byte("ready\n")}},
		{Message: &api.TerminalMessage_Exit{Exit: &api.TerminalExit{Code: 7}}},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := terminalUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		writer := &lockedWebSocketWriter{conn: conn}
		inputDone := make(chan string, 1)
		go func() { inputDone <- relayTerminalInput(conn, writer, stream) }()
		if reason := relayTerminalOutput(writer, stream); reason != "closed: process exited with code 7" {
			t.Errorf("unexpected output reason %q", reason)
		}
		<-inputDone
	}))
	defer server.Close()

	url := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(terminalControl{Type: "resize", Cols: 120, Rows: 40}); err != nil {
		t.Fatal(err)
	}

	messageType, payload, err := conn.ReadMessage()
	if err != nil || messageType != websocket.TextMessage || string(payload) != `{"type":"ready","execID":"exec-1"}` {
		t.Fatalf("ready frame type=%d payload=%s err=%v", messageType, payload, err)
	}
	messageType, payload, err = conn.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || string(payload) != "ready\n" {
		t.Fatalf("output frame type=%d payload=%q err=%v", messageType, payload, err)
	}
	_, _, _ = conn.ReadMessage()
	_ = conn.Close()

	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.sent) < 2 || string(stream.sent[0].GetInput()) != "ls\n" {
		t.Fatalf("input was not relayed: %+v", stream.sent)
	}
	resize := stream.sent[1].GetResize()
	if resize == nil || resize.Cols != 120 || resize.Rows != 40 {
		t.Fatalf("resize was not relayed: %+v", stream.sent)
	}
}
