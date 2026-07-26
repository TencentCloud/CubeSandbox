// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
)

const (
	terminalGatewayTokenHeader    = "X-Cube-Terminal-Token"
	terminalTicketTTL             = 60 * time.Second
	terminalFrameLimit            = 256 * 1024
	terminalOpenTimeout           = 15 * time.Second
	terminalWriteTimeout          = 15 * time.Second
	terminalIdleTimeout           = 30 * time.Minute
	terminalMaxDimension          = 1000
	terminalMaxCWDBytes           = 4096
	terminalMaxEnvEntries         = 128
	terminalMaxEnvBytes           = 32 * 1024
	terminalMaxTickets            = 256
	terminalMaxTicketsPerUser     = 8
	terminalTicketRateWindow      = time.Minute
	terminalMaxTicketsPerWindow   = 20
	terminalMaxSessions           = 64
	terminalMaxSessionsPerUser    = 4
	terminalMaxSessionsPerSandbox = 8
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
	ContainerID string            `json:"containerID"`
	Rows        uint32            `json:"rows"`
	Cols        uint32            `json:"cols"`
	CWD         string            `json:"cwd"`
	Envs        map[string]string `json:"envs"`
	User        string            `json:"user"`
}

type terminalTicketResponse struct {
	Ticket       string `json:"ticket"`
	ExpiresAt    string `json:"expiresAt"`
	WebSocketURL string `json:"websocketUrl"`
	ContainerID  string `json:"containerID,omitempty"`
}

type terminalTicket struct {
	SandboxID   string
	ContainerID string
	CreatedBy   string
	Rows        uint32
	Cols        uint32
	CWD         string
	Envs        map[string]string
	ExpiresAt   time.Time
}

type terminalTicketStore struct {
	mu           sync.Mutex
	tickets      map[string]terminalTicket
	issuedByUser map[string][]time.Time
	now          func() time.Time
}

func newTerminalTicketStore() *terminalTicketStore {
	return &terminalTicketStore{
		tickets:      make(map[string]terminalTicket),
		issuedByUser: make(map[string][]time.Time),
		now:          time.Now,
	}
}

func (s *terminalTicketStore) issue(ticket terminalTicket) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredLocked(now)
	s.pruneIssueHistoryLocked(now)
	if len(s.tickets) >= terminalMaxTickets {
		return "", errors.New("too many pending terminal tickets")
	}
	pendingForUser := 0
	for _, existing := range s.tickets {
		if existing.CreatedBy == ticket.CreatedBy {
			pendingForUser++
		}
	}
	if pendingForUser >= terminalMaxTicketsPerUser {
		return "", errors.New("too many pending terminal tickets for this user")
	}
	if len(s.issuedByUser[ticket.CreatedBy]) >= terminalMaxTicketsPerWindow {
		return "", errors.New("terminal ticket rate limit exceeded for this user")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate terminal ticket: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s.tickets[token] = ticket
	s.issuedByUser[ticket.CreatedBy] = append(s.issuedByUser[ticket.CreatedBy], now)
	return token, nil
}

func (s *terminalTicketStore) claim(token, sandboxID string) (terminalTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredLocked(s.now())
	ticket, ok := s.tickets[token]
	if ok {
		delete(s.tickets, token)
	}
	if !ok || ticket.SandboxID != sandboxID {
		return terminalTicket{}, errors.New("terminal ticket is invalid or expired")
	}
	return ticket, nil
}

func (s *terminalTicketStore) pruneExpiredLocked(now time.Time) {
	for token, existing := range s.tickets {
		if !existing.ExpiresAt.After(now) {
			delete(s.tickets, token)
		}
	}
}

func (s *terminalTicketStore) pruneIssueHistoryLocked(now time.Time) {
	cutoff := now.Add(-terminalTicketRateWindow)
	for username, issuedAt := range s.issuedByUser {
		firstCurrent := 0
		for firstCurrent < len(issuedAt) && !issuedAt[firstCurrent].After(cutoff) {
			firstCurrent++
		}
		if firstCurrent == len(issuedAt) {
			delete(s.issuedByUser, username)
			continue
		}
		s.issuedByUser[username] = issuedAt[firstCurrent:]
	}
}

type terminalSessionLimiter struct {
	mu        sync.Mutex
	total     int
	byUser    map[string]int
	bySandbox map[string]int
}

func newTerminalSessionLimiter() *terminalSessionLimiter {
	return &terminalSessionLimiter{
		byUser:    make(map[string]int),
		bySandbox: make(map[string]int),
	}
}

func (l *terminalSessionLimiter) acquire(username, sandboxID string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.total >= terminalMaxSessions {
		return nil, errors.New("terminal session capacity reached")
	}
	if l.byUser[username] >= terminalMaxSessionsPerUser {
		return nil, errors.New("too many active terminal sessions for this user")
	}
	if l.bySandbox[sandboxID] >= terminalMaxSessionsPerSandbox {
		return nil, errors.New("too many active terminal sessions for this sandbox")
	}

	l.total++
	l.byUser[username]++
	l.bySandbox[sandboxID]++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			decrementTerminalSessionCount(l.byUser, username)
			decrementTerminalSessionCount(l.bySandbox, sandboxID)
		})
	}, nil
}

func decrementTerminalSessionCount(counts map[string]int, key string) {
	if counts[key] <= 1 {
		delete(counts, key)
		return
	}
	counts[key]--
}

// TerminalGateway owns browser authentication and relays terminal traffic to
// CubeMaster. CubeAPI is intentionally not part of this ops-only flow.
type TerminalGateway struct {
	cm           CubeMasterClient
	masterAddr   string
	gatewayToken string
	tickets      *terminalTicketStore
	sessions     *terminalSessionLimiter
	upgrader     websocket.Upgrader
	dialer       websocket.Dialer
}

func NewTerminalGateway(cm CubeMasterClient, masterAddr, gatewayToken string) *TerminalGateway {
	return &TerminalGateway{
		cm:           cm,
		masterAddr:   masterAddr,
		gatewayToken: gatewayToken,
		tickets:      newTerminalTicketStore(),
		sessions:     newTerminalSessionLimiter(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin:     terminalOriginAllowed,
		},
		dialer: websocket.Dialer{
			HandshakeTimeout: terminalOpenTimeout,
			ReadBufferSize:   32 * 1024,
			WriteBufferSize:  32 * 1024,
			TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

func (h *TerminalGateway) RegisterPublic(r *gin.RouterGroup) {
	r.GET("/terminal/sandboxes/:id/ws", h.OpenWebSocket)
}

func (h *TerminalGateway) RegisterAuthed(r *gin.RouterGroup) {
	r.POST("/sandboxes/:id/terminal/tickets", h.CreateTicket)
}

func (h *TerminalGateway) CreateTicket(c *gin.Context) {
	if strings.TrimSpace(h.gatewayToken) == "" {
		httputil.WriteError(c, http.StatusServiceUnavailable, "web terminal gateway is not configured")
		return
	}

	sandboxID := strings.TrimSpace(c.Param("id"))
	if sandboxID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "sandbox id is required")
		return
	}

	var body terminalTicketRequest
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		httputil.WriteError(c, http.StatusBadRequest, "invalid terminal ticket request")
		return
	}
	if err := validateTerminalTicketRequest(&body); err != nil {
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

	rows, cols := terminalDimensions(body.Rows, body.Cols)
	expiresAt := time.Now().Add(terminalTicketTTL)
	ticket := terminalTicket{
		SandboxID:   sandboxID,
		ContainerID: containerID,
		CreatedBy:   c.GetString("username"),
		Rows:        rows,
		Cols:        cols,
		CWD:         strings.TrimSpace(body.CWD),
		Envs:        cloneStrings(body.Envs),
		ExpiresAt:   expiresAt,
	}
	token, err := h.tickets.issue(ticket)
	if err != nil {
		httputil.WriteError(c, http.StatusTooManyRequests, err.Error())
		return
	}

	slog.Info("terminal ticket issued",
		"sandbox_id", sandboxID,
		"container_id", containerID,
		"username", ticket.CreatedBy,
	)
	httputil.WriteJSON(c, http.StatusCreated, terminalTicketResponse{
		Ticket:       token,
		ExpiresAt:    expiresAt.UTC().Format(time.RFC3339),
		WebSocketURL: fmt.Sprintf("/opsapi/v1/terminal/sandboxes/%s/ws?ticket=%s", url.PathEscape(sandboxID), url.QueryEscape(token)),
		ContainerID:  containerID,
	})
}

func (h *TerminalGateway) OpenWebSocket(c *gin.Context) {
	if strings.TrimSpace(h.gatewayToken) == "" {
		httputil.WriteError(c, http.StatusServiceUnavailable, "web terminal gateway is not configured")
		return
	}
	if !terminalOriginAllowed(c.Request) {
		httputil.WriteError(c, http.StatusForbidden, "terminal websocket origin is not allowed")
		return
	}
	ticket, err := h.tickets.claim(c.Query("ticket"), c.Param("id"))
	if err != nil {
		httputil.WriteError(c, http.StatusUnauthorized, err.Error())
		return
	}
	releaseSession, err := h.sessions.acquire(ticket.CreatedBy, ticket.SandboxID)
	if err != nil {
		httputil.WriteError(c, http.StatusTooManyRequests, err.Error())
		return
	}
	defer releaseSession()

	browser, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer browser.Close()
	browser.SetReadLimit(terminalFrameLimit)

	backendURL, err := cubeMasterTerminalURL(h.masterAddr)
	if err != nil {
		writeTerminalBrowserControl(browser, map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}
	headers := http.Header{}
	headers.Set(terminalGatewayTokenHeader, h.gatewayToken)
	backend, _, err := h.dialer.DialContext(c.Request.Context(), backendURL, headers)
	if err != nil {
		writeTerminalBrowserControl(browser, map[string]interface{}{"type": "error", "message": "failed to connect terminal backend"})
		slog.Warn("terminal backend connection failed", "sandbox_id", ticket.SandboxID, "error", err)
		return
	}
	defer backend.Close()
	backend.SetReadLimit(terminalFrameLimit)

	env := make([]string, 0, len(ticket.Envs))
	for key, value := range ticket.Envs {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	open := map[string]interface{}{
		"type":        "open",
		"requestID":   "cubeops-terminal-" + uuid.NewString(),
		"sandboxID":   ticket.SandboxID,
		"containerID": ticket.ContainerID,
		"cwd":         ticket.CWD,
		"env":         env,
		"cols":        ticket.Cols,
		"rows":        ticket.Rows,
	}
	if err := writeTerminalJSON(backend, open); err != nil {
		writeTerminalBrowserControl(browser, map[string]interface{}{"type": "error", "message": "failed to open terminal backend"})
		return
	}
	execID, err := awaitTerminalReady(backend)
	if err != nil {
		writeTerminalBrowserControl(browser, map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}

	sessionID := uuid.NewString()
	if err := writeTerminalBrowserControl(browser, map[string]interface{}{
		"type": "start", "execId": execID, "sessionId": sessionID,
	}); err != nil {
		return
	}
	slog.Info("terminal session opened",
		"sandbox_id", ticket.SandboxID,
		"container_id", ticket.ContainerID,
		"username", ticket.CreatedBy,
		"session_id", sessionID,
	)
	openedAt := time.Now()

	browserWriter := &lockedTerminalWriter{conn: browser}
	backendWriter := &lockedTerminalWriter{conn: backend}
	browser.SetPingHandler(func(data string) error { return browserWriter.pong([]byte(data)) })
	backend.SetPingHandler(func(data string) error { return backendWriter.pong([]byte(data)) })

	done := make(chan string, 2)
	go func() {
		done <- runCubeOpsTerminalRelay("browser-to-backend", func() string {
			return relayTerminalBrowserToBackend(browser, browserWriter, backendWriter)
		})
	}()
	go func() {
		done <- runCubeOpsTerminalRelay("backend-to-browser", func() string {
			return relayTerminalBackendToBrowser(backend, browserWriter)
		})
	}()
	reason := <-done
	switch reason {
	case terminalCloseIdleTimeout:
		_ = browserWriter.control(map[string]interface{}{
			"type":               "idleTimeout",
			"idleTimeoutSeconds": int(terminalIdleTimeout.Seconds()),
		})
	case terminalCloseBackendDisconnected:
		_ = browserWriter.control(map[string]interface{}{"type": "streamEnd"})
	case terminalCloseRelayFailure:
		_ = browserWriter.control(map[string]interface{}{"type": "error", "message": "terminal relay failed"})
	}
	_ = backendWriter.text([]byte(`{"type":"close"}`))
	_ = backendWriter.close()
	_ = browserWriter.close()

	slog.Info("terminal session closed",
		"sandbox_id", ticket.SandboxID,
		"container_id", ticket.ContainerID,
		"username", ticket.CreatedBy,
		"session_id", sessionID,
		"reason", reason,
		"duration_ms", time.Since(openedAt).Milliseconds(),
	)
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

func validateTerminalTicketRequest(body *terminalTicketRequest) error {
	if body == nil {
		return errors.New("terminal ticket request is required")
	}
	body.User = strings.TrimSpace(body.User)
	if body.User != "" && body.User != "root" {
		return errors.New("web terminal currently supports only the root user")
	}
	if body.Rows > terminalMaxDimension || body.Cols > terminalMaxDimension {
		return fmt.Errorf("terminal dimensions must not exceed %d", terminalMaxDimension)
	}
	if body.CWD != "" && (!strings.HasPrefix(body.CWD, "/") || len(body.CWD) > terminalMaxCWDBytes || strings.ContainsRune(body.CWD, '\x00')) {
		return fmt.Errorf("terminal cwd must be absolute, at most %d bytes, and contain no NUL characters", terminalMaxCWDBytes)
	}
	if len(body.Envs) > terminalMaxEnvEntries {
		return fmt.Errorf("terminal envs must contain at most %d entries", terminalMaxEnvEntries)
	}
	total := 0
	for key, value := range body.Envs {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return errors.New("terminal environment keys must be non-empty and environment values must contain no NUL characters")
		}
		total += len(key) + len(value) + 1
	}
	if total > terminalMaxEnvBytes {
		return fmt.Errorf("terminal envs exceed %d bytes", terminalMaxEnvBytes)
	}
	return nil
}

func terminalDimensions(rows, cols uint32) (uint32, uint32) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	return rows, cols
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
		if container.Status == 1 {
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

func cubeMasterTerminalURL(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid CubeMaster URL: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported CubeMaster URL scheme %q", parsed.Scheme)
	}
	parsed.Path = "/cube/sandbox/terminal"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func awaitTerminalReady(backend *websocket.Conn) (string, error) {
	_ = backend.SetReadDeadline(time.Now().Add(terminalOpenTimeout))
	defer backend.SetReadDeadline(time.Time{})
	for {
		messageType, payload, err := backend.ReadMessage()
		if err != nil {
			return "", fmt.Errorf("terminal backend closed before ready: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var control struct {
			Type    string `json:"type"`
			ExecID  string `json:"execID"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload, &control); err != nil {
			return "", errors.New("invalid terminal backend control message")
		}
		switch control.Type {
		case "ready":
			if control.ExecID == "" {
				return "", errors.New("terminal backend ready message missing execID")
			}
			return control.ExecID, nil
		case "error":
			if control.Message == "" {
				control.Message = "terminal backend rejected the session"
			}
			return "", errors.New(control.Message)
		}
	}
}

type terminalBrowserMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Rows uint32 `json:"rows"`
	Cols uint32 `json:"cols"`
}

func relayTerminalBrowserToBackend(browser *websocket.Conn, browserWriter, backendWriter *lockedTerminalWriter) string {
	for {
		_ = browser.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
		messageType, payload, err := browser.ReadMessage()
		if err != nil {
			if isTerminalTimeout(err) {
				return terminalCloseIdleTimeout
			}
			return terminalCloseBrowserDisconnected
		}
		switch messageType {
		case websocket.BinaryMessage:
			if err := backendWriter.binary(payload); err != nil {
				return "backend input failed"
			}
		case websocket.TextMessage:
			var message terminalBrowserMessage
			if err := json.Unmarshal(payload, &message); err != nil {
				_ = browserWriter.control(map[string]interface{}{"type": "error", "message": "invalid terminal message"})
				continue
			}
			switch message.Type {
			case "input", "stdin":
				if err := backendWriter.binary([]byte(message.Data)); err != nil {
					return "backend input failed"
				}
			case "inputBase64", "stdinBase64":
				data, err := base64.StdEncoding.DecodeString(message.Data)
				if err != nil {
					_ = browserWriter.control(map[string]interface{}{"type": "error", "message": "invalid terminal input base64"})
					continue
				}
				if err := backendWriter.binary(data); err != nil {
					return "backend input failed"
				}
			case "resize":
				rows, cols := terminalDimensions(message.Rows, message.Cols)
				rows = min(rows, terminalMaxDimension)
				cols = min(cols, terminalMaxDimension)
				if err := backendWriter.control(map[string]interface{}{"type": "resize", "rows": rows, "cols": cols}); err != nil {
					return "backend resize failed"
				}
			case "kill":
				_ = backendWriter.control(map[string]interface{}{"type": "close"})
				return "browser requested close"
			case "ping":
				_ = backendWriter.control(map[string]interface{}{"type": "heartbeat"})
			default:
				_ = browserWriter.control(map[string]interface{}{"type": "error", "message": "unsupported terminal message"})
			}
		}
	}
}

func relayTerminalBackendToBrowser(backend *websocket.Conn, browserWriter *lockedTerminalWriter) string {
	for {
		_ = backend.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
		messageType, payload, err := backend.ReadMessage()
		if err != nil {
			if isTerminalTimeout(err) {
				return terminalCloseIdleTimeout
			}
			return terminalCloseBackendDisconnected
		}
		switch messageType {
		case websocket.BinaryMessage:
			if err := browserWriter.control(map[string]interface{}{
				"type": "output",
				"data": base64.StdEncoding.EncodeToString(payload),
			}); err != nil {
				return "browser output failed"
			}
		case websocket.TextMessage:
			var control struct {
				Type    string `json:"type"`
				Code    uint32 `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(payload, &control); err != nil {
				_ = browserWriter.control(map[string]interface{}{"type": "error", "message": "invalid terminal backend message"})
				return "invalid backend message"
			}
			switch control.Type {
			case "exit":
				_ = browserWriter.control(map[string]interface{}{"type": "exit", "exitCode": control.Code})
				return terminalCloseProcessExited
			case "error":
				_ = browserWriter.control(map[string]interface{}{"type": "error", "message": control.Message})
				return terminalCloseBackendError
			case "heartbeat":
				// The heartbeat is only an activity signal.
			}
		}
	}
}

func isTerminalTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
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
	return w.text(payload)
}

func (w *lockedTerminalWriter) text(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	return w.conn.WriteMessage(websocket.TextMessage, payload)
}

func (w *lockedTerminalWriter) binary(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	return w.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (w *lockedTerminalWriter) pong(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteControl(websocket.PongMessage, payload, time.Now().Add(terminalWriteTimeout))
}

func (w *lockedTerminalWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(terminalWriteTimeout))
}

func writeTerminalJSON(conn *websocket.Conn, value interface{}) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func writeTerminalBrowserControl(conn *websocket.Conn, value interface{}) error {
	return writeTerminalJSON(conn, value)
}

func cloneStrings(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
