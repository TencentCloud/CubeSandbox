// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

// Interactive sandbox terminal, served entirely by CubeOps.
//
// The browser (xterm.js) opens an authenticated WebSocket to CubeOps; CubeOps
// bridges it to the sandbox's in-guest envd PTY API (service.OpenPTY). There is
// no new CubeMaster/Cubelet RPC and no CubeAPI involvement — the terminal is an
// operational capability living in the ops backend.
//
// Frame contract (browser ⇄ CubeOps):
//   - client → server: JSON text frames {type:"open"|"input"|"resize", ...};
//     a lone "K" text frame is a keepalive.
//   - server → client: raw PTY output as BINARY frames (no lossy JSON/UTF-8
//     re-encoding); control events {type:"error"|"exit", ...} as JSON text.
//
// Auth: browsers cannot set request headers on a WebSocket upgrade, so the
// CubeOps JWT is carried as the second Sec-WebSocket-Protocol token (the first
// is the fixed "cube-terminal" marker) and verified here. Because the JWT lives
// in the victim origin's storage and is unreachable cross-origin, this also
// defeats cross-site WebSocket hijacking — the token is the authorization
// boundary, so Origin is not enforced.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

const (
	terminalSubprotocol  = "cube-terminal"
	terminalMaxFrame     = 64 * 1024
	terminalIdleTimeout  = 30 * time.Minute
	terminalWriteTimeout = 10 * time.Second
	// terminalPingInterval must stay well below terminalIdleTimeout so several
	// pings are attempted before a healthy-but-quiet session could time out.
	terminalPingInterval    = 30 * time.Second
	terminalMaxCols         = 512
	terminalMaxRows         = 256
	terminalMaxPerSandbox   = 4
	terminalOpenErrorText   = "unable to open terminal"
	terminalKeepaliveMarker = "K"
)

// TerminalHandler serves the sandbox terminal WebSocket.
type TerminalHandler struct {
	jm       *auth.JWTManager
	domain   string
	upgrader websocket.Upgrader
	sessions *terminalSessionLimiter
}

// NewTerminalHandler builds a terminal handler. domain is the sandbox DNS
// suffix used to address envd (cfg.SandboxDomain).
func NewTerminalHandler(jm *auth.JWTManager, domain string) *TerminalHandler {
	return &TerminalHandler{
		jm:     jm,
		domain: domain,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			Subprotocols:    []string{terminalSubprotocol},
			// Auth is the JWT carried in the subprotocol, which is unreachable
			// cross-origin; see the file header. Origin is therefore not a
			// second boundary.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		sessions: newTerminalSessionLimiter(terminalMaxPerSandbox),
	}
}

// Register mounts the terminal route. It is intentionally NOT placed behind the
// bearer-header JWT middleware: a browser WebSocket cannot send an Authorization
// header, so Handle authenticates the token from the subprotocol instead.
func (th *TerminalHandler) Register(r gin.IRoutes) {
	r.GET("/sdk/sandboxes/:id/terminal/ws", th.Handle)
}

type terminalClientFrame struct {
	Type      string `json:"type"`
	SandboxID string `json:"sandboxId,omitempty"`
	Data      string `json:"data,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

// Handle upgrades the request and bridges it to an envd PTY.
func (th *TerminalHandler) Handle(c *gin.Context) {
	sandboxID := c.Param("id")

	token := subprotocolToken(c.Request.Header.Values("Sec-WebSocket-Protocol"))
	operator := ""
	if token != "" {
		if claims, err := th.jm.VerifyAccessToken(token); err == nil {
			operator = claims.Username
		}
	}
	if operator == "" {
		c.String(http.StatusUnauthorized, "terminal session token is required")
		return
	}

	if !th.sessions.acquire(sandboxID) {
		c.String(http.StatusTooManyRequests, "terminal session limit reached for sandbox")
		return
	}
	defer th.sessions.release(sandboxID)

	conn, err := th.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // upgrader already wrote the error response
	}
	defer conn.Close()

	conn.SetReadLimit(terminalMaxFrame)
	_ = conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
	})

	var writeMu sync.Mutex
	writeJSON := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
		_ = conn.WriteJSON(v)
	}
	writeError := func(msg string) {
		writeJSON(map[string]any{"type": "error", "message": msg})
	}

	// First frame must open the session and name the same sandbox as the URL.
	// The check is unconditional: an omitted sandboxId used to skip it, which
	// left the frame's agreement with the path optional and made the contract
	// ambiguous to audit. Only the URL parameter is ever passed to OpenPTY, so
	// the frame is a cross-check rather than a source of truth.
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var open terminalClientFrame
	if json.Unmarshal(raw, &open) != nil || open.Type != "open" {
		writeError("first terminal frame must open the session")
		return
	}
	if !terminalOpenMatchesPath(open, sandboxID) {
		writeError("terminal sandbox does not match request path")
		return
	}

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	pty, err := service.OpenPTY(ctx, sandboxID, th.domain, service.PtySize{
		Cols: clampDim(open.Cols, terminalMaxCols),
		Rows: clampDim(open.Rows, terminalMaxRows),
	}, "", terminalIdleTimeout)
	if err != nil {
		slog.Warn("terminal open failed", "sandboxId", sandboxID, "operator", operator, "error", err)
		writeError(terminalOpenErrorText)
		return
	}
	slog.Info("audit", "event", "terminal.session.open", "operator", operator, "sandboxId", sandboxID, "pid", pty.PID())
	defer func() {
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = pty.Kill(killCtx)
		killCancel()
		pty.Close()
		slog.Info("audit", "event", "terminal.session.close", "operator", operator, "sandboxId", sandboxID)
	}()

	// Keepalive pings. SetPongHandler above extends the read deadline, but
	// nothing was driving it: a silently dropped TCP connection (laptop lid,
	// NAT timeout) would hold one of the terminalMaxPerSandbox slots until the
	// 30-minute idle timeout expired. Pinging makes the write fail promptly,
	// which tears the session down and frees the slot.
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(terminalPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
				perr := conn.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
				if perr != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Output pump: raw PTY bytes → browser as binary frames; on stream end,
	// emit an exit control frame and close.
	go func() {
		for chunk := range pty.Output() {
			writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
			werr := conn.WriteMessage(websocket.BinaryMessage, chunk)
			writeMu.Unlock()
			if werr != nil {
				cancel()
				return
			}
		}
		code, hasCode, errMsg := pty.ExitInfo()
		if errMsg != "" {
			writeError(errMsg)
		}
		exit := map[string]any{"type": "exit"}
		if hasCode {
			exit["exitCode"] = code
		}
		writeJSON(exit)
		writeMu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "terminal exited"))
		writeMu.Unlock()
		cancel()
	}()

	// Read pump: browser input / resize / keepalive → envd.
	for {
		mt, data, rerr := conn.ReadMessage()
		if rerr != nil {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
		if mt == websocket.TextMessage && string(data) == terminalKeepaliveMarker {
			continue
		}
		var frame terminalClientFrame
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "input":
			if frame.Data != "" {
				if serr := pty.SendStdin(ctx, []byte(frame.Data)); serr != nil {
					cancel()
				}
			}
		case "resize":
			cols, rows := clampDim(frame.Cols, terminalMaxCols), clampDim(frame.Rows, terminalMaxRows)
			if cols > 0 && rows > 0 {
				_ = pty.Resize(ctx, service.PtySize{Cols: cols, Rows: rows})
			}
		}
	}

	cancel()
	<-pty.Done()
}

// terminalOpenMatchesPath reports whether an open frame agrees with the sandbox
// named in the request path. The frame must state the id explicitly: treating an
// omitted value as consent made the cross-check optional, so a client could skip
// it entirely. Only the path parameter is ever passed to OpenPTY, so this is
// defence in depth rather than the sole authorization boundary.
func terminalOpenMatchesPath(open terminalClientFrame, sandboxID string) bool {
	return sandboxID != "" && open.SandboxID == sandboxID
}

// clampDim caps a browser-supplied dimension at max. A non-positive value is
// left as 0 so callers can apply their own default.
func clampDim(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// subprotocolToken returns the JWT carried alongside the fixed "cube-terminal"
// marker in Sec-WebSocket-Protocol. Values may arrive as multiple headers or a
// single comma-joined header.
func subprotocolToken(values []string) string {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			candidate := strings.TrimSpace(part)
			if candidate != "" && candidate != terminalSubprotocol {
				return candidate
			}
		}
	}
	return ""
}

// terminalSessionLimiter caps concurrent terminal sessions per sandbox.
type terminalSessionLimiter struct {
	max int
	mu  sync.Mutex
	n   map[string]int
}

func newTerminalSessionLimiter(max int) *terminalSessionLimiter {
	return &terminalSessionLimiter{max: max, n: make(map[string]int)}
}

func (l *terminalSessionLimiter) acquire(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[id] >= l.max {
		return false
	}
	l.n[id]++
	return true
}

func (l *terminalSessionLimiter) release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[id] <= 1 {
		delete(l.n, id)
		return
	}
	l.n[id]--
}
