// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
)

const (
	terminalTicketTTL      = 30 * time.Second
	terminalOpenTimeout    = 15 * time.Second
	terminalIdleTimeout    = 30 * time.Minute
	terminalPingInterval   = 20 * time.Second
	terminalControlTimeout = 5 * time.Second
	terminalMaxClientFrame = 256 << 10
	terminalMaxSessions    = 4
	terminalProtocol       = "cube-terminal"
	terminalTicketPrefix   = "cube-ticket."
	terminalInitialRows    = 24
	terminalInitialCols    = 80
)

var terminalSandboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// TerminalHandler turns a browser WebSocket into a bounded envd PTY session.
// Long-lived access JWTs never enter the WebSocket URL: the authenticated
// ticket endpoint issues a short-lived, one-use, sandbox-bound ticket.
type TerminalHandler struct {
	cm      CubeMasterClient
	jm      *auth.JWTManager
	backend service.TerminalBackend
	upgrade websocket.Upgrader
	limits  *terminalSessionLimiter
	replay  *terminalReplayGuard

	ticketTTL    time.Duration
	openTimeout  time.Duration
	idleTimeout  time.Duration
	pingInterval time.Duration
}

// NewTerminalHandler creates the production Web Terminal handler.
func NewTerminalHandler(cm CubeMasterClient, jm *auth.JWTManager, backend service.TerminalBackend) *TerminalHandler {
	return &TerminalHandler{
		cm:      cm,
		jm:      jm,
		backend: backend,
		upgrade: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   32 << 10,
			WriteBufferSize:  64 << 10,
			Subprotocols:     []string{terminalProtocol},
			CheckOrigin:      terminalSameOrigin,
		},
		limits:       newTerminalSessionLimiter(terminalMaxSessions),
		replay:       newTerminalReplayGuard(),
		ticketTTL:    terminalTicketTTL,
		openTimeout:  terminalOpenTimeout,
		idleTimeout:  terminalIdleTimeout,
		pingInterval: terminalPingInterval,
	}
}

// RegisterAuthed installs the access-token-protected ticket endpoint.
func (h *TerminalHandler) RegisterAuthed(r *gin.RouterGroup) {
	r.POST("/sandboxes/:id/terminal/ticket", h.CreateTicket)
}

// RegisterPublic installs the WebSocket upgrade endpoint. It performs its own
// short-lived ticket authentication because browsers cannot set an
// Authorization header in the WebSocket constructor.
func (h *TerminalHandler) RegisterPublic(r *gin.RouterGroup) {
	r.GET("/sandboxes/:id/terminal/ws", h.Connect)
}

// CreateTicket validates the sandbox before issuing a one-use ticket.
func (h *TerminalHandler) CreateTicket(c *gin.Context) {
	sandboxID := c.Param("id")
	if !terminalSandboxIDPattern.MatchString(sandboxID) {
		httputil.WriteError(c, http.StatusBadRequest, "invalid sandbox ID")
		return
	}
	if err := h.requireRunningSandbox(c.Request.Context(), sandboxID); err != nil {
		h.writeSandboxError(c, err)
		return
	}
	username := c.GetString("username")
	if username == "" {
		username = auth.UsernameFromContext(c.Request.Context())
	}
	ticket, err := h.jm.GenerateTerminalTicket(username, sandboxID, h.ticketTTL)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to create terminal ticket")
		return
	}
	h.replay.prune(time.Now())
	httputil.WriteJSON(c, http.StatusCreated, gin.H{
		"ticket":    ticket,
		"expiresIn": int(h.ticketTTL.Seconds()),
		"protocol":  terminalProtocol,
	})
}

// Connect authenticates, upgrades, then serves one interactive PTY session.
func (h *TerminalHandler) Connect(c *gin.Context) {
	sandboxID := c.Param("id")
	if !terminalSandboxIDPattern.MatchString(sandboxID) {
		httputil.WriteError(c, http.StatusBadRequest, "invalid sandbox ID")
		return
	}
	ticket := terminalTicketFromProtocols(c.GetHeader("Sec-WebSocket-Protocol"))
	if ticket == "" {
		httputil.WriteError(c, http.StatusUnauthorized, "missing terminal ticket")
		return
	}
	claims, err := h.jm.VerifyTerminalTicket(ticket)
	if err != nil || claims.SandboxID != sandboxID {
		httputil.WriteError(c, http.StatusUnauthorized, "invalid or expired terminal ticket")
		return
	}
	if claims.ID == "" || !h.replay.use(claims.ID, claims.ExpiresAt.Time) {
		httputil.WriteError(c, http.StatusUnauthorized, "terminal ticket was already used")
		return
	}
	if err := h.requireRunningSandbox(c.Request.Context(), sandboxID); err != nil {
		h.writeSandboxError(c, err)
		return
	}
	release, ok := h.limits.acquire(claims.Username, sandboxID)
	if !ok {
		httputil.WriteError(c, http.StatusTooManyRequests, "too many active terminal sessions")
		return
	}
	defer release()

	conn, err := h.upgrade.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(terminalMaxClientFrame)
	sessionCtx, cancelSession := context.WithCancel(c.Request.Context())
	defer cancelSession()

	started := time.Now()
	reason := "client disconnected"
	slog.Info("terminal session opening", "username", claims.Username, "sandbox_id", sandboxID)
	type openResult struct {
		session service.TerminalSession
		err     error
	}
	opened := make(chan openResult)
	go func() {
		session, err := h.backend.Open(sessionCtx, sandboxID, service.TerminalSize{Rows: terminalInitialRows, Cols: terminalInitialCols})
		select {
		case opened <- openResult{session: session, err: err}:
		case <-sessionCtx.Done():
			if session != nil {
				_ = session.Close()
			}
		}
	}()
	openTimeout := h.openTimeout
	if openTimeout <= 0 {
		openTimeout = terminalOpenTimeout
	}
	openTimer := time.NewTimer(openTimeout)
	defer openTimer.Stop()
	var session service.TerminalSession
	select {
	case result := <-opened:
		session, err = result.session, result.err
	case <-openTimer.C:
		reason = "backend open timed out"
		cancelSession()
		_ = conn.WriteJSON(terminalServerMessage{Type: "error", Message: "timed out opening sandbox terminal"})
		slog.Warn("terminal session opening timed out", "username", claims.Username, "sandbox_id", sandboxID)
		return
	}
	if err != nil {
		reason = "backend open failed"
		_ = conn.WriteJSON(terminalServerMessage{Type: "error", Message: "failed to open sandbox terminal"})
		slog.Warn("terminal session failed", "username", claims.Username, "sandbox_id", sandboxID, "error", err)
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), terminalControlTimeout)
		defer cancel()
		if err := session.Kill(ctx); err != nil {
			slog.Warn("terminal cleanup kill failed", "username", claims.Username, "sandbox_id", sandboxID, "pid", session.PID(), "error", err)
		}
		_ = session.Close()
		slog.Info("terminal session closed", "username", claims.Username, "sandbox_id", sandboxID, "pid", session.PID(), "duration_ms", time.Since(started).Milliseconds(), "reason", reason)
	}()

	if err := conn.WriteJSON(terminalServerMessage{Type: "ready", PID: session.PID()}); err != nil {
		reason = "ready write failed"
		return
	}

	actions := make(chan terminalClientAction, 32)
	readDone := make(chan error, 1)
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	go h.readClient(sessionCtx, conn, actions, readDone, &lastActivity)

	pingEvery := h.pingInterval
	if pingEvery <= 0 {
		pingEvery = terminalPingInterval
	}
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	idleCheck := time.NewTicker(time.Minute)
	defer idleCheck.Stop()

	output := session.Output()
	done := session.Done()
	for {
		select {
		case chunk, open := <-output:
			if !open {
				output = nil
				continue
			}
			lastActivity.Store(time.Now().UnixNano())
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				reason = "output write failed"
				return
			}
		case exit, open := <-done:
			if !open {
				done = nil
				if output == nil {
					reason = "terminal stream ended"
					return
				}
				continue
			}
			msg := terminalServerMessage{Type: "exit", ExitCode: exit.ExitCode}
			if exit.Error != nil {
				msg.Message = exit.Error.Error()
			}
			_ = conn.WriteJSON(msg)
			reason = "terminal exited"
			return
		case action := <-actions:
			if action.Err != nil {
				_ = conn.WriteJSON(terminalServerMessage{Type: "error", Message: action.Err.Error()})
				continue
			}
			if action.Close {
				reason = "client requested close"
				return
			}
			ctx, cancel := context.WithTimeout(sessionCtx, terminalControlTimeout)
			if len(action.Input) > 0 {
				err = session.SendInput(ctx, action.Input)
			} else if action.Resize != nil {
				err = session.Resize(ctx, *action.Resize)
			}
			cancel()
			if err != nil {
				_ = conn.WriteJSON(terminalServerMessage{Type: "error", Message: "terminal control request failed"})
				slog.Warn("terminal control failed", "username", claims.Username, "sandbox_id", sandboxID, "pid", session.PID(), "error", err)
			}
		case err := <-readDone:
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				reason = "websocket read failed"
			}
			return
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				reason = "ping failed"
				return
			}
		case <-idleCheck.C:
			if h.idleTimeout > 0 && time.Since(time.Unix(0, lastActivity.Load())) > h.idleTimeout {
				_ = conn.WriteJSON(terminalServerMessage{Type: "error", Message: "terminal session timed out due to inactivity"})
				reason = "idle timeout"
				return
			}
		case <-c.Request.Context().Done():
			reason = "request cancelled"
			return
		}
	}
}

func (h *TerminalHandler) readClient(ctx context.Context, conn *websocket.Conn, actions chan<- terminalClientAction, done chan<- error, lastActivity *atomic.Int64) {
	sendAction := func(action terminalClientAction) bool {
		select {
		case actions <- action:
			return true
		case <-ctx.Done():
			return false
		}
	}
	conn.SetPongHandler(func(string) error {
		lastActivity.Store(time.Now().UnixNano())
		return nil
	})
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			select {
			case done <- err:
			case <-ctx.Done():
			}
			return
		}
		lastActivity.Store(time.Now().UnixNano())
		switch messageType {
		case websocket.BinaryMessage:
			if !sendAction(terminalClientAction{Input: append([]byte(nil), payload...)}) {
				return
			}
		case websocket.TextMessage:
			var msg terminalClientMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				if !sendAction(terminalClientAction{Err: errors.New("invalid terminal control message")}) {
					return
				}
				continue
			}
			switch msg.Type {
			case "resize":
				size := service.NormalizeTerminalSize(service.TerminalSize{Rows: msg.Rows, Cols: msg.Cols})
				if !sendAction(terminalClientAction{Resize: &size}) {
					return
				}
			case "close":
				sendAction(terminalClientAction{Close: true})
				return
			case "ping":
				// Reading the message is enough to refresh the idle timestamp.
			default:
				if !sendAction(terminalClientAction{Err: errors.New("unsupported terminal control message")}) {
					return
				}
			}
		}
	}
}

func (h *TerminalHandler) requireRunningSandbox(ctx context.Context, sandboxID string) error {
	raw, err := h.cm.GetSandbox(ctx, sandboxID, sdkInstanceType)
	if err != nil {
		return err
	}
	var env translator.CMEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode sandbox response: %w", err)
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		return fmt.Errorf("cubemaster returned %d: %s", env.Ret.RetCode, env.Ret.RetMsg)
	}
	var items []translator.CMSandboxDetailItem
	if err := json.Unmarshal(env.Data, &items); err != nil || len(items) == 0 {
		return errTerminalSandboxNotFound
	}
	if items[0].SandboxID != "" && items[0].SandboxID != sandboxID {
		return errTerminalSandboxNotFound
	}
	if items[0].Status != 1 {
		return errTerminalSandboxNotRunning
	}
	return nil
}

var (
	errTerminalSandboxNotFound   = errors.New("sandbox not found")
	errTerminalSandboxNotRunning = errors.New("sandbox must be running to open a terminal")
)

func (h *TerminalHandler) writeSandboxError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errTerminalSandboxNotFound):
		httputil.WriteError(c, http.StatusNotFound, err.Error())
	case errors.Is(err, errTerminalSandboxNotRunning):
		httputil.WriteError(c, http.StatusConflict, err.Error())
	default:
		writeCMError(c, err)
	}
}

func terminalSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func terminalTicketFromProtocols(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, terminalTicketPrefix) {
			return strings.TrimPrefix(part, terminalTicketPrefix)
		}
	}
	return ""
}

type terminalClientMessage struct {
	Type string `json:"type"`
	Rows int    `json:"rows,omitempty"`
	Cols int    `json:"cols,omitempty"`
}

type terminalServerMessage struct {
	Type     string `json:"type"`
	PID      int    `json:"pid,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Message  string `json:"message,omitempty"`
}

type terminalClientAction struct {
	Input  []byte
	Resize *service.TerminalSize
	Close  bool
	Err    error
}

type terminalSessionLimiter struct {
	mu        sync.Mutex
	max       int
	byUser    map[string]int
	bySandbox map[string]int
}

func newTerminalSessionLimiter(max int) *terminalSessionLimiter {
	return &terminalSessionLimiter{max: max, byUser: make(map[string]int), bySandbox: make(map[string]int)}
}

func (l *terminalSessionLimiter) acquire(username, sandboxID string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser[username] >= l.max || l.bySandbox[sandboxID] >= l.max {
		return nil, false
	}
	l.byUser[username]++
	l.bySandbox[sandboxID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.byUser[username]--
			l.bySandbox[sandboxID]--
			if l.byUser[username] == 0 {
				delete(l.byUser, username)
			}
			if l.bySandbox[sandboxID] == 0 {
				delete(l.bySandbox, sandboxID)
			}
		})
	}, true
}

type terminalReplayGuard struct {
	mu   sync.Mutex
	used map[string]time.Time
}

func newTerminalReplayGuard() *terminalReplayGuard {
	return &terminalReplayGuard{used: make(map[string]time.Time)}
}

func (g *terminalReplayGuard) use(id string, expires time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.pruneLocked(now)
	if _, exists := g.used[id]; exists {
		return false
	}
	g.used[id] = expires
	return true
}

func (g *terminalReplayGuard) prune(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(now)
}

func (g *terminalReplayGuard) pruneLocked(now time.Time) {
	for id, expires := range g.used {
		if !expires.After(now) {
			delete(g.used, id)
		}
	}
}
