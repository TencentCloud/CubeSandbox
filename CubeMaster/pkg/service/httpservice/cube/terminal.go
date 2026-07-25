// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	terminalGatewayTokenHeader    = "X-Cube-Terminal-Token"
	terminalGatewayTokenEnv       = "CUBE_TERMINAL_GATEWAY_TOKEN"
	terminalWebSocketFrameLimit   = 256 * 1024
	terminalOpenTimeout           = 10 * time.Second
	terminalWebSocketWriteTimeout = 15 * time.Second
	maxTerminalOpenPayload        = 64 * 1024
	maxTerminalDimension          = 1000
)

var terminalUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	// Only CubeOps may connect to this internal endpoint. Browsers cannot add
	// the shared-token header, and server-to-server requests send no Origin.
	CheckOrigin: terminalGatewayAllowed,
}

func terminalGatewayAllowed(r *http.Request) bool {
	if r == nil || r.Header.Get("Origin") != "" {
		return false
	}
	expected := os.Getenv(terminalGatewayTokenEnv)
	provided := r.Header.Get(terminalGatewayTokenHeader)
	if expected == "" {
		return false
	}
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1 &&
		subtle.ConstantTimeEq(int32(len(expected)), int32(len(provided))) == 1
}

type terminalOpenControl struct {
	Type        string   `json:"type"`
	RequestID   string   `json:"requestID"`
	SandboxID   string   `json:"sandboxID"`
	ContainerID string   `json:"containerID"`
	Args        []string `json:"args,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	Env         []string `json:"env,omitempty"`
	Cols        uint32   `json:"cols"`
	Rows        uint32   `json:"rows"`
}

type terminalControl struct {
	Type    string `json:"type"`
	Cols    uint32 `json:"cols,omitempty"`
	Rows    uint32 `json:"rows,omitempty"`
	Code    uint32 `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	ExecID  string `json:"execID,omitempty"`
}

func handleTerminalGinAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	handleTerminalAction(c.Writer, c.Request, rt)
}

func handleTerminalAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) {
	if r.Method != http.MethodGet {
		http.Error(w, "terminal requires GET", http.StatusMethodNotAllowed)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "terminal requires a WebSocket upgrade", http.StatusBadRequest)
		return
	}

	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(terminalWebSocketFrameLimit)
	writer := &lockedWebSocketWriter{conn: conn}

	open, err := readTerminalOpen(conn)
	if err != nil {
		_ = writer.control(terminalControl{Type: "error", Message: err.Error()})
		logTerminalAudit(r.Context(), "", "", "", "", "rejected: "+err.Error())
		return
	}
	if open.RequestID == "" && rt != nil {
		open.RequestID = rt.RequestID
	}

	hostIP, err := resolveTerminalHost(r.Context(), open.SandboxID)
	if err != nil {
		_ = writer.control(terminalControl{Type: "error", Message: err.Error()})
		logTerminalAudit(r.Context(), open.RequestID, open.SandboxID, open.ContainerID, "", "rejected: "+err.Error())
		return
	}
	endpoint := cubelet.GetCubeletAddr(hostIP)
	stream, err := cubelet.OpenTerminal(r.Context(), endpoint)
	if err != nil {
		_ = writer.control(terminalControl{Type: "error", Message: fmt.Sprintf("failed to connect to cubelet: %v", err)})
		logTerminalAudit(r.Context(), open.RequestID, open.SandboxID, open.ContainerID, hostIP, "rejected: cubelet connection failed")
		return
	}
	defer stream.Close()

	if err := stream.Send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Open{Open: &cubebox.TerminalOpen{
		RequestId:   open.RequestID,
		SandboxId:   open.SandboxID,
		ContainerId: open.ContainerID,
		Args:        open.Args,
		Cwd:         open.Cwd,
		Env:         open.Env,
		Cols:        open.Cols,
		Rows:        open.Rows,
	}}}); err != nil {
		_ = writer.control(terminalControl{Type: "error", Message: fmt.Sprintf("failed to open cubelet terminal: %v", err)})
		return
	}

	logTerminalAudit(r.Context(), open.RequestID, open.SandboxID, open.ContainerID, hostIP, "connected")
	reason := relayTerminal(r.Context(), conn, writer, stream)
	logTerminalAudit(r.Context(), open.RequestID, open.SandboxID, open.ContainerID, hostIP, reason)
}

func readTerminalOpen(conn *websocket.Conn) (*terminalOpenControl, error) {
	_ = conn.SetReadDeadline(time.Now().Add(terminalOpenTimeout))
	messageType, payload, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("terminal open message is required: %w", err)
	}
	if messageType != websocket.TextMessage {
		return nil, errors.New("the first terminal message must be text open control")
	}
	if len(payload) > maxTerminalOpenPayload {
		return nil, fmt.Errorf("terminal open message exceeds %d bytes", maxTerminalOpenPayload)
	}
	var open terminalOpenControl
	if err := json.Unmarshal(payload, &open); err != nil {
		return nil, fmt.Errorf("invalid terminal open message: %w", err)
	}
	if err := normalizeAndValidateTerminalOpenControl(&open); err != nil {
		return nil, err
	}
	return &open, nil
}

func normalizeAndValidateTerminalOpenControl(open *terminalOpenControl) error {
	if open == nil {
		return errors.New("terminal open message is required")
	}
	if open.Type != "open" {
		return errors.New("the first terminal message must have type open")
	}
	open.SandboxID = strings.TrimSpace(open.SandboxID)
	open.ContainerID = strings.TrimSpace(open.ContainerID)
	if open.SandboxID == "" || open.ContainerID == "" {
		return errors.New("sandboxID and containerID are required")
	}
	if open.Cols == 0 || open.Rows == 0 || open.Cols > maxTerminalDimension || open.Rows > maxTerminalDimension {
		return fmt.Errorf("terminal dimensions must be between 1 and %d", maxTerminalDimension)
	}
	if open.Cwd != "" && (!strings.HasPrefix(open.Cwd, "/") || strings.ContainsRune(open.Cwd, '\x00')) {
		return errors.New("terminal cwd must be an absolute path without NUL characters")
	}
	if len(open.Args) > 0 && strings.TrimSpace(open.Args[0]) == "" {
		return errors.New("terminal args must start with a command")
	}
	for _, env := range open.Env {
		key, _, ok := strings.Cut(env, "=")
		if !ok || key == "" || strings.ContainsRune(env, '\x00') {
			return errors.New("terminal environment entries must use non-empty KEY=VALUE form")
		}
	}
	return nil
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
	var closeOnce sync.Once
	closeWebSocket := func() {
		closeOnce.Do(func() {
			_ = conn.Close()
		})
	}
	grpcDone := make(chan string, 1)
	go func() {
		grpcDone <- relayTerminalOutput(writer, stream)
		closeWebSocket()
	}()

	wsDone := make(chan string, 1)
	go func() { wsDone <- relayTerminalInput(conn, writer, stream) }()

	select {
	case reason := <-grpcDone:
		closeWebSocket()
		return reason
	case reason := <-wsDone:
		_ = stream.Send(&cubebox.TerminalMessage{Message: &cubebox.TerminalMessage_Close{Close: &cubebox.TerminalClose{}}})
		closeWebSocket()
		return reason
	case <-ctx.Done():
		closeWebSocket()
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
				if control.Cols == 0 || control.Rows == 0 || control.Cols > maxTerminalDimension || control.Rows > maxTerminalDimension {
					_ = writer.control(terminalControl{Type: "error", Message: "invalid terminal dimensions"})
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
	_ = w.conn.SetWriteDeadline(time.Now().Add(terminalWebSocketWriteTimeout))
	return w.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (w *lockedWebSocketWriter) control(control terminalControl) error {
	payload, err := json.Marshal(control)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(terminalWebSocketWriteTimeout))
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
