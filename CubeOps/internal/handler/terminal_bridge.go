// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	internalhttputil "github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

const (
	terminalProtocol               = "cube-terminal.v1"
	terminalGrantProtocolPrefix    = "cube-terminal.grant."
	terminalGatewayTokenHeader     = "X-Cube-Terminal-Gateway"
	maxTerminalSessionRequestBytes = 4 * 1024
	maxTerminalFrameBytes          = 64 * 1024
	terminalIdleTimeout            = 30 * time.Minute
)

type createTerminalSessionRequest struct {
	ContainerID string `json:"containerId"`
	Cols        uint16 `json:"cols"`
	Rows        uint16 `json:"rows"`
}

type terminalGateway struct {
	masterURL      string
	token          string
	allowedOrigins string
	grants         *terminalGrantStore
}

func newTerminalGateway(masterURL, token, allowedOrigins string) *terminalGateway {
	return &terminalGateway{
		masterURL:      strings.TrimRight(masterURL, "/"),
		token:          strings.TrimSpace(token),
		allowedOrigins: allowedOrigins,
		grants:         newTerminalGrantStore(defaultTerminalLimits()),
	}
}

// CreateTerminalSession validates the authenticated CubeOps user and target,
// then issues a short-lived, single-use grant bound to an HttpOnly cookie.
func (h *SDKHandler) CreateTerminalSession(c *gin.Context) {
	if h.terminal == nil || h.terminal.token == "" {
		internalhttputil.WriteError(c, http.StatusServiceUnavailable, "web terminal is not configured")
		return
	}
	if c.Request.ContentLength > maxTerminalSessionRequestBytes {
		internalhttputil.WriteError(c, http.StatusRequestEntityTooLarge, "terminal session request is too large")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTerminalSessionRequestBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var body createTerminalSessionRequest
	if err := decoder.Decode(&body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			internalhttputil.WriteError(c, http.StatusRequestEntityTooLarge, "terminal session request is too large")
		} else {
			internalhttputil.WriteError(c, http.StatusBadRequest, "invalid terminal session request")
		}
		return
	}
	if err := ensureTerminalJSONEOF(decoder); err != nil {
		internalhttputil.WriteError(c, http.StatusBadRequest, "invalid terminal session request")
		return
	}
	containerID, err := h.resolveTerminalContainer(c, c.Param("id"), body.ContainerID)
	if err != nil {
		return
	}
	target := terminalTarget{sandboxID: c.Param("id"), containerID: containerID, cols: body.Cols, rows: body.Rows}
	issued, err := h.terminal.grants.issue(c.GetString("username"), target)
	if err != nil {
		switch err {
		case errTerminalInvalidTarget:
			internalhttputil.WriteError(c, http.StatusBadRequest, "invalid terminal dimensions or target")
		case errTerminalPendingLimit:
			c.Header("Retry-After", "30")
			internalhttputil.WriteError(c, http.StatusTooManyRequests, "too many pending terminal grants")
		default:
			internalhttputil.WriteError(c, http.StatusInternalServerError, "failed to create terminal session")
		}
		return
	}
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") || strings.HasPrefix(c.GetHeader("Origin"), "https://")
	http.SetCookie(c.Writer, &http.Cookie{
		Name: issued.cookieName, Value: issued.cookieValue, Path: "/", MaxAge: int(issued.expiresIn.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
	slog.Info("terminal grant issued", "session_id", issued.sessionID, "sandbox_id", target.sandboxID, "container_id", target.containerID, "principal", c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{
		"sessionId": issued.sessionID, "grant": issued.token,
		"expiresInSeconds": int(issued.expiresIn.Seconds()), "protocol": terminalProtocol,
	})
}

func ensureTerminalJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (h *SDKHandler) resolveTerminalContainer(c *gin.Context, sandboxID, requested string) (string, error) {
	raw, err := h.cm.GetSandbox(c.Request.Context(), sandboxID, sdkInstanceType)
	if err != nil {
		writeCMError(c, err)
		return "", err
	}
	var envelope cmEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		internalhttputil.WriteError(c, http.StatusBadGateway, "invalid sandbox response")
		return "", err
	}
	if envelope.Ret != nil && envelope.Ret.RetCode != 0 && envelope.Ret.RetCode != 200 {
		internalhttputil.WriteError(c, http.StatusNotFound, envelope.Ret.RetMsg)
		return "", errors.New("sandbox unavailable")
	}
	var sandboxes []cmSandboxDetailItem
	if err := json.Unmarshal(envelope.Data, &sandboxes); err != nil || len(sandboxes) == 0 {
		internalhttputil.WriteError(c, http.StatusNotFound, "sandbox not found")
		return "", errors.New("sandbox not found")
	}
	sandbox := sandboxes[0]
	if sandbox.Status != 1 {
		internalhttputil.WriteError(c, http.StatusConflict, "sandbox is not running")
		return "", errors.New("sandbox not running")
	}
	if requested != "" {
		for _, container := range sandbox.Containers {
			if container.ContainerID == requested {
				return requested, nil
			}
		}
		internalhttputil.WriteError(c, http.StatusBadRequest, "container does not belong to sandbox")
		return "", errors.New("container not found")
	}
	for _, container := range sandbox.Containers {
		if container.Type == "sandbox" || container.ContainerID == sandbox.SandboxID {
			return container.ContainerID, nil
		}
	}
	if len(sandbox.Containers) > 0 {
		return sandbox.Containers[0].ContainerID, nil
	}
	internalhttputil.WriteError(c, http.StatusConflict, "sandbox has no terminal container")
	return "", errors.New("sandbox has no containers")
}

var terminalUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   32 * 1024,
	WriteBufferSize:  32 * 1024,
	Subprotocols:     []string{terminalProtocol},
	CheckOrigin:      func(*http.Request) bool { return true }, // validated before Upgrade
}

// ProxyTerminal consumes a one-time CubeOps grant and relays the browser
// WebSocket directly to CubeMaster. CubeAPI is intentionally not involved.
func (h *SDKHandler) ProxyTerminal(c *gin.Context) {
	if h.terminal == nil || h.terminal.token == "" {
		internalhttputil.WriteError(c, http.StatusServiceUnavailable, "web terminal is not configured")
		return
	}
	if !terminalOriginAllowed(c.Request, h.terminal.allowedOrigins) {
		slog.Warn("terminal session denied", "sandbox_id", c.Param("id"), "reason", "origin_not_allowed")
		internalhttputil.WriteError(c, http.StatusForbidden, "terminal WebSocket origin is not allowed")
		return
	}
	if !terminalProtocolOffered(c.Request.Header, terminalProtocol) {
		slog.Warn("terminal session denied", "sandbox_id", c.Param("id"), "reason", "protocol_missing")
		internalhttputil.WriteError(c, http.StatusBadRequest, "terminal WebSocket protocol is required")
		return
	}
	grant := terminalGrantFromProtocols(c.Request.Header)
	lease, err := h.terminal.grants.consume(grant, terminalCookies(c.Request))
	if err != nil {
		slog.Warn("terminal session denied", "sandbox_id", c.Param("id"), "reason", err)
		status := http.StatusUnauthorized
		if err == errTerminalActiveLimit {
			status = http.StatusTooManyRequests
		}
		internalhttputil.WriteError(c, status, "terminal grant is invalid, expired, or limited")
		return
	}
	if lease.target.sandboxID != c.Param("id") {
		slog.Warn("terminal session denied", "sandbox_id", c.Param("id"), "reason", "sandbox_mismatch")
		lease.release()
		internalhttputil.WriteError(c, http.StatusUnauthorized, "terminal grant does not match sandbox")
		return
	}
	upgrader := terminalUpgrader
	// Repeat the validated policy inside gorilla's upgrader as defense in
	// depth if this handshake code is moved or reused in the future.
	upgrader.CheckOrigin = func(request *http.Request) bool {
		return terminalOriginAllowed(request, h.terminal.allowedOrigins)
	}
	connection, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		lease.release()
		return
	}
	defer connection.Close()
	defer lease.release()
	if err := h.terminal.relay(c.Request, connection, lease); err != nil {
		slog.Warn("terminal relay closed", "session_id", lease.sessionID, "sandbox_id", lease.target.sandboxID, "container_id", lease.target.containerID, "principal", lease.principal, "error", err)
	} else {
		slog.Info("terminal relay closed", "session_id", lease.sessionID, "sandbox_id", lease.target.sandboxID, "container_id", lease.target.containerID, "principal", lease.principal)
	}
}

func (gateway *terminalGateway) relay(request *http.Request, browser *websocket.Conn, lease *terminalSessionLease) error {
	backendURL, err := terminalBackendURL(gateway.masterURL)
	if err != nil {
		return err
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	header := http.Header{}
	header.Set(terminalGatewayTokenHeader, gateway.token)
	backend, response, err := dialer.DialContext(request.Context(), backendURL, header)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		_ = browser.WriteJSON(gin.H{"v": 1, "type": "error", "code": "BACKEND_UNAVAILABLE", "message": "terminal backend is unavailable", "retryable": true})
		return err
	}
	defer backend.Close()
	browser.SetReadLimit(maxTerminalFrameBytes)
	backend.SetReadLimit(maxTerminalFrameBytes)
	open := gin.H{
		"v": 1, "type": "open", "requestId": fmt.Sprintf("cubeops-terminal-%d", time.Now().UnixNano()),
		"sessionId": lease.sessionID, "sandboxId": lease.target.sandboxID, "containerId": lease.target.containerID,
		"cols": lease.target.cols, "rows": lease.target.rows,
	}
	if err := backend.WriteJSON(open); err != nil {
		return err
	}
	return relayTerminalFrames(browser, backend)
}

type terminalFrame struct {
	messageType int
	data        []byte
}

func relayTerminalFrames(browser, backend *websocket.Conn) error {
	done := make(chan struct{})
	defer close(done)
	browserFrames, browserErrors := readTerminalFrames(browser, done)
	backendFrames, backendErrors := readTerminalFrames(backend, done)
	timer := time.NewTimer(terminalIdleTimeout)
	defer timer.Stop()
	resetIdle := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(terminalIdleTimeout)
	}
	for {
		select {
		case frame, ok := <-browserFrames:
			if !ok {
				return terminalReadError(<-browserErrors)
			}
			if frame.messageType == websocket.BinaryMessage || (frame.messageType == websocket.TextMessage && terminalTextCountsAsActivity(frame.data)) {
				resetIdle()
			}
			if err := backend.WriteMessage(frame.messageType, frame.data); err != nil {
				return err
			}
		case frame, ok := <-backendFrames:
			if !ok {
				return terminalReadError(<-backendErrors)
			}
			if frame.messageType == websocket.BinaryMessage {
				resetIdle()
			}
			if err := browser.WriteMessage(frame.messageType, frame.data); err != nil {
				return err
			}
		case <-timer.C:
			_ = browser.WriteJSON(gin.H{"v": 1, "type": "error", "code": "IDLE_TIMEOUT", "message": "terminal session timed out", "retryable": true})
			return errors.New("terminal idle timeout")
		}
	}
}

func terminalReadError(err error) error {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return nil
	}
	return err
}

func readTerminalFrames(connection *websocket.Conn, done <-chan struct{}) (<-chan terminalFrame, <-chan error) {
	frames := make(chan terminalFrame)
	errorsOut := make(chan error, 1)
	go func() {
		defer close(frames)
		for {
			messageType, data, err := connection.ReadMessage()
			if err != nil {
				select {
				case errorsOut <- err:
				case <-done:
				}
				return
			}
			if len(data) > maxTerminalFrameBytes {
				select {
				case errorsOut <- errors.New("terminal frame exceeds limit"):
				case <-done:
				}
				return
			}
			select {
			case frames <- terminalFrame{messageType: messageType, data: data}:
			case <-done:
				return
			}
		}
	}()
	return frames, errorsOut
}

func terminalTextCountsAsActivity(data []byte) bool {
	var control struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(data, &control) == nil && control.Type == "resize"
}

func terminalBackendURL(masterURL string) (string, error) {
	target, err := url.Parse(masterURL)
	if err != nil || target.Host == "" {
		return "", errors.New("invalid CubeMaster URL")
	}
	switch target.Scheme {
	case "http":
		target.Scheme = "ws"
	case "https":
		target.Scheme = "wss"
	default:
		return "", errors.New("invalid CubeMaster URL scheme")
	}
	target.Path = path.Join(target.Path, "/cube/sandbox/terminal")
	// CubeMaster's internal terminal endpoint never accepts query parameters.
	// Clear caller-supplied URL metadata so configuration cannot smuggle data
	// into the authenticated backend handshake.
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	return target.String(), nil
}

func terminalOriginAllowed(request *http.Request, configured string) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return false
	}
	if configured != "" {
		for _, allowed := range strings.Split(configured, ",") {
			if strings.TrimSpace(allowed) == origin {
				return true
			}
		}
		return false
	}
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == request.Host
}

func terminalProtocolOffered(header http.Header, expected string) bool {
	for _, protocol := range strings.Split(header.Get("Sec-WebSocket-Protocol"), ",") {
		if strings.TrimSpace(protocol) == expected {
			return true
		}
	}
	return false
}

func terminalGrantFromProtocols(header http.Header) string {
	for _, protocol := range strings.Split(header.Get("Sec-WebSocket-Protocol"), ",") {
		if token, ok := strings.CutPrefix(strings.TrimSpace(protocol), terminalGrantProtocolPrefix); ok {
			return token
		}
	}
	return ""
}

func terminalCookies(request *http.Request) map[string]string {
	cookies := make(map[string]string)
	for _, cookie := range request.Cookies() {
		cookies[cookie.Name] = cookie.Value
	}
	return cookies
}
