// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
)

const (
	terminalTicketTTL       = 30 * time.Second
	terminalFrameLimit      = 256 * 1024
	terminalWriteTimeout    = 15 * time.Second
	terminalControlTimeout  = 10 * time.Second
	terminalIdleTimeout     = 30 * time.Minute
	terminalPingInterval    = 30 * time.Second
	terminalTicketIssuer    = "cubeops"
	terminalTicketAudience  = "cubeops-terminal"
	terminalWSProtocol      = "cube-terminal-v1"
	terminalDefaultRows     = 24
	terminalDefaultCols     = 80
	terminalMaximumEnvdRows = 200
	terminalMaximumEnvdCols = 400
)

const (
	terminalCloseBrowserDisconnected = "browser disconnected"
	terminalCloseBackendDisconnected = "terminal backend disconnected"
	terminalCloseIdleTimeout         = "idle timeout"
	terminalCloseProcessExited       = "terminal process exited"
	terminalCloseBackendError        = "terminal backend error"
	terminalCloseRelayFailure        = "terminal relay failure"
)

type terminalTicketRequest struct {
	ContainerID string `json:"containerID"`
	Rows        uint32 `json:"rows"`
	Cols        uint32 `json:"cols"`
}

type terminalTicketResponse struct {
	Ticket       string `json:"ticket"`
	ExpiresAt    string `json:"expiresAt"`
	WebSocketURL string `json:"websocketUrl"`
	ContainerID  string `json:"containerID,omitempty"`
}

type terminalTicketClaims struct {
	SandboxID   string `json:"sandbox_id"`
	ContainerID string `json:"container_id"`
	CreatedBy   string `json:"created_by"`
	Rows        uint32 `json:"rows"`
	Cols        uint32 `json:"cols"`
	jwt.RegisteredClaims
}

// TerminalGateway owns browser authentication and talks directly to envd's
// existing Process PTY API. Tickets are signed capabilities, so any CubeOps
// replica can accept a WebSocket without sticky routing or shared process
// memory.
type TerminalGateway struct {
	cm             CubeMasterClient
	ticketSecret   []byte
	sandboxDomain  string
	upgrader       websocket.Upgrader
	envdHTTP       *http.Client
	proxyBase      string
	ticketLimiter  *terminalTicketLimiter
	sessionLimiter *terminalSessionLimiter
}

func NewTerminalGateway(cm CubeMasterClient, ticketSecret, sandboxDomain string) *TerminalGateway {
	return &TerminalGateway{
		cm:             cm,
		ticketSecret:   []byte(ticketSecret),
		sandboxDomain:  strings.TrimSpace(sandboxDomain),
		proxyBase:      service.EnvdProxyURL(),
		ticketLimiter:  newTerminalTicketLimiter(terminalTicketLimitPerUser, terminalTicketLimitWindow),
		sessionLimiter: newTerminalSessionLimiter(terminalSessionLimitPerReplica, terminalSessionLimitPerUser, terminalSessionLimitPerSandbox),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin:     terminalOriginAllowed,
		},
	}
}

func (h *TerminalGateway) RegisterPublic(r *gin.RouterGroup) {
	r.GET("/terminal/sandboxes/:id/ws", h.OpenWebSocket)
}

func (h *TerminalGateway) RegisterAuthed(r *gin.RouterGroup) {
	r.POST("/terminal/sandboxes/:id/tickets", h.CreateTicket)
}

func (h *TerminalGateway) CreateTicket(c *gin.Context) {
	if len(h.ticketSecret) == 0 || h.sandboxDomain == "" {
		httputil.WriteError(c, http.StatusServiceUnavailable, "web terminal is not configured")
		return
	}

	sandboxID := strings.TrimSpace(c.Param("id"))
	if sandboxID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "sandbox id is required")
		return
	}
	username := strings.TrimSpace(c.GetString("username"))
	if username == "" {
		httputil.WriteError(c, http.StatusUnauthorized, "terminal user is not authenticated")
		return
	}
	if !h.ticketLimiter.allow(username) {
		c.Header("Retry-After", "60")
		httputil.WriteError(c, http.StatusTooManyRequests, "terminal ticket request rate limit exceeded")
		return
	}

	var body terminalTicketRequest
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		httputil.WriteError(c, http.StatusBadRequest, "invalid terminal ticket request")
		return
	}
	rows, cols, err := terminalDimensions(body.Rows, body.Cols)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}

	raw, err := h.cm.GetSandbox(c.Request.Context(), sandboxID, sdkInstanceType)
	if err != nil {
		writeCMError(c, err)
		return
	}
	detail, err := decodeTerminalSandboxDetail(raw)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, err.Error())
		return
	}
	if detail.Status != 1 {
		httputil.WriteError(c, http.StatusConflict, fmt.Sprintf("sandbox %s must be running before opening a terminal", sandboxID))
		return
	}
	containerID, err := selectTerminalContainer(detail, body.ContainerID)
	if err != nil {
		var statusErr *terminalStatusError
		if errors.As(err, &statusErr) {
			httputil.WriteError(c, statusErr.Status, statusErr.Error())
		} else {
			httputil.WriteError(c, http.StatusBadRequest, err.Error())
		}
		return
	}

	now := time.Now()
	expiresAt := now.Add(terminalTicketTTL)
	claims := terminalTicketClaims{
		SandboxID:   sandboxID,
		ContainerID: containerID,
		CreatedBy:   username,
		Rows:        rows,
		Cols:        cols,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    terminalTicketIssuer,
			Audience:  jwt.ClaimStrings{terminalTicketAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token, err := signTerminalTicket(h.ticketSecret, claims)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to issue terminal ticket")
		return
	}

	slog.Info("terminal ticket issued",
		"sandbox_id", sandboxID,
		"container_id", containerID,
		"username", claims.CreatedBy,
		"ticket_id", claims.ID,
	)
	httputil.WriteJSON(c, http.StatusCreated, terminalTicketResponse{
		Ticket:       token,
		ExpiresAt:    expiresAt.UTC().Format(time.RFC3339),
		WebSocketURL: fmt.Sprintf("/opsapi/v1/terminal/sandboxes/%s/ws", url.PathEscape(sandboxID)),
		ContainerID:  containerID,
	})
}

func (h *TerminalGateway) OpenWebSocket(c *gin.Context) {
	if len(h.ticketSecret) == 0 || h.sandboxDomain == "" {
		httputil.WriteError(c, http.StatusServiceUnavailable, "web terminal is not configured")
		return
	}
	rawTicket, err := terminalTicketFromProtocols(c.Request)
	if err != nil {
		httputil.WriteError(c, http.StatusUnauthorized, "terminal ticket is invalid or expired")
		return
	}
	claims, err := parseTerminalTicket(h.ticketSecret, rawTicket)
	if err != nil || claims.SandboxID != c.Param("id") {
		httputil.WriteError(c, http.StatusUnauthorized, "terminal ticket is invalid or expired")
		return
	}
	releaseSession, ok := h.sessionLimiter.acquire(claims.CreatedBy, claims.SandboxID)
	if !ok {
		httputil.WriteError(c, http.StatusTooManyRequests, "terminal session limit reached")
		return
	}
	defer releaseSession()

	responseHeader := http.Header{}
	responseHeader.Set("Sec-WebSocket-Protocol", terminalWSProtocol)
	browser, err := h.upgrader.Upgrade(c.Writer, c.Request, responseHeader)
	if err != nil {
		return
	}
	defer browser.Close()
	browser.SetReadLimit(terminalFrameLimit)

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	pty := service.NewEnvdPTYClient(h.envdHTTP, h.proxyBase, claims.SandboxID, h.sandboxDomain)
	stream, err := pty.Start(ctx, service.EnvdPTYStartOptions{
		Rows: claims.Rows,
		Cols: claims.Cols,
	})
	if err != nil {
		_ = writeTerminalJSON(browser, map[string]interface{}{"type": "error", "message": "failed to start terminal session"})
		slog.Warn("terminal envd start failed", "sandbox_id", claims.SandboxID, "error", err)
		return
	}
	defer stream.Close()

	pid, pending, err := awaitEnvdPTYStart(ctx, stream)
	if err != nil {
		_ = writeTerminalJSON(browser, map[string]interface{}{"type": "error", "message": "terminal backend closed before ready"})
		slog.Warn("terminal envd stream did not start", "sandbox_id", claims.SandboxID, "error", err)
		return
	}

	sessionID := uuid.NewString()
	browserWriter := &lockedTerminalWriter{conn: browser}
	if err := browserWriter.control(map[string]interface{}{
		"type":      "start",
		"execId":    fmt.Sprintf("envd-%d", pid),
		"sessionId": sessionID,
	}); err != nil {
		return
	}
	for _, event := range pending {
		if len(event.Output) > 0 {
			if err := writeEnvdPTYOutput(browserWriter, event.Output); err != nil {
				return
			}
		}
	}

	slog.Info("terminal session opened",
		"sandbox_id", claims.SandboxID,
		"container_id", claims.ContainerID,
		"username", claims.CreatedBy,
		"session_id", sessionID,
		"pid", pid,
	)
	openedAt := time.Now()

	browser.SetPingHandler(func(data string) error { return browserWriter.pong([]byte(data)) })
	_ = browser.SetReadDeadline(time.Now().Add(2 * terminalPingInterval))
	browser.SetPongHandler(func(string) error {
		return browser.SetReadDeadline(time.Now().Add(2 * terminalPingInterval))
	})
	done := make(chan string, 2)
	activity := make(chan struct{}, 1)
	go func() {
		done <- runCubeOpsTerminalRelay("browser-to-envd", func() string {
			return relayTerminalBrowserToEnvd(ctx, browser, browserWriter, pty, pid, activity)
		})
	}()
	go func() {
		done <- runCubeOpsTerminalRelay("envd-to-browser", func() string {
			return relayTerminalEnvdToBrowser(ctx, stream, browserWriter, activity)
		})
	}()

	idleTimer := time.NewTimer(terminalIdleTimeout)
	defer idleTimer.Stop()
	pingTicker := time.NewTicker(terminalPingInterval)
	defer pingTicker.Stop()
	reason := terminalCloseRelayFailure
	completedRelays := 0
	selecting := true
	for selecting {
		select {
		case reason = <-done:
			completedRelays = 1
			selecting = false
		case <-activity:
			resetTerminalTimer(idleTimer)
		case <-idleTimer.C:
			reason = terminalCloseIdleTimeout
			_ = browserWriter.control(map[string]interface{}{
				"type":               "idleTimeout",
				"idleTimeoutSeconds": int(terminalIdleTimeout.Seconds()),
			})
			selecting = false
		case <-pingTicker.C:
			if err := browserWriter.ping(); err != nil {
				reason = terminalCloseBrowserDisconnected
				selecting = false
			}
		}
	}

	cancel()
	_ = stream.Close()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), terminalControlTimeout)
	_ = pty.Kill(cleanupCtx, pid)
	cleanupCancel()
	_ = browserWriter.close()
	_ = browser.Close()
	waitForTerminalRelays(done, completedRelays)

	slog.Info("terminal session closed",
		"sandbox_id", claims.SandboxID,
		"container_id", claims.ContainerID,
		"username", claims.CreatedBy,
		"session_id", sessionID,
		"pid", pid,
		"reason", reason,
		"duration_ms", time.Since(openedAt).Milliseconds(),
	)
}

func terminalTicketFromProtocols(r *http.Request) (string, error) {
	protocols := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
	if len(protocols) != 2 || strings.TrimSpace(protocols[0]) != terminalWSProtocol {
		return "", errors.New("terminal websocket protocol is invalid")
	}
	ticket := strings.TrimSpace(protocols[1])
	if ticket == "" {
		return "", errors.New("terminal ticket is missing")
	}
	return ticket, nil
}

func waitForTerminalRelays(done <-chan string, completed int) {
	timer := time.NewTimer(terminalControlTimeout)
	defer timer.Stop()
	for completed < 2 {
		select {
		case <-done:
			completed++
		case <-timer.C:
			slog.Warn("terminal relay shutdown timed out", "remaining", 2-completed)
			return
		}
	}
}

func signTerminalTicket(secret []byte, claims terminalTicketClaims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

func parseTerminalTicket(secret []byte, raw string) (*terminalTicketClaims, error) {
	if len(secret) == 0 || strings.TrimSpace(raw) == "" {
		return nil, errors.New("terminal ticket is missing")
	}
	claims := &terminalTicketClaims{}
	token, err := jwt.ParseWithClaims(
		raw,
		claims,
		func(token *jwt.Token) (interface{}, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("unexpected terminal ticket signing method %q", token.Method.Alg())
			}
			return secret, nil
		},
		jwt.WithAudience(terminalTicketAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(terminalTicketIssuer),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !token.Valid {
		return nil, errors.New("terminal ticket is invalid")
	}
	if claims.SandboxID == "" || claims.ContainerID == "" || claims.CreatedBy == "" || claims.ID == "" {
		return nil, errors.New("terminal ticket is incomplete")
	}
	return claims, nil
}

func awaitEnvdPTYStart(ctx context.Context, stream io.ReadCloser) (int, []service.EnvdPTYEvent, error) {
	type result struct {
		pid     int
		pending []service.EnvdPTYEvent
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		pending := make([]service.EnvdPTYEvent, 0, 2)
		for {
			event, err := service.ReadEnvdPTYEvent(stream)
			if err != nil {
				resultCh <- result{err: err}
				return
			}
			if event.Started {
				resultCh <- result{pid: event.PID, pending: pending}
				return
			}
			if event.StreamEnd || event.Exited {
				resultCh <- result{err: errors.New("envd PTY stream ended before start")}
				return
			}
			pending = append(pending, event)
		}
	}()

	timer := time.NewTimer(terminalControlTimeout)
	defer timer.Stop()
	select {
	case started := <-resultCh:
		return started.pid, started.pending, started.err
	case <-ctx.Done():
		_ = stream.Close()
		return 0, nil, ctx.Err()
	case <-timer.C:
		_ = stream.Close()
		return 0, nil, errors.New("timed out waiting for envd PTY start")
	}
}

func runCubeOpsTerminalRelay(direction string, relay func() string) (reason string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("terminal relay panic", "direction", direction, "panic", recovered)
			reason = terminalCloseRelayFailure
		}
	}()
	return relay()
}

type terminalBrowserMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Rows uint32 `json:"rows"`
	Cols uint32 `json:"cols"`
}

func relayTerminalBrowserToEnvd(
	ctx context.Context,
	browser *websocket.Conn,
	writer *lockedTerminalWriter,
	pty *service.EnvdPTYClient,
	pid int,
	activity chan<- struct{},
) string {
	for {
		messageType, payload, err := browser.ReadMessage()
		if err != nil {
			return terminalCloseBrowserDisconnected
		}
		terminalActivity(activity)
		if messageType == websocket.BinaryMessage {
			if err := pty.SendInput(ctx, pid, payload); err != nil {
				return terminalCloseBackendError
			}
			continue
		}
		if messageType != websocket.TextMessage {
			continue
		}

		var message terminalBrowserMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			_ = writer.control(map[string]interface{}{"type": "error", "message": "invalid terminal message"})
			continue
		}
		switch message.Type {
		case "input", "stdin":
			if err := pty.SendInput(ctx, pid, []byte(message.Data)); err != nil {
				return terminalCloseBackendError
			}
		case "inputBase64", "stdinBase64":
			data, err := base64.StdEncoding.DecodeString(message.Data)
			if err != nil {
				_ = writer.control(map[string]interface{}{"type": "error", "message": "invalid terminal input base64"})
				continue
			}
			if err := pty.SendInput(ctx, pid, data); err != nil {
				return terminalCloseBackendError
			}
		case "resize":
			rows, cols, err := terminalDimensions(message.Rows, message.Cols)
			if err != nil {
				_ = writer.control(map[string]interface{}{"type": "error", "message": err.Error()})
				continue
			}
			if err := pty.Resize(ctx, pid, rows, cols); err != nil {
				return terminalCloseBackendError
			}
		case "kill", "close":
			return "browser requested close"
		case "ping":
			_ = writer.control(map[string]interface{}{"type": "heartbeat"})
		default:
			_ = writer.control(map[string]interface{}{"type": "error", "message": "unsupported terminal message"})
		}
	}
}

func relayTerminalEnvdToBrowser(
	ctx context.Context,
	stream io.Reader,
	writer *lockedTerminalWriter,
	activity chan<- struct{},
) string {
	for {
		event, err := service.ReadEnvdPTYEvent(stream)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return terminalCloseBackendDisconnected
			}
			_ = writer.control(map[string]interface{}{"type": "error", "message": "terminal backend disconnected"})
			return terminalCloseBackendError
		}
		terminalActivity(activity)
		if len(event.Output) > 0 {
			if err := writeEnvdPTYOutput(writer, event.Output); err != nil {
				return terminalCloseBrowserDisconnected
			}
		}
		if event.Exited {
			_ = writer.control(map[string]interface{}{
				"type":     "exit",
				"exitCode": event.ExitCode,
				"error":    event.Error,
			})
			return terminalCloseProcessExited
		}
		if event.StreamEnd {
			_ = writer.control(map[string]interface{}{"type": "streamEnd"})
			return terminalCloseBackendDisconnected
		}
	}
}

func writeEnvdPTYOutput(writer *lockedTerminalWriter, output []byte) error {
	return writer.control(map[string]interface{}{
		"type": "output",
		"data": base64.StdEncoding.EncodeToString(output),
	})
}

func terminalActivity(activity chan<- struct{}) {
	select {
	case activity <- struct{}{}:
	default:
	}
}

func resetTerminalTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(terminalIdleTimeout)
}

func terminalDimensions(rows, cols uint32) (uint32, uint32, error) {
	if rows == 0 {
		rows = terminalDefaultRows
	}
	if cols == 0 {
		cols = terminalDefaultCols
	}
	if rows > terminalMaximumEnvdRows || cols > terminalMaximumEnvdCols {
		return 0, 0, fmt.Errorf(
			"terminal dimensions must not exceed %d rows by %d columns",
			terminalMaximumEnvdRows,
			terminalMaximumEnvdCols,
		)
	}
	return rows, cols, nil
}

func decodeTerminalSandboxDetail(raw json.RawMessage) (*translator.CMSandboxDetailItem, error) {
	var envelope translator.CMEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("invalid CubeMaster sandbox response: %w", err)
	}
	var details []translator.CMSandboxDetailItem
	if err := json.Unmarshal(envelope.Data, &details); err != nil || len(details) == 0 {
		return nil, errors.New("CubeMaster returned no sandbox detail")
	}
	return &details[0], nil
}

type terminalStatusError struct {
	Status  int
	Message string
}

func (e *terminalStatusError) Error() string { return e.Message }

func selectTerminalContainer(detail *translator.CMSandboxDetailItem, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, container := range detail.Containers {
			if container.ContainerID != requested {
				continue
			}
			if container.Status != 1 {
				return "", &terminalStatusError{
					Status:  http.StatusConflict,
					Message: fmt.Sprintf("container %s in sandbox %s is not running", requested, detail.SandboxID),
				}
			}
			if !terminalContainerSupportsEnvd(container) {
				return "", &terminalStatusError{
					Status:  http.StatusConflict,
					Message: fmt.Sprintf("container %s in sandbox %s does not expose the envd PTY", requested, detail.SandboxID),
				}
			}
			return container.ContainerID, nil
		}
		return "", &terminalStatusError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("container %s was not found in sandbox %s", requested, detail.SandboxID),
		}
	}
	if len(detail.Containers) == 0 {
		return detail.SandboxID, nil
	}
	running := make([]translator.CMSandboxContainer, 0, len(detail.Containers))
	for _, container := range detail.Containers {
		if container.Status == 1 && terminalContainerSupportsEnvd(container) {
			running = append(running, container)
		}
	}
	switch len(running) {
	case 0:
		return "", &terminalStatusError{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("sandbox %s has no running container available for terminal", detail.SandboxID),
		}
	case 1:
		return running[0].ContainerID, nil
	default:
		return "", errors.New("containerID is required when a sandbox has multiple running containers")
	}
}

func terminalContainerSupportsEnvd(container translator.CMSandboxContainer) bool {
	return container.Type == "" || container.Type == "sandbox"
}

func terminalOriginAllowed(r *http.Request) bool {
	host := strings.TrimSpace(r.Host)
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return isLoopbackHost(host)
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	if strings.EqualFold(parsed.Host, host) {
		return true
	}
	return isLoopbackHost(parsed.Host) && isLoopbackHost(host)
}

func isLoopbackHost(authority string) bool {
	host := authority
	if parsedHost, _, err := net.SplitHostPort(authority); err == nil {
		host = parsedHost
	} else if strings.HasPrefix(authority, "[") && strings.Contains(authority, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(strings.Split(authority, "]")[0], "["), "]")
	} else if strings.Count(authority, ":") == 1 {
		host = strings.Split(authority, ":")[0]
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

type lockedTerminalWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *lockedTerminalWriter) control(value interface{}) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	return w.conn.WriteMessage(websocket.TextMessage, payload)
}

func (w *lockedTerminalWriter) pong(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteControl(websocket.PongMessage, payload, time.Now().Add(terminalWriteTimeout))
}

func (w *lockedTerminalWriter) ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(terminalWriteTimeout))
}

func (w *lockedTerminalWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(terminalWriteTimeout),
	)
}

func writeTerminalJSON(conn *websocket.Conn, value interface{}) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, payload)
}
