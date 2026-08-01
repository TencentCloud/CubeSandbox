// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
)

// SandboxInfoClient is the subset of the CubeMaster client the terminal needs
// to validate a sandbox before opening a PTY into it.
type SandboxInfoClient interface {
	GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error)
}

// wsMessage is the JSON frame exchanged over the terminal WebSocket.
//
//	client -> server: input (base64) | resize | ping | close
//	server -> client: ready (pid) | output (base64) | exit | error | pong
type wsMessage struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	Cols     int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	PID      int    `json:"pid,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Message  string `json:"message,omitempty"`
}

// Handler exposes the terminal HTTP endpoints.
type Handler struct {
	cm       SandboxInfoClient
	tickets  *TicketManager
	registry *Registry
	client   *Client
	upgrader websocket.Upgrader
}

// NewHandler wires the terminal endpoints. jwtSecret must be the same secret
// the login JWTs use; domain is the sandbox domain cube-proxy routes on.
func NewHandler(cm SandboxInfoClient, jwtSecret, domain string, idleTimeout time.Duration) *Handler {
	client := NewClient(domain)
	registry := NewRegistry(client, idleTimeout)
	registry.Start()
	return &Handler{
		cm:       cm,
		tickets:  NewTicketManager(jwtSecret, DefaultTicketTTL),
		registry: registry,
		client:   client,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// The handshake is authenticated by a one-time 30s ticket that a
			// cross-site page cannot obtain (issuing it requires the JWT in an
			// Authorization header), so an Origin allowlist adds nothing and
			// would break deployments served under arbitrary hosts/ports.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// Registry exposes the session registry (used by tests).
func (h *Handler) Registry() *Registry { return h.registry }

// RegisterAuthed mounts the ticket endpoint on the authenticated SDK group
// (POST /api/v1/sdk/sandboxes/:id/terminal).
func (h *Handler) RegisterAuthed(r *gin.RouterGroup) {
	r.POST("/sandboxes/:id/terminal", h.CreateTicket)
}

// RegisterPublic mounts the WebSocket endpoint on the public group
// (GET /api/v1/terminal/ws). The ticket in the query string is the auth.
func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	r.GET("/terminal/ws", h.ServeWS)
}

// CreateTicket validates that the sandbox exists and is running, then issues
// a one-time WebSocket ticket for it.
func (h *Handler) CreateTicket(c *gin.Context) {
	sandboxID := c.Param("id")
	username := auth.UsernameFromContext(c.Request.Context())

	raw, err := h.cm.GetSandbox(c.Request.Context(), sandboxID, "cubebox")
	if err != nil {
		// A deleted sandbox is a 404 to the caller, not a gateway failure —
		// the WebUI distinguishes the two when deciding whether to retry.
		var cmErr *cubemaster.CMError
		if errors.As(err, &cmErr) && cmErr.IsNotFound() {
			httputil.WriteError(c, http.StatusNotFound, "sandbox not found")
			return
		}
		httputil.WriteError(c, http.StatusBadGateway, "failed to query sandbox: "+err.Error())
		return
	}
	detail, ok := translator.TransformSandboxDetail(raw).(map[string]interface{})
	if !ok || detail == nil {
		httputil.WriteError(c, http.StatusNotFound, "sandbox not found")
		return
	}
	if state, _ := detail["state"].(string); state != "running" {
		httputil.WriteError(c, http.StatusConflict, "sandbox is not running (state: "+state+")")
		return
	}

	ticket, err := h.tickets.Issue(username, sandboxID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to issue terminal ticket")
		return
	}
	// wsPath is relative to the ops API base the frontend already talks to
	// (/opsapi/v1 in production, proxied to /api/v1).
	httputil.WriteJSON(c, http.StatusOK, gin.H{
		"ticket": ticket,
		"wsPath": "/terminal/ws",
	})
}

// ServeWS upgrades to a WebSocket and bridges it to a sandbox PTY. Query
// params: ticket (required), cols/rows (initial size), pid (reconnect to an
// existing detached session instead of starting a new shell).
func (h *Handler) ServeWS(c *gin.Context) {
	claims, err := h.tickets.Redeem(c.Query("ticket"))
	if err != nil {
		httputil.WriteError(c, http.StatusUnauthorized, err.Error())
		return
	}

	cols := queryInt(c, "cols", 80)
	rows := queryInt(c, "rows", 24)
	reconnectPID := queryInt(c, "pid", 0)

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error response.
		slog.Warn("terminal ws upgrade failed", "err", err, "sandboxID", claims.SandboxID)
		return
	}
	h.bridge(conn, claims, c.ClientIP(), rows, cols, reconnectPID)
}

// wsWriter serializes writes to a websocket connection (gorilla allows only
// one concurrent writer).
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) send(msg wsMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.conn.WriteJSON(msg)
}

func (w *wsWriter) close(reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = w.conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason))
	_ = w.conn.Close()
}

// bridge runs one terminal session over an upgraded WebSocket until the PTY
// exits, the client leaves, or the session is reaped.
func (h *Handler) bridge(conn *websocket.Conn, claims *TicketClaims, clientIP string, rows, cols, reconnectPID int) {
	w := &wsWriter{conn: conn}
	// The stream context outlives the request handler on purpose; it is
	// cancelled when the bridge finishes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		stream *Stream
		sess   *Session
		err    error
	)
	if reconnectPID > 0 {
		// Reconnect: the PID must belong to a detached session of this
		// sandbox — a bare PID from the client is not trusted.
		sess = h.registry.Reattach(claims.SandboxID, reconnectPID, w.close)
		if sess == nil {
			_ = w.send(wsMessage{Type: "error", Message: "terminal session not found or already attached"})
			w.close("no session")
			return
		}
		stream, err = h.client.ConnectPTY(ctx, claims.SandboxID, reconnectPID)
		if err != nil {
			h.registry.Remove(sess.ID)
			auditEnd(sess, "reconnect_failed")
			_ = w.send(wsMessage{Type: "error", Message: "failed to reconnect terminal: " + err.Error()})
			w.close("reconnect failed")
			return
		}
	} else {
		stream, err = h.client.StartPTY(ctx, claims.SandboxID, rows, cols)
		if err != nil {
			_ = w.send(wsMessage{Type: "error", Message: "failed to start terminal: " + err.Error()})
			w.close("start failed")
			return
		}
		sess = &Session{
			ID:        uuid.New().String(),
			SandboxID: claims.SandboxID,
			Username:  claims.Username,
			ClientIP:  clientIP,
			PID:       stream.PID,
			CreatedAt: time.Now(),
		}
		h.registry.Add(sess, w.close)
	}

	auditStart(sess, reconnectPID > 0)
	_ = w.send(wsMessage{Type: "ready", PID: stream.PID})

	// PTY output -> WebSocket.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		for chunk := range stream.Output {
			if w.send(wsMessage{Type: "output", Data: base64.StdEncoding.EncodeToString(chunk)}) != nil {
				return
			}
		}
	}()

	// WebSocket input -> PTY, until the socket dies or the client leaves.
	// reason records why the bridge ended, for the audit trail.
	reason := h.readClientLoop(ctx, conn, w, sess, stream)

	cancel() // tear down the envd output stream
	<-outDone

	switch reason {
	case "client_close":
		// The user closed the terminal: kill the shell.
		killCtx, killCancel := context.WithTimeout(context.Background(), unaryTimeout)
		_ = h.client.Kill(killCtx, sess.SandboxID, sess.PID)
		killCancel()
		h.registry.Remove(sess.ID)
		auditEnd(sess, reason)
		w.close("bye")
	case "detached":
		// Abnormal socket loss: keep the PTY for reconnect; the idle sweeper
		// will reap it if the user never comes back.
		h.registry.Detach(sess.ID)
		auditEnd(sess, reason)
	default: // pty_exit / stream_error / idle_timeout (socket closed by sweeper)
		h.registry.Remove(sess.ID)
		auditEnd(sess, reason)
		w.close("bye")
	}
}

// readClientLoop pumps client frames into the PTY. It returns when the
// session ends, with a reason string: "pty_exit", "stream_error",
// "client_close" (explicit close frame or clean WS close) or "detached"
// (abnormal socket loss with the PTY still alive).
func (h *Handler) readClientLoop(ctx context.Context, conn *websocket.Conn, w *wsWriter, sess *Session, stream *Stream) string {
	// A goroutine watches for PTY exit so the read loop can be unblocked by
	// closing the connection from the side.
	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		select {
		case <-stream.Done():
			exited, code, streamErr := stream.Result()
			if exited {
				_ = w.send(wsMessage{Type: "exit", ExitCode: &code})
			} else if streamErr != nil {
				_ = w.send(wsMessage{Type: "error", Message: streamErr.Error()})
			}
			// Unblock ReadMessage below.
			_ = conn.Close()
		case <-readDone:
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// Distinguish why the read ended, in priority order.
			select {
			case <-stream.Done():
				if exited, _, _ := stream.Result(); exited {
					return "pty_exit"
				}
				return "stream_error"
			default:
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return "client_close"
			}
			return "detached"
		}

		var msg wsMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "input":
			raw, decErr := base64.StdEncoding.DecodeString(msg.Data)
			if decErr != nil {
				continue
			}
			h.registry.Touch(sess.ID)
			if sendErr := h.client.SendInput(ctx, sess.SandboxID, sess.PID, raw); sendErr != nil {
				_ = w.send(wsMessage{Type: "error", Message: "failed to send input: " + sendErr.Error()})
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				h.registry.Touch(sess.ID)
				_ = h.client.Resize(ctx, sess.SandboxID, sess.PID, msg.Rows, msg.Cols)
			}
		case "ping":
			// Keepalive only — deliberately does not touch the idle timer,
			// so an untouched terminal still idles out.
			_ = w.send(wsMessage{Type: "pong"})
		case "close":
			return "client_close"
		}
	}
}

func queryInt(c *gin.Context, key string, def int) int {
	if v := c.Query(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// ── audit ───────────────────────────────────────────────────────────────────

// Audit lines go through the standard slog pipeline (cubelog rolling files).
// requestLogger() does not record the username, so these lines carry the full
// identity themselves.

func auditStart(s *Session, reconnect bool) {
	slog.Info("terminal_session_start",
		"sessionID", s.ID,
		"sandboxID", s.SandboxID,
		"username", s.Username,
		"clientIP", s.ClientIP,
		"pid", s.PID,
		"reconnect", reconnect,
	)
}

func auditEnd(s *Session, reason string) {
	slog.Info("terminal_session_end",
		"sessionID", s.ID,
		"sandboxID", s.SandboxID,
		"username", s.Username,
		"clientIP", s.ClientIP,
		"pid", s.PID,
		"reason", reason,
		"durationMs", time.Since(s.CreatedAt).Milliseconds(),
	)
}
