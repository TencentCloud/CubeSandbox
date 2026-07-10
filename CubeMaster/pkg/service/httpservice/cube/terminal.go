// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var terminalUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	// CubeAPI does not send an Origin header. Reject browser-originated direct
	// connections so terminal authentication cannot be bypassed via CubeMaster.
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
}

type terminalControl struct {
	Type    string `json:"type"`
	Cols    uint32 `json:"cols,omitempty"`
	Rows    uint32 `json:"rows,omitempty"`
	Code    uint32 `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	ExecID  string `json:"execID,omitempty"`
}

func handleTerminalAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) {
	if r.Method != http.MethodGet {
		http.Error(w, "terminal requires GET", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	sandboxID := strings.TrimSpace(query.Get("sandbox_id"))
	containerID := strings.TrimSpace(query.Get("container_id"))
	requestID := strings.TrimSpace(query.Get("request_id"))
	if sandboxID == "" || containerID == "" {
		http.Error(w, "sandbox_id and container_id are required", http.StatusBadRequest)
		return
	}
	if requestID == "" && rt != nil {
		requestID = rt.RequestID
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "terminal requires a WebSocket upgrade", http.StatusBadRequest)
		logTerminalAudit(r.Context(), requestID, sandboxID, containerID, "", "rejected: missing WebSocket upgrade")
		return
	}
	if !terminalUpgrader.CheckOrigin(r) {
		http.Error(w, "browser-originated terminal connections are not allowed", http.StatusForbidden)
		logTerminalAudit(r.Context(), requestID, sandboxID, containerID, "", "rejected: browser origin")
		return
	}
	hostIP, err := resolveTerminalHost(r.Context(), sandboxID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		logTerminalAudit(r.Context(), requestID, sandboxID, containerID, "", "rejected: "+err.Error())
		return
	}
	endpoint := cubelet.GetCubeletAddr(hostIP)
	stream, err := cubelet.OpenTerminal(r.Context(), endpoint)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to connect to cubelet: %v", err), http.StatusBadGateway)
		logTerminalAudit(r.Context(), requestID, sandboxID, containerID, hostIP, "rejected: cubelet connection failed")
		return
	}
	defer stream.Close()

	cols, rows := terminalDimensions(query.Get("cols"), query.Get("rows"))
	if err := stream.Send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Open{Open: &cubebox.TerminalOpen{
		RequestId:   requestID,
		SandboxId:   sandboxID,
		ContainerId: containerID,
		Args:        []string{"/bin/sh"},
		Cols:        cols,
		Rows:        rows,
	}}}); err != nil {
		http.Error(w, fmt.Sprintf("failed to open cubelet terminal: %v", err), http.StatusBadGateway)
		return
	}

	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	writer := &lockedWebSocketWriter{conn: conn}
	logTerminalAudit(r.Context(), requestID, sandboxID, containerID, hostIP, "connected")
	reason := relayTerminal(r.Context(), conn, writer, stream)
	logTerminalAudit(r.Context(), requestID, sandboxID, containerID, hostIP, reason)
}

func resolveTerminalHost(ctx context.Context, sandboxID string) (string, error) {
	if cache := localcache.GetSandboxCache(sandboxID); cache != nil && strings.TrimSpace(cache.HostIP) != "" {
		return cache.HostIP, nil
	}
	if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok && proxyMap != nil && strings.TrimSpace(proxyMap.HostIP) != "" {
		return proxyMap.HostIP, nil
	}
	return "", fmt.Errorf("sandbox %q host could not be resolved", sandboxID)
}

func terminalDimensions(colsValue, rowsValue string) (uint32, uint32) {
	cols64, colsErr := strconv.ParseUint(colsValue, 10, 32)
	rows64, rowsErr := strconv.ParseUint(rowsValue, 10, 32)
	if colsErr != nil || rowsErr != nil || cols64 == 0 || rows64 == 0 {
		return 80, 24
	}
	return uint32(cols64), uint32(rows64)
}

type terminalGRPCStream interface {
	Send(*cubebox.TerminalMessage) error
	Recv() (*cubebox.TerminalMessage, error)
	CloseSend() error
}

func relayTerminal(
	ctx context.Context,
	conn *websocket.Conn,
	writer *lockedWebSocketWriter,
	stream terminalGRPCStream,
) string {
	grpcDone := make(chan string, 1)
	go func() {
		grpcDone <- relayTerminalOutput(writer, stream)
		_ = conn.Close()
	}()

	wsDone := make(chan string, 1)
	go func() {
		wsDone <- relayTerminalInput(conn, writer, stream)
	}()

	select {
	case reason := <-grpcDone:
		return reason
	case reason := <-wsDone:
		_ = stream.Send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Close{Close: &cubebox.TerminalClose{}}})
		return reason
	case <-ctx.Done():
		return "closed: request context canceled"
	}
}

func relayTerminalInput(conn *websocket.Conn, writer *lockedWebSocketWriter, stream terminalGRPCStream) string {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return "closed: client disconnected"
			}
			return "closed: websocket read error"
		}
		switch messageType {
		case websocket.BinaryMessage:
			if err := stream.Send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Input{Input: payload}}); err != nil {
				return "closed: cubelet input error"
			}
		case websocket.TextMessage:
			var control terminalControl
			if err := json.Unmarshal(payload, &control); err != nil {
				_ = writer.control(terminalControl{Type: "error", Message: "invalid terminal control message"})
				continue
			}
			switch control.Type {
			case "resize":
				if control.Cols == 0 || control.Rows == 0 {
					_ = writer.control(terminalControl{Type: "error", Message: "resize requires positive cols and rows"})
					continue
				}
				if err := stream.Send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Resize{Resize: &cubebox.TerminalResize{Cols: control.Cols, Rows: control.Rows}}}); err != nil {
					return "closed: cubelet resize error"
				}
			case "close":
				return "closed: client requested close"
			case "heartbeat":
				_ = writer.control(terminalControl{Type: "heartbeat"})
			default:
				_ = writer.control(terminalControl{Type: "error", Message: "unsupported terminal control message"})
			}
		}
	}
}

func relayTerminalOutput(writer *lockedWebSocketWriter, stream terminalGRPCStream) string {
	for {
		message, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_ = writer.control(terminalControl{Type: "error", Message: err.Error()})
				return "closed: cubelet stream error"
			}
			return "closed: cubelet stream ended"
		}
		switch payload := message.Message.(type) {
		case *cubebox.TerminalMessage_Output:
			if err := writer.binary(payload.Output); err != nil {
				return "closed: websocket output error"
			}
		case *cubebox.TerminalMessage_Started:
			if err := writer.control(terminalControl{Type: "ready", ExecID: payload.Started.ExecId}); err != nil {
				return "closed: websocket ready error"
			}
		case *cubebox.TerminalMessage_Exit:
			_ = writer.control(terminalControl{Type: "exit", Code: payload.Exit.Code})
			return fmt.Sprintf("closed: process exited with code %d", payload.Exit.Code)
		case *cubebox.TerminalMessage_Error:
			_ = writer.control(terminalControl{Type: "error", Message: payload.Error.Message})
			return "closed: cubelet terminal error"
		}
	}
}

type lockedWebSocketWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *lockedWebSocketWriter) binary(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (w *lockedWebSocketWriter) control(control terminalControl) error {
	payload, err := json.Marshal(control)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteMessage(websocket.TextMessage, payload)
}

func logTerminalAudit(ctx context.Context, requestID, sandboxID, containerID, host, reason string) {
	log.G(ctx).WithFields(map[string]interface{}{
		"requestID":   requestID,
		"sandboxID":   sandboxID,
		"containerID": containerID,
		"host":        host,
		"closeReason": reason,
	}).Info("terminal access audit")
}
