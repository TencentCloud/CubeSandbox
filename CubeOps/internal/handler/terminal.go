// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

const (
	terminalMinRows    = 2
	terminalMaxRows    = 200
	terminalMinCols    = 2
	terminalMaxCols    = 500
	terminalReadLimit  = 64 << 10
	terminalInputLimit = 32 << 10
	terminalWriteChunk = 64 << 10
	terminalWriteWait  = 10 * time.Second
	terminalPongWait   = 60 * time.Second
	terminalPingEvery  = 20 * time.Second
	terminalRateWindow = time.Second
	terminalRateEvents = 200
	terminalRateBytes  = 256 << 10
)

type terminalCubeMaster interface {
	GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error)
}

type TerminalHandler struct {
	cm          terminalCubeMaster
	terminals   *service.TerminalService
	idleTimeout time.Duration
	upgrader    websocket.Upgrader
}

func NewTerminalHandler(cm terminalCubeMaster, terminals *service.TerminalService, idleTimeout time.Duration) *TerminalHandler {
	return &TerminalHandler{
		cm:          cm,
		terminals:   terminals,
		idleTimeout: idleTimeout,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			CheckOrigin:      terminalSameOrigin,
		},
	}
}

func (h *TerminalHandler) RegisterAuthed(r *gin.RouterGroup) {
	r.POST("/sandboxes/:id/terminal/tickets", h.CreateTicket)
}

func (h *TerminalHandler) RegisterPublic(r *gin.RouterGroup) {
	r.GET("/sandboxes/:id/terminal/ws", h.Connect)
}

type createTerminalTicketRequest struct {
	ContainerID string `json:"containerID"`
	SessionID   string `json:"sessionID"`
	Rows        int    `json:"rows"`
	Cols        int    `json:"cols"`
}

type terminalContainer struct {
	Name        string `json:"name"`
	ContainerID string `json:"container_id"`
	Status      int    `json:"status"`
	Type        string `json:"type"`
	EnvdPort    int    `json:"envd_port"`
}

func (h *TerminalHandler) CreateTicket(c *gin.Context) {
	var req createTerminalTicketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	if req.Cols == 0 {
		req.Cols = 80
	}
	if !validTerminalSize(req.Rows, req.Cols) {
		httputil.WriteError(c, http.StatusBadRequest, "terminal size is outside the supported range")
		return
	}

	sandboxID := c.Param("id")
	container, err := h.validateTarget(c.Request.Context(), sandboxID, req.ContainerID)
	if err != nil {
		h.writeTargetError(c, err)
		return
	}

	ticket, err := h.terminals.IssueTicket(service.TerminalTicket{
		Username:    c.GetString("username"),
		SandboxID:   sandboxID,
		ContainerID: container.ContainerID,
		EnvdPort:    container.EnvdPort,
		SessionID:   req.SessionID,
		Rows:        req.Rows,
		Cols:        req.Cols,
	})
	if err != nil {
		if errors.Is(err, service.ErrTerminalSessionGone) {
			httputil.WriteError(c, http.StatusGone, "terminal session can no longer be reconnected")
			return
		}
		if errors.Is(err, service.ErrTerminalSessionLimit) {
			httputil.WriteError(c, http.StatusTooManyRequests, "too many pending terminal sessions")
			return
		}
		httputil.WriteError(c, http.StatusInternalServerError, "failed to create terminal ticket")
		return
	}

	slog.Info("terminal audit",
		"event", "terminal.ticket.issued",
		"operator", ticket.Username,
		"time", time.Now().UTC().Format(time.RFC3339Nano),
		"sandbox_id", ticket.SandboxID,
		"container_id", ticket.ContainerID,
		"reconnect", ticket.SessionID != "",
	)
	httputil.WriteJSON(c, http.StatusCreated, gin.H{
		"ticket":      ticket.Token,
		"expiresAt":   ticket.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"containerID": ticket.ContainerID,
		"wsPath":      "/cubeapi/v1/sandboxes/" + url.PathEscape(ticket.SandboxID) + "/terminal/ws",
	})
}

var (
	errTerminalSandboxNotFound = errors.New("sandbox not found")
	errTerminalSandboxStopped  = errors.New("sandbox is not running")
	errTerminalContainerGone   = errors.New("container not found")
	errTerminalContainerStop   = errors.New("container is not running")
	errTerminalUnavailable     = errors.New("container does not expose an envd terminal endpoint")
)

func (h *TerminalHandler) validateTarget(ctx context.Context, sandboxID, containerID string) (terminalContainer, error) {
	raw, err := h.cm.GetSandbox(ctx, sandboxID, sdkInstanceType)
	if err != nil {
		var cmErr interface{ IsNotFound() bool }
		if errors.As(err, &cmErr) && cmErr.IsNotFound() {
			return terminalContainer{}, errTerminalSandboxNotFound
		}
		return terminalContainer{}, err
	}
	var envelope struct {
		Ret  *cmRet `json:"ret"`
		Data []struct {
			SandboxID  string              `json:"sandbox_id"`
			Status     int                 `json:"status"`
			Containers []terminalContainer `json:"containers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return terminalContainer{}, err
	}
	if envelope.Ret != nil && envelope.Ret.RetCode != 0 && envelope.Ret.RetCode != 200 {
		if envelope.Ret.RetCode == 130404 || envelope.Ret.RetCode == 404 {
			return terminalContainer{}, errTerminalSandboxNotFound
		}
		return terminalContainer{}, errors.New(envelope.Ret.RetMsg)
	}
	if len(envelope.Data) == 0 {
		return terminalContainer{}, errTerminalSandboxNotFound
	}
	sandbox := envelope.Data[0]
	if sandbox.Status != 1 {
		return terminalContainer{}, errTerminalSandboxStopped
	}

	var selected *terminalContainer
	for i := range sandbox.Containers {
		container := &sandbox.Containers[i]
		if containerID != "" {
			if container.ContainerID == containerID {
				selected = container
				break
			}
			continue
		}
		if container.Type == "sandbox" || container.ContainerID == sandboxID {
			selected = container
			break
		}
	}
	if selected == nil && containerID == "" && len(sandbox.Containers) > 0 {
		selected = &sandbox.Containers[0]
	}
	if selected == nil {
		return terminalContainer{}, errTerminalContainerGone
	}
	if selected.Status != 1 {
		return terminalContainer{}, errTerminalContainerStop
	}
	if selected.EnvdPort == 0 && (selected.Type == "sandbox" || selected.ContainerID == sandboxID) {
		selected.EnvdPort = 49983
	}
	if selected.EnvdPort < 1 || selected.EnvdPort > 65535 {
		return terminalContainer{}, errTerminalUnavailable
	}
	return *selected, nil
}

func (h *TerminalHandler) writeTargetError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errTerminalSandboxNotFound), errors.Is(err, errTerminalContainerGone):
		httputil.WriteError(c, http.StatusNotFound, err.Error())
	case errors.Is(err, errTerminalSandboxStopped), errors.Is(err, errTerminalContainerStop),
		errors.Is(err, errTerminalUnavailable):
		httputil.WriteError(c, http.StatusConflict, err.Error())
	default:
		writeCMError(c, err)
	}
}

type terminalClientMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Cols int    `json:"cols,omitempty"`
}

type terminalReadResult struct {
	message terminalClientMessage
	err     error
}

type terminalInputLimiter struct {
	windowStart time.Time
	events      int
	bytes       int
}

func (l *terminalInputLimiter) allow(size int, now time.Time) bool {
	if l.windowStart.IsZero() || now.Sub(l.windowStart) >= terminalRateWindow {
		l.windowStart = now
		l.events = 0
		l.bytes = 0
	}
	if size < 0 || l.events >= terminalRateEvents || l.bytes+size > terminalRateBytes {
		return false
	}
	l.events++
	l.bytes += size
	return true
}

func (h *TerminalHandler) Connect(c *gin.Context) {
	sandboxID := c.Param("id")
	ticket, err := h.terminals.ConsumeTicket(c.Query("ticket"), sandboxID)
	if err != nil {
		httputil.WriteError(c, http.StatusUnauthorized, "invalid or expired terminal ticket")
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(terminalReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(terminalPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(terminalPongWait))
	})

	session, reconnected, err := h.terminals.Open(c.Request.Context(), ticket)
	if err != nil {
		code := "terminal_start_failed"
		if errors.Is(err, service.ErrTerminalSessionLimit) {
			code = "session_limit"
		}
		_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": code, "message": err.Error(), "recoverable": false})
		return
	}
	process := session.Process()
	slog.Info("terminal audit",
		"event", "terminal.session.opened",
		"operator", session.Username,
		"time", time.Now().UTC().Format(time.RFC3339Nano),
		"sandbox_id", session.SandboxID,
		"container_id", session.ContainerID,
		"session_id", session.ID,
		"reconnected", reconnected,
	)

	if err := writeTerminalJSON(conn, gin.H{
		"type":        "ready",
		"sessionID":   session.ID,
		"containerID": session.ContainerID,
		"reconnected": reconnected,
	}); err != nil {
		h.terminals.Detach(session)
		return
	}

	readResults := make(chan terminalReadResult, 1)
	readerDone := make(chan struct{})
	defer close(readerDone)
	go readTerminalMessages(conn, readResults, readerDone)

	ping := time.NewTicker(terminalPingEvery)
	defer ping.Stop()
	idle := time.NewTimer(h.idleTimeout)
	defer idle.Stop()
	resetIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(h.idleTimeout)
	}

	activeDisconnect := false
	abnormalDisconnect := false
	exitReason := "process_exit"
	inputLimiter := terminalInputLimiter{}

	for {
		select {
		case output, ok := <-process.Output():
			if !ok {
				exitCode, hasExitCode := process.ExitCode()
				message := gin.H{"type": "exit", "reason": "process_exit"}
				if hasExitCode {
					message["exitCode"] = exitCode
				}
				if detail := process.ErrorMessage(); detail != "" {
					message["message"] = detail
				}
				_ = writeTerminalJSON(conn, message)
				h.terminals.Finish(session)
				goto done
			}
			resetIdle()
			for len(output) > 0 {
				chunkSize := min(len(output), terminalWriteChunk)
				_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteWait))
				if err := conn.WriteMessage(websocket.BinaryMessage, output[:chunkSize]); err != nil {
					abnormalDisconnect = true
					exitReason = "connection_lost"
					h.terminals.Detach(session)
					goto done
				}
				output = output[chunkSize:]
			}

		case result := <-readResults:
			if result.err != nil {
				abnormalDisconnect = true
				exitReason = "connection_lost"
				h.terminals.Detach(session)
				goto done
			}
			message := result.message
			switch message.Type {
			case "input":
				if len(message.Data) > terminalInputLimit {
					_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": "input_too_large", "message": "terminal input is too large", "recoverable": true})
					continue
				}
				if !inputLimiter.allow(len(message.Data), time.Now()) {
					_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": "input_rate_limited", "message": "terminal input rate limit exceeded", "recoverable": true})
					continue
				}
				resetIdle()
				ctx, cancel := context.WithTimeout(c.Request.Context(), terminalWriteWait)
				err := process.SendStdin(ctx, []byte(message.Data))
				cancel()
				if err != nil {
					_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": "input_failed", "message": err.Error(), "recoverable": false})
					exitReason = "input_failed"
					h.terminals.Terminate(session.ID)
					goto done
				}
			case "resize":
				if !validTerminalSize(message.Rows, message.Cols) {
					_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": "invalid_size", "message": "terminal size is outside the supported range", "recoverable": true})
					continue
				}
				resetIdle()
				ctx, cancel := context.WithTimeout(c.Request.Context(), terminalWriteWait)
				err := process.Resize(ctx, cubesandbox.PtySize{Rows: message.Rows, Cols: message.Cols})
				cancel()
				if err != nil {
					_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": "resize_failed", "message": err.Error(), "recoverable": true})
				}
			case "ping":
				_ = writeTerminalJSON(conn, gin.H{"type": "pong"})
			case "disconnect":
				activeDisconnect = true
				exitReason = "client_disconnect"
				h.terminals.Terminate(session.ID)
				_ = writeTerminalJSON(conn, gin.H{"type": "exit", "reason": "client_disconnect"})
				goto done
			default:
				_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": "unknown_message", "message": "unknown terminal message type", "recoverable": true})
			}

		case <-idle.C:
			exitReason = "idle_timeout"
			_ = writeTerminalJSON(conn, gin.H{"type": "error", "code": "idle_timeout", "message": "terminal session closed after being idle", "recoverable": false})
			h.terminals.Terminate(session.ID)
			goto done

		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteWait))
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(terminalWriteWait)); err != nil {
				abnormalDisconnect = true
				exitReason = "connection_lost"
				h.terminals.Detach(session)
				goto done
			}

		case <-c.Request.Context().Done():
			abnormalDisconnect = true
			exitReason = "request_cancelled"
			h.terminals.Detach(session)
			goto done
		}
	}

done:
	slog.Info("terminal audit",
		"event", "terminal.session.closed",
		"operator", session.Username,
		"time", time.Now().UTC().Format(time.RFC3339Nano),
		"sandbox_id", session.SandboxID,
		"container_id", session.ContainerID,
		"session_id", session.ID,
		"reason", exitReason,
		"active", activeDisconnect,
		"abnormal", abnormalDisconnect,
	)
}

func readTerminalMessages(conn *websocket.Conn, results chan<- terminalReadResult, done <-chan struct{}) {
	publish := func(result terminalReadResult) bool {
		select {
		case results <- result:
			return true
		case <-done:
			return false
		}
	}
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			publish(terminalReadResult{err: err})
			return
		}
		if messageType != websocket.TextMessage {
			publish(terminalReadResult{err: errors.New("terminal control messages must be JSON text")})
			return
		}
		var message terminalClientMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			publish(terminalReadResult{err: errors.New("invalid terminal control message")})
			return
		}
		if !publish(terminalReadResult{message: message}) {
			return
		}
	}
}

func writeTerminalJSON(conn *websocket.Conn, value interface{}) error {
	_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteWait))
	return conn.WriteJSON(value)
}

func validTerminalSize(rows, cols int) bool {
	return rows >= terminalMinRows && rows <= terminalMaxRows &&
		cols >= terminalMinCols && cols <= terminalMaxCols
}

func terminalSameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}
