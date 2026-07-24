// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

const (
	terminalGrantTTL        = 60 * time.Second
	terminalIdleTimeout     = 30 * time.Minute
	terminalRequestTimeout  = 10 * time.Second
	terminalProtocol        = "cube-terminal.v1"
	terminalGrantPrefix     = "grant."
	terminalEnvdPort        = 49983
	terminalDefaultRows     = 24
	terminalDefaultCols     = 80
	terminalMaxFrameBytes   = 64 * 1024
	terminalMaxPending      = 1024
	terminalMaxActive       = 256
	connectContentType      = "application/connect+json"
	connectProtocolVersion  = "1"
	connectCompressedFlag   = byte(0x01)
	connectEndStreamFlag    = byte(0x02)
	maxConnectEnvelopeBytes = 64 * 1024 * 1024
)

type TerminalHandler struct {
	cm            CubeMasterClient
	sandboxDomain string
	grants        *terminalGrantStore
	httpClient    *http.Client
}

func NewTerminalHandler(cm CubeMasterClient, sandboxDomain string) *TerminalHandler {
	return &TerminalHandler{
		cm:            cm,
		sandboxDomain: sandboxDomain,
		grants:        newTerminalGrantStore(),
		httpClient: &http.Client{
			Transport: &http.Transport{MaxIdleConnsPerHost: 100},
		},
	}
}

func (h *TerminalHandler) RegisterAuthed(r *gin.RouterGroup) {
	r.POST("/terminal/sandboxes/:id/sessions", h.CreateSession)
}

func (h *TerminalHandler) RegisterPublic(r *gin.Engine) {
	r.GET("/sandboxes/:id/terminal", h.WebSocket)
}

type terminalCreateSessionRequest struct {
	ContainerID string `json:"containerID"`
}

type terminalCreateSessionResponse struct {
	Grant         string `json:"grant"`
	ExpiresAt     string `json:"expiresAt"`
	WebSocketPath string `json:"websocketPath"`
	ContainerID   string `json:"containerID"`
}

func (h *TerminalHandler) CreateSession(c *gin.Context) {
	sandboxID := c.Param("id")
	var req terminalCreateSessionRequest
	if c.Request.Body != nil {
		_ = c.ShouldBindJSON(&req)
	}

	target, err := h.resolveTarget(c.Request.Context(), sandboxID, req.ContainerID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTerminalSandboxNotRunning) {
			status = http.StatusConflict
		}
		if errors.Is(err, errTerminalSandboxNotFound) {
			status = http.StatusNotFound
		}
		httputil.WriteError(c, status, err.Error())
		return
	}

	operator := c.GetString("username")
	if operator == "" {
		operator = auth.UsernameFromContext(c.Request.Context())
	}
	grant, expiresAt, err := h.grants.issue(terminalGrant{
		Operator:    operator,
		SandboxID:   sandboxID,
		ContainerID: target.ContainerID,
		Domain:      target.Domain,
	})
	if err != nil {
		httputil.WriteError(c, http.StatusTooManyRequests, err.Error())
		return
	}

	slog.Info("terminal.session.grant",
		"operator", operator,
		"sandbox_id", sandboxID,
		"container_id", target.ContainerID,
		"expires_at", expiresAt.UTC().Format(time.RFC3339),
	)
	httputil.WriteJSON(c, http.StatusOK, terminalCreateSessionResponse{
		Grant:         grant,
		ExpiresAt:     expiresAt.UTC().Format(time.RFC3339),
		WebSocketPath: fmt.Sprintf("/sandboxes/%s/terminal", url.PathEscape(sandboxID)),
		ContainerID:   target.ContainerID,
	})
}

func (h *TerminalHandler) WebSocket(c *gin.Context) {
	sandboxID := c.Param("id")
	if !sameOrigin(c.Request) {
		httputil.WriteError(c, http.StatusForbidden, "terminal websocket origin is not allowed")
		return
	}
	grantToken, ok := terminalGrantFromProtocols(websocket.Subprotocols(c.Request))
	if !ok {
		httputil.WriteError(c, http.StatusUnauthorized, "missing terminal grant subprotocol")
		return
	}
	grant, err := h.grants.consume(grantToken, sandboxID)
	if err != nil {
		httputil.WriteError(c, http.StatusUnauthorized, err.Error())
		return
	}

	upgrader := websocket.Upgrader{
		Subprotocols: []string{terminalProtocol},
		CheckOrigin: func(r *http.Request) bool {
			return sameOrigin(r)
		},
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.grants.release(grant)
		return
	}
	defer conn.Close()

	sessionID := newTerminalID()
	slog.Info("terminal.session.open",
		"session_id", sessionID,
		"operator", grant.Operator,
		"sandbox_id", grant.SandboxID,
		"container_id", grant.ContainerID,
	)
	defer func() {
		h.grants.release(grant)
		slog.Info("terminal.session.close",
			"session_id", sessionID,
			"operator", grant.Operator,
			"sandbox_id", grant.SandboxID,
			"container_id", grant.ContainerID,
		)
	}()

	if err := h.bridgeEnvd(c.Request.Context(), conn, grant); err != nil {
		slog.Warn("terminal.session.error",
			"session_id", sessionID,
			"operator", grant.Operator,
			"sandbox_id", grant.SandboxID,
			"container_id", grant.ContainerID,
			"error", err,
		)
		_ = conn.WriteJSON(serverTerminalMessage{Type: "error", Message: publicTerminalError(err)})
	}
}

type terminalTarget struct {
	ContainerID string
	Domain      string
}

var (
	errTerminalSandboxNotFound   = errors.New("sandbox not found")
	errTerminalSandboxNotRunning = errors.New("sandbox is not running")
)

func (h *TerminalHandler) resolveTarget(ctx context.Context, sandboxID, requestedContainerID string) (terminalTarget, error) {
	raw, err := h.cm.GetSandbox(ctx, sandboxID, sdkInstanceType)
	if err != nil {
		return terminalTarget{}, err
	}
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return terminalTarget{}, err
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		if env.Ret.RetCode == 130404 || env.Ret.RetCode == 404 {
			return terminalTarget{}, errTerminalSandboxNotFound
		}
		return terminalTarget{}, fmt.Errorf("cubemaster error %d: %s", env.Ret.RetCode, env.Ret.RetMsg)
	}
	var items []cmSandboxDetailItem
	if err := json.Unmarshal(env.Data, &items); err != nil || len(items) == 0 {
		return terminalTarget{}, errTerminalSandboxNotFound
	}
	item := items[0]
	if sandboxStateFromInt(item.Status) != "running" {
		return terminalTarget{}, errTerminalSandboxNotRunning
	}

	containerID := requestedContainerID
	if containerID == "" {
		containerID = primaryTerminalContainer(item)
	}
	if containerID == "" {
		containerID = sandboxID
	}
	if len(item.Containers) > 0 {
		found := false
		for _, container := range item.Containers {
			if container.ContainerID == containerID {
				found = true
				if sandboxStateFromInt(container.Status) != "running" {
					return terminalTarget{}, errors.New("target container is not running")
				}
				break
			}
		}
		if !found {
			return terminalTarget{}, fmt.Errorf("container %s not found", containerID)
		}
	}

	domain := h.sandboxDomain
	if domain == "" {
		domain = sandboxDomain()
	}
	return terminalTarget{ContainerID: containerID, Domain: domain}, nil
}

func primaryTerminalContainer(item cmSandboxDetailItem) string {
	for _, container := range item.Containers {
		if container.Type == "sandbox" || container.ContainerID == item.SandboxID {
			return container.ContainerID
		}
	}
	if len(item.Containers) > 0 {
		return item.Containers[0].ContainerID
	}
	return ""
}

type terminalGrant struct {
	Token       string
	Operator    string
	SandboxID   string
	ContainerID string
	Domain      string
	ExpiresAt   time.Time
}

type terminalGrantStore struct {
	mu      sync.Mutex
	pending map[string]terminalGrant
	active  map[string]terminalGrant
}

func newTerminalGrantStore() *terminalGrantStore {
	return &terminalGrantStore{
		pending: make(map[string]terminalGrant),
		active:  make(map[string]terminalGrant),
	}
}

func (s *terminalGrantStore) issue(grant terminalGrant) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.sweepLocked(now)
	if len(s.pending) >= terminalMaxPending {
		return "", time.Time{}, errors.New("too many pending terminal sessions")
	}
	if len(s.active) >= terminalMaxActive {
		return "", time.Time{}, errors.New("too many active terminal sessions")
	}
	token := newTerminalID()
	grant.Token = token
	grant.ExpiresAt = now.Add(terminalGrantTTL)
	s.pending[token] = grant
	return token, grant.ExpiresAt, nil
}

func (s *terminalGrantStore) consume(token, sandboxID string) (terminalGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.sweepLocked(now)
	grant, ok := s.pending[token]
	if !ok {
		return terminalGrant{}, errors.New("terminal grant is invalid or already used")
	}
	if grant.ExpiresAt.Before(now) {
		delete(s.pending, token)
		return terminalGrant{}, errors.New("terminal grant expired")
	}
	if grant.SandboxID != sandboxID {
		return terminalGrant{}, errors.New("terminal grant target mismatch")
	}
	delete(s.pending, token)
	s.active[token] = grant
	return grant, nil
}

func (s *terminalGrantStore) release(grant terminalGrant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, grant.Token)
}

func (s *terminalGrantStore) sweepLocked(now time.Time) {
	for token, grant := range s.pending {
		if grant.ExpiresAt.Before(now) {
			delete(s.pending, token)
		}
	}
}

type clientTerminalMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

type serverTerminalMessage struct {
	Type     string `json:"type"`
	Status   string `json:"status,omitempty"`
	Message  string `json:"message,omitempty"`
	PID      int64  `json:"pid,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode *int64 `json:"exitCode,omitempty"`
}

func (h *TerminalHandler) bridgeEnvd(ctx context.Context, conn *websocket.Conn, grant terminalGrant) error {
	writer := &terminalWSWriter{conn: conn}
	if err := writer.writeJSON(serverTerminalMessage{Type: "status", Status: "connecting", Message: "Opening shell session..."}); err != nil {
		return err
	}
	envd := &envdTerminalClient{
		httpClient: h.httpClient,
		sandboxID:  grant.SandboxID,
		domain:     grant.Domain,
	}
	stream, err := envd.start(ctx, terminalDefaultRows, terminalDefaultCols)
	if err != nil {
		return fmt.Errorf("failed to start terminal: %w", err)
	}
	defer stream.Close()

	var pid atomic.Int64
	done := make(chan error, 2)
	activity := make(chan struct{}, 1)

	go func() {
		for {
			frame, err := stream.nextFrame()
			if err != nil {
				done <- err
				return
			}
			if frame == nil {
				done <- nil
				return
			}
			if frame.flags&connectCompressedFlag != 0 {
				done <- errors.New("compressed Connect frames are not supported")
				return
			}
			if frame.flags&connectEndStreamFlag != 0 {
				done <- connectErrorMessage(frame.payload)
				return
			}
			event, err := parseTerminalEvent(frame.payload)
			if err != nil {
				done <- err
				return
			}
			if event == nil {
				continue
			}
			switch event.Type {
			case "started":
				pid.Store(event.PID)
				_ = writer.writeJSON(serverTerminalMessage{Type: "started", PID: event.PID})
			case "output":
				if err := writer.writeJSON(serverTerminalMessage{Type: "output", Data: event.Data}); err != nil {
					done <- err
					return
				}
			case "closed":
				_ = writer.writeJSON(serverTerminalMessage{Type: "closed", ExitCode: event.ExitCode})
				done <- nil
				return
			}
			nonblockingActivity(activity)
		}
	}()

	go func() {
		conn.SetReadLimit(terminalMaxFrameBytes)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				done <- nil
				return
			}
			var msg clientTerminalMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				_ = writer.writeJSON(serverTerminalMessage{Type: "error", Message: "invalid terminal message"})
				continue
			}
			currentPID := pid.Load()
			if currentPID <= 0 {
				continue
			}
			switch msg.Type {
			case "input":
				if err := envd.sendInput(context.Background(), currentPID, msg.Data); err != nil {
					done <- err
					return
				}
			case "resize":
				if msg.Rows > 0 && msg.Cols > 0 {
					_ = envd.resize(context.Background(), currentPID, msg.Rows, msg.Cols)
				}
			case "close":
				done <- nil
				return
			}
			nonblockingActivity(activity)
		}
	}()

	idle := time.NewTimer(terminalIdleTimeout)
	defer idle.Stop()
	for {
		select {
		case err := <-done:
			currentPID := pid.Load()
			if currentPID > 0 {
				_ = envd.kill(context.Background(), currentPID)
			}
			return err
		case <-activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(terminalIdleTimeout)
		case <-idle.C:
			currentPID := pid.Load()
			if currentPID > 0 {
				_ = envd.kill(context.Background(), currentPID)
			}
			_ = writer.writeJSON(serverTerminalMessage{Type: "error", Message: "terminal session timed out after 30 minutes of inactivity"})
			return nil
		case <-ctx.Done():
			currentPID := pid.Load()
			if currentPID > 0 {
				_ = envd.kill(context.Background(), currentPID)
			}
			return nil
		}
	}
}

type terminalWSWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *terminalWSWriter) writeJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteJSON(v)
}

type terminalEvent struct {
	Type     string
	PID      int64
	Data     string
	ExitCode *int64
}

func parseTerminalEvent(payload []byte) (*terminalEvent, error) {
	var message map[string]any
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, fmt.Errorf("invalid envd event JSON: %w", err)
	}
	event, _ := message["event"].(map[string]any)
	if event == nil {
		return nil, nil
	}
	if start, _ := event["start"].(map[string]any); start != nil {
		if rawPID, ok := start["pid"].(float64); ok {
			return &terminalEvent{Type: "started", PID: int64(rawPID)}, nil
		}
	}
	if data, _ := event["data"].(map[string]any); data != nil {
		if pty, ok := data["pty"].(string); ok {
			return &terminalEvent{Type: "output", Data: pty}, nil
		}
	}
	if end, _ := event["end"].(map[string]any); end != nil {
		var exitCode *int64
		if raw, ok := end["exitCode"].(float64); ok {
			v := int64(raw)
			exitCode = &v
		} else if raw, ok := end["exit_code"].(float64); ok {
			v := int64(raw)
			exitCode = &v
		}
		return &terminalEvent{Type: "closed", ExitCode: exitCode}, nil
	}
	return nil, nil
}

type envdTerminalClient struct {
	httpClient *http.Client
	sandboxID  string
	domain     string
}

func (c *envdTerminalClient) start(ctx context.Context, rows, cols uint16) (*connectFrameStream, error) {
	payload := map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/bash",
			"args": []string{"-i", "-l"},
			"envs": map[string]string{
				"TERM":   "xterm-256color",
				"LANG":   "C.UTF-8",
				"LC_ALL": "C.UTF-8",
			},
		},
		"pty": map[string]any{"size": map[string]uint16{"rows": rows, "cols": cols}},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("Start"), bytes.NewReader(encodeConnectEnvelope(body, 0)))
	if err != nil {
		return nil, err
	}
	for k, v := range c.streamingHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if err := ensureEnvdSuccess(resp); err != nil {
		return nil, err
	}
	return &connectFrameStream{reader: resp.Body}, nil
}

func (c *envdTerminalClient) sendInput(ctx context.Context, pid int64, data string) error {
	payload := map[string]any{
		"process": map[string]int64{"pid": pid},
		"input":   map[string]string{"pty": base64.StdEncoding.EncodeToString([]byte(data))},
	}
	return c.unary(ctx, "SendInput", payload)
}

func (c *envdTerminalClient) resize(ctx context.Context, pid int64, rows, cols uint16) error {
	payload := map[string]any{
		"process": map[string]int64{"pid": pid},
		"pty":     map[string]any{"size": map[string]uint16{"rows": rows, "cols": cols}},
	}
	return c.unary(ctx, "Update", payload)
}

func (c *envdTerminalClient) kill(ctx context.Context, pid int64) error {
	payload := map[string]any{
		"process": map[string]int64{"pid": pid},
		"signal":  "SIGNAL_SIGKILL",
	}
	return c.unary(ctx, "SendSignal", payload)
}

func (c *envdTerminalClient) unary(ctx context.Context, method string, payload any) error {
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, terminalRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range c.unaryHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return ensureEnvdSuccess(resp)
}

func (c *envdTerminalClient) url(method string) string {
	return fmt.Sprintf("http://%d-%s.%s/process.Process/%s", terminalEnvdPort, c.sandboxID, c.domain, method)
}

func (c *envdTerminalClient) streamingHeaders() map[string]string {
	return map[string]string{
		"Content-Type":             connectContentType,
		"Connect-Protocol-Version": connectProtocolVersion,
		"Connect-Content-Encoding": "identity",
		"Authorization":            "Basic " + base64.StdEncoding.EncodeToString([]byte("root:")),
	}
}

func (c *envdTerminalClient) unaryHeaders() map[string]string {
	return map[string]string{
		"Content-Type":             "application/json",
		"Connect-Protocol-Version": connectProtocolVersion,
		"Authorization":            "Basic " + base64.StdEncoding.EncodeToString([]byte("root:")),
	}
}

type connectFrame struct {
	flags   byte
	payload []byte
}

type connectFrameStream struct {
	reader io.ReadCloser
	buffer []byte
}

func (s *connectFrameStream) Close() error {
	return s.reader.Close()
}

func (s *connectFrameStream) nextFrame() (*connectFrame, error) {
	for {
		if frame, err := takeConnectFrame(&s.buffer); err != nil || frame != nil {
			return frame, err
		}
		chunk := make([]byte, 32*1024)
		n, err := s.reader.Read(chunk)
		if n > 0 {
			s.buffer = append(s.buffer, chunk[:n]...)
		}
		if err == io.EOF {
			if len(s.buffer) == 0 {
				return nil, nil
			}
			return nil, errors.New("Connect stream ended with a partial frame")
		}
		if err != nil {
			return nil, err
		}
	}
}

func takeConnectFrame(buffer *[]byte) (*connectFrame, error) {
	buf := *buffer
	if len(buf) < 5 {
		return nil, nil
	}
	size := int(uint32(buf[1])<<24 | uint32(buf[2])<<16 | uint32(buf[3])<<8 | uint32(buf[4]))
	if size > maxConnectEnvelopeBytes {
		return nil, fmt.Errorf("Connect stream message too large: %d bytes", size)
	}
	if len(buf) < 5+size {
		return nil, nil
	}
	frame := &connectFrame{flags: buf[0], payload: append([]byte(nil), buf[5:5+size]...)}
	*buffer = buf[5+size:]
	return frame, nil
}

func encodeConnectEnvelope(payload []byte, flags byte) []byte {
	out := make([]byte, 0, 5+len(payload))
	out = append(out, flags)
	size := uint32(len(payload))
	out = append(out, byte(size>>24), byte(size>>16), byte(size>>8), byte(size))
	out = append(out, payload...)
	return out
}

func ensureEnvdSuccess(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return fmt.Errorf("envd returned HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("envd returned HTTP %d: %s", resp.StatusCode, detail)
}

func connectErrorMessage(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var message struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		return err
	}
	if message.Error.Message == "" {
		return nil
	}
	return errors.New(message.Error.Message)
}

func terminalGrantFromProtocols(protocols []string) (string, bool) {
	hasProtocol := false
	grantToken := ""
	for _, protocol := range protocols {
		if protocol == terminalProtocol {
			hasProtocol = true
			continue
		}
		if strings.HasPrefix(protocol, terminalGrantPrefix) {
			grantToken = strings.TrimPrefix(protocol, terminalGrantPrefix)
		}
	}
	return grantToken, hasProtocol && grantToken != ""
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func newTerminalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func nonblockingActivity(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func publicTerminalError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "envd returned HTTP") || strings.Contains(msg, "failed to start terminal") {
		return msg
	}
	return "terminal session failed"
}
