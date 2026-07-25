// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTerminalGatewayAuthorization(t *testing.T) {
	request := httptest.NewRequest("GET", "/cube/sandbox/terminal/ws", nil)
	if terminalGatewayAuthorizedWithToken(request, "") {
		t.Fatal("empty configured token must disable the terminal endpoint")
	}

	request.Header.Set("X-Cube-Terminal-Gateway", "wrong")
	if terminalGatewayAuthorizedWithToken(request, "expected") {
		t.Fatal("mismatched gateway token must be rejected")
	}

	request.Header.Set("X-Cube-Terminal-Gateway", "expected")
	if !terminalGatewayAuthorizedWithToken(request, "expected") {
		t.Fatal("matching gateway token must be accepted")
	}
}

func TestReadTerminalFrameTreatsNormalCloseAsEOF(t *testing.T) {
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		var frame terminalClientFrame
		result <- readTerminalFrame(conn, &frame)
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("close error = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for close frame")
	}
}
