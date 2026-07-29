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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

const (
	terminalProtocol      = "cube-terminal.v1"
	terminalGrantPrefix   = "cube-terminal.grant."
	terminalWriteTimeout  = 10 * time.Second
	maxTerminalMessage    = 64 << 10
	envdVersionAnnotation = "cube.master.components.envd.version"

	terminalUnavailableClientMessage = "terminal is unavailable for this sandbox; use an envd-enabled template and verify /bin/sh"
)

type terminalRuntimeConfig struct {
	grantTTL    time.Duration
	idleTimeout time.Duration
	maxDuration time.Duration
	pingPeriod  time.Duration
}

func defaultTerminalRuntimeConfig() terminalRuntimeConfig {
	return terminalRuntimeConfig{
		grantTTL:    time.Minute,
		idleTimeout: 30 * time.Minute,
		maxDuration: 8 * time.Hour,
		pingPeriod:  30 * time.Second,
	}
}

type terminalControl struct {
	Type string `json:"type"`
	Rows int    `json:"rows,omitempty"`
	Cols int    `json:"cols,omitempty"`
}

type terminalSessionResponse struct {
	Grant        string `json:"grant"`
	Protocol     string `json:"protocol"`
	WebsocketURL string `json:"websocketURL"`
}

type terminalPTY interface {
	PID() int
	Output() <-chan []byte
	Wait(func([]byte)) (int, error)
	ErrorMessage() string
	SendStdin(context.Context, []byte) error
	Resize(context.Context, cubesandbox.PtySize) error
	Kill(context.Context) (bool, error)
	Disconnect() error
}

type terminalFactory interface {
	Open(context.Context, string, cubesandbox.PtySize, time.Duration) (terminalPTY, error)
	Close() error
}

type sdkTerminalFactory struct {
	client *cubesandbox.Client
}

func newSDKTerminalFactory(cfg *config.Config) (*sdkTerminalFactory, error) {
	proxyURL, err := url.Parse(cfg.SandboxProxyURL)
	if err != nil || proxyURL.Hostname() == "" {
		return nil, fmt.Errorf("invalid sandbox_proxy_url %q", cfg.SandboxProxyURL)
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, fmt.Errorf("sandbox_proxy_url must use http or https")
	}
	if proxyURL.User != nil || (proxyURL.Path != "" && proxyURL.Path != "/") ||
		proxyURL.RawQuery != "" || proxyURL.Fragment != "" {
		return nil, fmt.Errorf("sandbox_proxy_url must contain only scheme, host, and port")
	}

	port := 80
	if proxyURL.Scheme == "https" {
		port = 443
	}
	if proxyURL.Port() != "" {
		port, err = strconv.Atoi(proxyURL.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid sandbox_proxy_url port")
		}
	}

	return &sdkTerminalFactory{client: cubesandbox.NewClient(cubesandbox.Config{
		APIURL:         "http://cubeops.invalid",
		ProxyNodeIP:    proxyURL.Hostname(),
		ProxyPortHTTP:  port,
		ProxyScheme:    proxyURL.Scheme,
		SandboxDomain:  cfg.SandboxDomain,
		RequestTimeout: 10 * time.Second,
	})}, nil
}

func (f *sdkTerminalFactory) Open(
	ctx context.Context,
	sandboxID string,
	size cubesandbox.PtySize,
	maxDuration time.Duration,
) (terminalPTY, error) {
	sandbox, err := f.client.Attach(sandboxID)
	if err != nil {
		return nil, err
	}
	return sandbox.Pty().Create(ctx, size, cubesandbox.PtyCreateOptions{
		Command: "/bin/sh",
		Timeout: maxDuration,
	})
}

func (f *sdkTerminalFactory) Close() error {
	return f.client.Close()
}

// TerminalHandler keeps browser terminal traffic in CubeOps and the existing
// envd data plane. It does not add a CubeAPI or Cubelet RPC.
type TerminalHandler struct {
	cm interface {
		GetSandbox(context.Context, string, string) (json.RawMessage, error)
	}
	jm         *auth.JWTManager
	factory    terminalFactory
	factoryErr error
	runtime    terminalRuntimeConfig

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	lifecycleMu     sync.Mutex
	lifecycleWG     sync.WaitGroup
	closing         bool
}

func NewTerminalHandler(cm CubeMasterClient, jm *auth.JWTManager, cfg *config.Config) *TerminalHandler {
	factory, err := newSDKTerminalFactory(cfg)
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &TerminalHandler{
		cm:              cm,
		jm:              jm,
		factory:         factory,
		factoryErr:      err,
		runtime:         defaultTerminalRuntimeConfig(),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
}

func (h *TerminalHandler) RegisterAuthed(r *gin.RouterGroup) {
	r.POST("/terminal/sandboxes/:id/sessions", h.CreateSession)
}

func (h *TerminalHandler) RegisterPublic(r *gin.RouterGroup) {
	r.GET("/terminal/sandboxes/:id/ws", h.Connect)
}

// Shutdown cancels hijacked WebSocket connections before releasing the shared
// SDK client. net/http does not track hijacked connections during Shutdown.
func (h *TerminalHandler) Shutdown(ctx context.Context) error {
	h.lifecycleMu.Lock()
	if !h.closing {
		h.closing = true
		h.lifecycleCancel()
	}
	h.lifecycleMu.Unlock()

	done := make(chan struct{})
	go func() {
		h.lifecycleWG.Wait()
		close(done)
	}()

	var waitErr error
	select {
	case <-done:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	if h.factory == nil {
		return waitErr
	}
	return errors.Join(waitErr, h.factory.Close())
}

func (h *TerminalHandler) CreateSession(c *gin.Context) {
	if h.factoryErr != nil {
		httputil.WriteError(c, http.StatusServiceUnavailable, "terminal data plane is not configured")
		return
	}
	if h.isClosing() {
		httputil.WriteError(c, http.StatusServiceUnavailable, "terminal service is shutting down")
		return
	}
	operator := c.GetString("username")
	if operator == "" {
		httputil.WriteError(c, http.StatusUnauthorized, "authenticated operator is required")
		return
	}
	sandboxID := strings.TrimSpace(c.Param("id"))
	if sandboxID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "sandbox ID is required")
		return
	}

	grant, claims, err := h.jm.GenerateTerminalGrant(operator, sandboxID, h.runtime.grantTTL)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to create terminal grant")
		return
	}
	containerID, err := h.validateTarget(c.Request.Context(), sandboxID)
	if err != nil {
		logTerminalAudit(
			slog.LevelWarn,
			"terminal.failed",
			claims,
			"",
			c.ClientIP(),
			"target_validation_failed",
			0,
			err,
		)
		writeTerminalTargetError(c, err)
		return
	}
	logTerminalAudit(
		slog.LevelInfo,
		"terminal.grant",
		claims,
		containerID,
		c.ClientIP(),
		"",
		0,
		nil,
	)
	httputil.WriteJSON(c, http.StatusCreated, terminalSessionResponse{
		Grant:        grant,
		Protocol:     terminalProtocol,
		WebsocketURL: "/opsapi/v1/terminal/sandboxes/" + url.PathEscape(sandboxID) + "/ws",
	})
}

func (h *TerminalHandler) Connect(c *gin.Context) {
	if h.factoryErr != nil {
		httputil.WriteError(c, http.StatusServiceUnavailable, "terminal data plane is not configured")
		return
	}

	protocols := c.GetHeader("Sec-WebSocket-Protocol")
	if !terminalProtocolRequested(protocols) {
		httputil.WriteError(c, http.StatusBadRequest, "terminal WebSocket protocol is required")
		return
	}
	claims, err := h.jm.VerifyTerminalGrant(terminalGrantFromProtocols(protocols))
	if err != nil || claims.SandboxID != strings.TrimSpace(c.Param("id")) {
		httputil.WriteError(c, http.StatusUnauthorized, "invalid or expired terminal grant")
		return
	}
	containerID, err := h.validateTarget(c.Request.Context(), claims.SandboxID)
	if err != nil {
		logTerminalAudit(
			slog.LevelWarn,
			"terminal.failed",
			claims,
			"",
			c.ClientIP(),
			"target_revalidation_failed",
			0,
			err,
		)
		writeTerminalTargetError(c, err)
		return
	}
	if !h.beginSession() {
		logTerminalAudit(
			slog.LevelWarn,
			"terminal.failed",
			claims,
			containerID,
			c.ClientIP(),
			"server_shutdown",
			0,
			errors.New("terminal service is shutting down"),
		)
		httputil.WriteError(c, http.StatusServiceUnavailable, "terminal service is shutting down")
		return
	}
	defer h.lifecycleWG.Done()

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		Subprotocols:    []string{terminalProtocol},
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logTerminalAudit(
			slog.LevelWarn,
			"terminal.failed",
			claims,
			containerID,
			c.ClientIP(),
			"websocket_upgrade_failed",
			0,
			err,
		)
		return
	}
	h.relay(conn, claims, containerID, c.ClientIP())
}

func (h *TerminalHandler) beginSession() bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.closing {
		return false
	}
	h.lifecycleWG.Add(1)
	return true
}

func (h *TerminalHandler) isClosing() bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	return h.closing
}

func (h *TerminalHandler) validateTarget(ctx context.Context, sandboxID string) (string, error) {
	raw, err := h.cm.GetSandbox(ctx, sandboxID, sdkInstanceType)
	if err != nil {
		return "", err
	}
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("invalid CubeMaster sandbox response")
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != http.StatusOK {
		if env.Ret.RetCode == 130404 || env.Ret.RetCode == http.StatusNotFound {
			return "", errTerminalNotFound
		}
		return "", fmt.Errorf("CubeMaster rejected terminal target: %s", env.Ret.RetMsg)
	}

	var items []cmSandboxDetailItem
	if err := json.Unmarshal(env.Data, &items); err != nil || len(items) == 0 {
		return "", errTerminalNotFound
	}
	var item *cmSandboxDetailItem
	for i := range items {
		if items[i].SandboxID == sandboxID {
			item = &items[i]
			break
		}
	}
	if item == nil && len(items) == 1 && items[0].SandboxID == "" {
		item = &items[0]
	}
	if item == nil {
		return "", errTerminalNotFound
	}
	if item.Status != 1 {
		return "", errTerminalNotRunning
	}
	if strings.TrimSpace(item.Annotations[envdVersionAnnotation]) == "" {
		return "", errTerminalEnvdUnavailable
	}

	for _, container := range item.Containers {
		if container.Type == "sandbox" || container.ContainerID == sandboxID {
			if container.Status != 1 || strings.TrimSpace(container.ContainerID) == "" {
				return "", errTerminalNotRunning
			}
			return container.ContainerID, nil
		}
	}
	if len(item.Containers) == 0 {
		return "", errTerminalNotRunning
	}
	if item.Containers[0].Status != 1 || strings.TrimSpace(item.Containers[0].ContainerID) == "" {
		return "", errTerminalNotRunning
	}
	return item.Containers[0].ContainerID, nil
}

var (
	errTerminalNotFound        = errors.New("sandbox not found")
	errTerminalNotRunning      = errors.New("sandbox must be running before opening a terminal")
	errTerminalEnvdUnavailable = errors.New("terminal requires an envd-enabled template")
)

func writeTerminalTargetError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errTerminalNotFound):
		httputil.WriteError(c, http.StatusNotFound, err.Error())
	case errors.Is(err, errTerminalNotRunning), errors.Is(err, errTerminalEnvdUnavailable):
		httputil.WriteError(c, http.StatusConflict, err.Error())
	default:
		writeCMError(c, err)
	}
}

type terminalInput struct {
	data          []byte
	control       terminalControl
	err           error
	protocolError bool
}

func (h *TerminalHandler) relay(
	conn *websocket.Conn,
	claims *auth.TerminalClaims,
	containerID string,
	remoteAddr string,
) {
	started := time.Now()
	reason := "disconnected"
	defer func() {
		_ = conn.Close()
		logTerminalAudit(
			slog.LevelInfo,
			"terminal.ended",
			claims,
			containerID,
			remoteAddr,
			reason,
			time.Since(started),
			nil,
		)
	}()

	ctx, cancel := context.WithTimeout(h.lifecycleCtx, h.runtime.maxDuration)
	defer cancel()
	pty, err := h.factory.Open(
		ctx,
		claims.SandboxID,
		cubesandbox.PtySize{Rows: 24, Cols: 80},
		h.runtime.maxDuration,
	)
	if err != nil {
		reason = "pty_start_failed"
		logTerminalAudit(
			slog.LevelWarn,
			"terminal.failed",
			claims,
			containerID,
			remoteAddr,
			reason,
			time.Since(started),
			err,
		)
		_ = writeTerminalJSON(conn, gin.H{"type": "error", "message": terminalUnavailableClientMessage})
		_ = closeTerminalWebSocket(conn, websocket.CloseNormalClosure, "terminal unavailable")
		return
	}
	defer cleanupTerminalPTY(pty, claims.ID)

	logTerminalAudit(
		slog.LevelInfo,
		"terminal.started",
		claims,
		containerID,
		remoteAddr,
		"",
		0,
		nil,
		"pid",
		pty.PID(),
	)
	if err := writeTerminalJSON(conn, gin.H{"type": "status", "message": "connected"}); err != nil {
		reason = "client_write_failed"
		return
	}

	input := make(chan terminalInput, 16)
	go readTerminalInput(ctx, conn, input)
	idle := time.NewTimer(h.runtime.idleTimeout)
	defer idle.Stop()
	ping := time.NewTicker(h.runtime.pingPeriod)
	defer ping.Stop()

	for {
		select {
		case output, ok := <-pty.Output():
			if !ok {
				var finishErr error
				reason, finishErr = h.finishTerminal(conn, pty)
				if finishErr != nil {
					logTerminalAudit(
						slog.LevelWarn,
						"terminal.failed",
						claims,
						containerID,
						remoteAddr,
						reason,
						time.Since(started),
						finishErr,
					)
				}
				return
			}
			if err := writeTerminalMessage(conn, websocket.BinaryMessage, output); err != nil {
				reason = "client_write_failed"
				return
			}
			resetTimer(idle, h.runtime.idleTimeout)
		case event := <-input:
			if event.err != nil {
				reason = "client_disconnected"
				if event.protocolError {
					reason = "protocol_error"
					_ = closeTerminalWebSocket(conn, websocket.CloseProtocolError, "invalid terminal control frame")
				}
				return
			}
			if event.data != nil {
				if err := pty.SendStdin(ctx, event.data); err != nil {
					reason = "stdin_failed"
					_ = writeTerminalJSON(conn, gin.H{"type": "error", "message": "failed to write terminal input"})
					return
				}
				resetTimer(idle, h.runtime.idleTimeout)
				continue
			}
			if event.control.Type != "resize" {
				reason = "protocol_error"
				_ = closeTerminalWebSocket(conn, websocket.CloseProtocolError, "unsupported terminal control frame")
				return
			}
			if !validTerminalSize(event.control) {
				reason = "protocol_error"
				_ = closeTerminalWebSocket(conn, websocket.CloseProtocolError, "invalid terminal size")
				return
			}
			if err := pty.Resize(ctx, cubesandbox.PtySize{Rows: event.control.Rows, Cols: event.control.Cols}); err != nil {
				slog.Warn("terminal resize failed", "session_id", claims.ID, "error", err)
			} else {
				resetTimer(idle, h.runtime.idleTimeout)
			}
		case <-idle.C:
			reason = "idle_timeout"
			_ = writeTerminalJSON(conn, gin.H{"type": "error", "message": "terminal session closed after inactivity"})
			_ = closeTerminalWebSocket(conn, websocket.CloseNormalClosure, "idle timeout")
			return
		case <-ping.C:
			if err := writeTerminalControl(conn, websocket.PingMessage, nil); err != nil {
				reason = "ping_failed"
				return
			}
		case <-ctx.Done():
			reason = "maximum_duration"
			code := websocket.CloseNormalClosure
			message := "terminal session reached its maximum duration"
			if h.lifecycleCtx.Err() != nil {
				reason = "server_shutdown"
				code = websocket.CloseServiceRestart
				message = "terminal service is shutting down"
			}
			_ = writeTerminalJSON(conn, gin.H{"type": "error", "message": message})
			_ = closeTerminalWebSocket(conn, code, message)
			return
		}
	}
}

func cleanupTerminalPTY(pty terminalPTY, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pty.Kill(ctx); err != nil {
		slog.Warn("terminal PTY cleanup failed", "session_id", sessionID, "error", err)
	}
	_ = pty.Disconnect()
}

func (h *TerminalHandler) finishTerminal(conn *websocket.Conn, pty terminalPTY) (string, error) {
	exitCode, err := pty.Wait(nil)
	if err != nil {
		_ = writeTerminalJSON(conn, gin.H{"type": "error", "message": "terminal stream ended unexpectedly"})
		_ = closeTerminalWebSocket(conn, websocket.CloseInternalServerErr, "terminal stream failed")
		return "pty_stream_failed", err
	}
	_ = writeTerminalJSON(conn, gin.H{
		"type":     "exit",
		"exitCode": exitCode,
		"message":  pty.ErrorMessage(),
	})
	_ = closeTerminalWebSocket(conn, websocket.CloseNormalClosure, "terminal exited")
	return "process_exited", nil
}

func validTerminalSize(control terminalControl) bool {
	return control.Rows > 0 && control.Rows <= 1000 && control.Cols > 0 && control.Cols <= 1000
}

func readTerminalInput(ctx context.Context, conn *websocket.Conn, events chan<- terminalInput) {
	conn.SetReadLimit(maxTerminalMessage)
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			sendTerminalInput(ctx, events, terminalInput{err: err})
			return
		}
		switch messageType {
		case websocket.BinaryMessage:
			if !sendTerminalInput(ctx, events, terminalInput{data: payload}) {
				return
			}
		case websocket.TextMessage:
			var control terminalControl
			if err := json.Unmarshal(payload, &control); err != nil {
				sendTerminalInput(ctx, events, terminalInput{err: err, protocolError: true})
				return
			}
			if !sendTerminalInput(ctx, events, terminalInput{control: control}) {
				return
			}
		}
	}
}

func sendTerminalInput(ctx context.Context, events chan<- terminalInput, event terminalInput) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func terminalGrantFromProtocols(header string) string {
	for _, value := range strings.Split(header, ",") {
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, terminalGrantPrefix) {
			return strings.TrimPrefix(value, terminalGrantPrefix)
		}
	}
	return ""
}

func terminalProtocolRequested(header string) bool {
	for _, value := range strings.Split(header, ",") {
		if strings.TrimSpace(value) == terminalProtocol {
			return true
		}
	}
	return false
}

func writeTerminalJSON(conn *websocket.Conn, value interface{}) error {
	if err := conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(value)
}

func writeTerminalMessage(conn *websocket.Conn, messageType int, payload []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(messageType, payload)
}

func writeTerminalControl(conn *websocket.Conn, messageType int, payload []byte) error {
	return conn.WriteControl(messageType, payload, time.Now().Add(terminalWriteTimeout))
}

func closeTerminalWebSocket(conn *websocket.Conn, code int, reason string) error {
	return writeTerminalControl(conn, websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func logTerminalAudit(
	level slog.Level,
	event string,
	claims *auth.TerminalClaims,
	containerID string,
	remoteAddr string,
	closeReason string,
	duration time.Duration,
	err error,
	extra ...any,
) {
	attrs := []any{
		"event", event,
		"session_id", claims.ID,
		"operator", claims.Subject,
		"sandbox_id", claims.SandboxID,
		"container_id", containerID,
		"remote_addr", remoteAddr,
		"timestamp", time.Now().UTC().Format(time.RFC3339Nano),
		"duration_ms", duration.Milliseconds(),
		"close_reason", closeReason,
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	attrs = append(attrs, extra...)
	slog.Log(context.Background(), level, "terminal audit", attrs...)
}

var _ terminalPTY = (*cubesandbox.PtyHandle)(nil)
var _ terminalFactory = (*sdkTerminalFactory)(nil)
