// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

const (
	headerInternalToken     = "X-Cube-Internal-Token"
	headerTerminalSandbox   = "X-Cube-Terminal-Sandbox"
	headerTerminalContainer = "X-Cube-Terminal-Container"
	headerTerminalSession   = "X-Cube-Terminal-Session"
	headerTerminalCols      = "X-Cube-Terminal-Cols"
	headerTerminalRows      = "X-Cube-Terminal-Rows"
	headerTerminalResume    = "X-Cube-Terminal-Resume-Offset"
	headerRequestID         = "X-Cube-Request-ID"

	terminalHeartbeatInterval = 30 * time.Second
	terminalCloseTimeout      = 15 * time.Second
)

type gatewayService interface {
	ConsumeTerminalGrant(context.Context, string) (*service.ConsumedTerminalGrant, *service.TerminalError)
	PrepareTerminalSession(context.Context, *service.ConsumedTerminalGrant) *service.TerminalError
	TouchTerminalSession(context.Context, string, int64, int64) error
	CloseTerminalSession(context.Context, string, string, *int32, int64, int64) error
	RunTerminalMaintenance(context.Context)
}

var _ gatewayService = (*service.TerminalService)(nil)

type Gateway struct {
	cfg      config.TerminalConfig
	service  gatewayService
	relayURL string
	relayErr error

	upgrader websocket.Upgrader
	dialer   websocket.Dialer

	draining    atomic.Bool
	active      atomic.Int64
	mu          sync.Mutex
	sessions    map[*relaySession]struct{}
	requestWG   sync.WaitGroup
	startOnce   sync.Once
	stopOnce    sync.Once
	maintCancel context.CancelFunc
}

func NewGateway(cfg config.TerminalConfig, cubeMasterAddr string, terminalService gatewayService) *Gateway {
	relayURL, relayErr := terminalRelayURL(cubeMasterAddr)
	gateway := &Gateway{
		cfg:      cfg,
		service:  terminalService,
		relayURL: relayURL,
		relayErr: relayErr,
		sessions: make(map[*relaySession]struct{}),
	}
	gateway.upgrader = websocket.Upgrader{
		HandshakeTimeout:  time.Duration(cfg.HandshakeTimeoutSec) * time.Second,
		ReadBufferSize:    32 << 10,
		WriteBufferSize:   32 << 10,
		EnableCompression: false,
		Subprotocols:      []string{Subprotocol},
		CheckOrigin:       gateway.originAllowed,
	}
	gateway.dialer = websocket.Dialer{
		HandshakeTimeout:  time.Duration(cfg.HandshakeTimeoutSec) * time.Second,
		ReadBufferSize:    32 << 10,
		WriteBufferSize:   32 << 10,
		EnableCompression: false,
		Subprotocols:      []string{Subprotocol},
	}
	return gateway
}

func (g *Gateway) Start() {
	g.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		g.maintCancel = cancel
		go g.service.RunTerminalMaintenance(ctx)
	})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.available() {
		writeGatewayError(w, http.StatusServiceUnavailable, "INTERNAL")
		return
	}
	if g.draining.Load() {
		writeGatewayError(w, http.StatusServiceUnavailable, "SERVER_DRAINING")
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		writeGatewayError(w, http.StatusBadRequest, "PROTOCOL_ERROR")
		return
	}
	if !g.originAllowed(r) {
		writeGatewayError(w, http.StatusForbidden, "FORBIDDEN")
		return
	}
	rawGrant, status, code := parseTerminalSubprotocols(websocket.Subprotocols(r))
	if code != "" {
		writeGatewayError(w, status, code)
		return
	}
	if !g.reserveReplicaSlot() {
		writeGatewayError(w, http.StatusTooManyRequests, "LIMIT_EXCEEDED")
		return
	}
	if !g.beginRequest() {
		g.releaseReplicaSlot()
		writeGatewayError(w, http.StatusServiceUnavailable, "SERVER_DRAINING")
		return
	}
	defer func() {
		g.releaseReplicaSlot()
		g.requestWG.Done()
	}()

	handshakeCtx, handshakeCancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.HandshakeTimeoutSec)*time.Second)
	defer handshakeCancel()
	grant, terminalErr := g.service.ConsumeTerminalGrant(handshakeCtx, rawGrant)
	if terminalErr != nil {
		writeGatewayError(w, terminalErr.Status, terminalErr.Code)
		return
	}
	// Drop the only raw credential reference before any downstream object is
	// registered or logged.
	rawGrant = ""

	if terminalErr := g.service.PrepareTerminalSession(handshakeCtx, grant); terminalErr != nil {
		writeGatewayError(w, terminalErr.Status, terminalErr.Code)
		return
	}

	master, terminalErr := g.dialMaster(handshakeCtx, grant)
	if terminalErr != nil {
		g.finalizeFailedAttachment(r.Context(), grant, terminalErr.Code)
		writeGatewayError(w, terminalErr.Status, terminalErr.Code)
		return
	}
	if master.Subprotocol() != Subprotocol {
		_ = master.UnderlyingConn().Close()
		g.finalizeFailedAttachment(r.Context(), grant, "INTERNAL")
		writeGatewayError(w, http.StatusBadGateway, "INTERNAL")
		return
	}

	browser, err := g.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// No normal close control: Master must treat the failed browser attach
		// as a transport loss rather than USER_CLOSED.
		_ = master.UnderlyingConn().Close()
		g.touchDetached(r.Context(), grant.SessionID, 0, 0)
		return
	}

	session := newRelaySession(g.cfg, g.service, grant, browser, master)
	if !g.register(session) {
		session.requestDrain()
		outcome := session.relay(r.Context())
		g.finalizeOutcome(r.Context(), grant, outcome)
		return
	}
	outcome := session.relay(r.Context())
	g.finalizeOutcome(r.Context(), grant, outcome)
	g.unregister(session)
}

func (g *Gateway) Shutdown(ctx context.Context) error {
	g.stopOnce.Do(func() {
		g.mu.Lock()
		g.draining.Store(true)
		active := make([]*relaySession, 0, len(g.sessions))
		for session := range g.sessions {
			active = append(active, session)
		}
		g.mu.Unlock()
		for _, session := range active {
			session.requestDrain()
		}
	})

	done := make(chan struct{})
	go func() {
		g.requestWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		if g.maintCancel != nil {
			g.maintCancel()
		}
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		for session := range g.sessions {
			session.forceClose()
		}
		g.mu.Unlock()
		if g.maintCancel != nil {
			g.maintCancel()
		}
		return ctx.Err()
	}
}

func (g *Gateway) available() bool {
	return g.cfg.Enabled && g.cfg.InternalToken != "" && g.service != nil && g.relayErr == nil && g.relayURL != ""
}

func (g *Gateway) reserveReplicaSlot() bool {
	limit := int64(g.cfg.MaxSessionsPerReplica)
	for {
		if g.draining.Load() {
			return false
		}
		current := g.active.Load()
		if current >= limit {
			return false
		}
		if g.active.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (g *Gateway) releaseReplicaSlot() { g.active.Add(-1) }

func (g *Gateway) beginRequest() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining.Load() {
		return false
	}
	g.requestWG.Add(1)
	return true
}

func (g *Gateway) register(session *relaySession) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining.Load() {
		return false
	}
	g.sessions[session] = struct{}{}
	return true
}

func (g *Gateway) unregister(session *relaySession) {
	g.mu.Lock()
	if _, ok := g.sessions[session]; ok {
		delete(g.sessions, session)
	}
	g.mu.Unlock()
}

func (g *Gateway) dialMaster(ctx context.Context, grant *service.ConsumedTerminalGrant) (*websocket.Conn, *service.TerminalError) {
	headers := make(http.Header)
	headers.Set(headerInternalToken, g.cfg.InternalToken)
	headers.Set(headerTerminalSandbox, grant.SandboxID)
	headers.Set(headerTerminalContainer, grant.ContainerID)
	headers.Set(headerTerminalSession, grant.SessionID)
	headers.Set(headerTerminalCols, strconv.FormatUint(uint64(grant.Cols), 10))
	headers.Set(headerTerminalRows, strconv.FormatUint(uint64(grant.Rows), 10))
	headers.Set(headerRequestID, uuid.NewString())
	if grant.Kind == "resume" {
		headers.Set(headerTerminalResume, strconv.FormatUint(grant.ResumeOffset, 10))
	}
	connection, response, err := g.dialer.DialContext(ctx, g.relayURL, headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		return connection, nil
	}
	status := http.StatusBadGateway
	code := "INTERNAL"
	if response != nil {
		switch response.StatusCode {
		case http.StatusNotFound:
			status, code = http.StatusNotFound, "TARGET_NOT_FOUND"
		case http.StatusServiceUnavailable:
			status, code = http.StatusServiceUnavailable, "TARGET_NOT_RUNNING"
		case http.StatusUnauthorized:
			status, code = http.StatusServiceUnavailable, "INTERNAL"
		case http.StatusBadRequest:
			status, code = http.StatusBadGateway, "INTERNAL"
		}
	}
	return nil, &service.TerminalError{Status: status, Code: code, Cause: err}
}

func (g *Gateway) finalizeFailedAttachment(ctx context.Context, grant *service.ConsumedTerminalGrant, reason string) {
	if grant.Kind == "resume" {
		g.touchDetached(ctx, grant.SessionID, 0, 0)
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := g.service.CloseTerminalSession(closeCtx, grant.SessionID, reason, nil, 0, 0); err != nil {
		slog.Warn("terminal session pre-open audit close failed", "session_id", grant.SessionID, "error", err)
	}
}

func (g *Gateway) finalizeOutcome(ctx context.Context, grant *service.ConsumedTerminalGrant, outcome relayOutcome) {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if outcome.Detached {
		if err := g.service.TouchTerminalSession(finalCtx, grant.SessionID, outcome.BytesIn, outcome.BytesOut); err != nil {
			slog.Warn("terminal detached audit heartbeat failed", "session_id", grant.SessionID, "error", err)
		}
		return
	}
	reason := outcome.CloseReason
	if reason == "" {
		reason = "INTERNAL"
	}
	if err := g.service.CloseTerminalSession(finalCtx, grant.SessionID, reason, outcome.ExitCode, outcome.BytesIn, outcome.BytesOut); err != nil {
		slog.Warn("terminal close audit update failed", "session_id", grant.SessionID, "error", err)
	}
}

func (g *Gateway) touchDetached(ctx context.Context, sessionID string, bytesIn, bytesOut int64) {
	touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := g.service.TouchTerminalSession(touchCtx, sessionID, bytesIn, bytesOut); err != nil {
		slog.Warn("terminal detached audit heartbeat failed", "session_id", sessionID, "error", err)
	}
}

func (g *Gateway) originAllowed(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return false
	}
	origin, err := normalizeOrigin(origins[0])
	if err != nil {
		return false
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		requestScheme = forwarded
	}
	requestOrigin, err := normalizeOrigin(requestScheme + "://" + r.Host)
	if err == nil && strings.EqualFold(origin, requestOrigin) {
		return true
	}
	for _, allowed := range g.cfg.AllowedOrigins {
		normalized, err := normalizeOrigin(allowed)
		if err == nil && strings.EqualFold(origin, normalized) {
			return true
		}
	}
	return false
}

func normalizeOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid WebSocket origin")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func parseTerminalSubprotocols(protocols []string) (rawGrant string, status int, code string) {
	terminalCount := 0
	grantCount := 0
	for _, protocol := range protocols {
		switch {
		case protocol == Subprotocol:
			terminalCount++
		case strings.HasPrefix(protocol, GrantPrefix):
			grantCount++
			rawGrant = strings.TrimPrefix(protocol, GrantPrefix)
		}
	}
	if terminalCount != 1 {
		return "", http.StatusBadRequest, "PROTOCOL_ERROR"
	}
	if grantCount != 1 || rawGrant == "" {
		return "", http.StatusUnauthorized, "GRANT_INVALID"
	}
	if len(protocols) != 2 {
		return "", http.StatusBadRequest, "PROTOCOL_ERROR"
	}
	return rawGrant, 0, ""
}

func terminalRelayURL(cubeMasterAddr string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(cubeMasterAddr))
	if err != nil || parsed.Host == "" {
		return "", errors.New("invalid CubeMaster address")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", errors.New("CubeMaster address must use http or https")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/terminal/relay"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func writeGatewayError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

type relaySession struct {
	cfg     config.TerminalConfig
	service gatewayService
	grant   *service.ConsumedTerminalGrant
	browser *websocket.Conn
	master  *websocket.Conn

	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
	draining  atomic.Bool
	drainCh   chan struct{}
	drainOnce sync.Once
	forceOnce sync.Once

	stateMu     sync.Mutex
	closeReason string
	exitCode    *int32
}

type pumpResult struct {
	err         error
	waitForPeer bool
	detached    bool
	closeReason string
}

type relayOutcome struct {
	Detached    bool
	CloseReason string
	ExitCode    *int32
	BytesIn     int64
	BytesOut    int64
}

func newRelaySession(cfg config.TerminalConfig, terminalService gatewayService, grant *service.ConsumedTerminalGrant, browser, master *websocket.Conn) *relaySession {
	return &relaySession{
		cfg:     cfg,
		service: terminalService,
		grant:   grant,
		browser: browser,
		master:  master,
		drainCh: make(chan struct{}),
	}
}

func (s *relaySession) relay(parent context.Context) relayOutcome {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.browser.SetReadLimit(int64(s.cfg.MaxFrameBytes + 1))
	s.master.SetReadLimit(int64(s.cfg.MaxFrameBytes + 1))
	_ = s.browser.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.PingIntervalSeconds+s.cfg.PongTimeoutSeconds) * time.Second))
	s.browser.SetPongHandler(func(string) error {
		return s.browser.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.PingIntervalSeconds+s.cfg.PongTimeoutSeconds) * time.Second))
	})

	results := make(chan pumpResult, 2)
	go func() { results <- s.pumpBrowserToMaster(ctx) }()
	go func() { results <- s.pumpMasterToBrowser(ctx) }()

	pingTicker := time.NewTicker(time.Duration(s.cfg.PingIntervalSeconds) * time.Second)
	heartbeatTicker := time.NewTicker(terminalHeartbeatInterval)
	defer pingTicker.Stop()
	defer heartbeatTicker.Stop()

	first, gotFirst := pumpResult{}, false
	for !gotFirst {
		select {
		case first = <-results:
			gotFirst = true
		case <-pingTicker.C:
			deadline := time.Now().Add(time.Duration(s.cfg.WriteDeadlineSeconds) * time.Second)
			if err := s.browser.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				_ = s.master.UnderlyingConn().Close()
				_ = s.browser.UnderlyingConn().Close()
			}
		case <-heartbeatTicker.C:
			s.flushHeartbeat(ctx)
		case <-s.drainCh:
			s.draining.Store(true)
			// An abnormal internal close preserves Cubelet DetachedGrace. The
			// sole browser data writer emits SERVER_DRAINING when it unblocks.
			_ = s.master.UnderlyingConn().Close()
		case <-ctx.Done():
			_ = s.master.UnderlyingConn().Close()
			_ = s.browser.UnderlyingConn().Close()
		}
	}

	var second pumpResult
	if first.waitForPeer {
		timer := time.NewTimer(terminalCloseTimeout)
		select {
		case second = <-results:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			cancel()
			_ = s.master.UnderlyingConn().Close()
			_ = s.browser.UnderlyingConn().Close()
			second = <-results
		}
	} else {
		cancel()
		_ = s.master.UnderlyingConn().Close()
		_ = s.browser.UnderlyingConn().Close()
		second = <-results
	}
	cancel()
	_ = s.master.Close()
	_ = s.browser.Close()

	closeReason, exitCode := s.finalState()
	detached := first.detached || second.detached
	if closeReason != "" {
		detached = false
	}
	if closeReason == "" {
		if first.closeReason != "" {
			closeReason = first.closeReason
		}
		if closeReason == "" && second.closeReason != "" {
			closeReason = second.closeReason
		}
	}
	if closeReason != "" && closeReason != "SERVER_DRAINING" {
		detached = false
	}
	if closeReason == "SERVER_DRAINING" {
		detached = true
	}
	if err := errors.Join(first.err, second.err); err != nil && !isExpectedGatewayError(err) {
		logUnexpectedGatewayError(s.grant.SessionID, err)
	}
	return relayOutcome{
		Detached:    detached,
		CloseReason: closeReason,
		ExitCode:    exitCode,
		BytesIn:     s.bytesIn.Load(),
		BytesOut:    s.bytesOut.Load(),
	}
}

func (s *relaySession) pumpBrowserToMaster(ctx context.Context) pumpResult {
	for {
		messageType, message, err := s.browser.ReadMessage()
		if err != nil {
			if errors.Is(err, websocket.ErrReadLimit) {
				// Trigger Master/Cubelet's authoritative PROTOCOL_ERROR cleanup
				// without allocating an attacker-controlled oversized payload.
				_ = s.writeMaster(websocket.TextMessage, []byte("invalid terminal frame"))
				writeCloseControl(s.browser, websocket.CloseMessageTooBig, "terminal frame too large")
				return pumpResult{waitForPeer: true, closeReason: "PROTOCOL_ERROR"}
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseNormalClosure {
				writeCloseControl(s.master, websocket.CloseNormalClosure, "terminal closed")
				return pumpResult{waitForPeer: true, closeReason: "USER_CLOSED"}
			}
			_ = s.master.UnderlyingConn().Close()
			return pumpResult{err: err, detached: true}
		}

		info, validationErr := ValidateClientMessage(messageType, message, s.cfg.MaxFrameBytes)
		if validationErr != nil {
			// Forward the bounded offending frame so CubeMaster maps it to the
			// typed Cubelet PROTOCOL_ERROR close rather than DetachedGrace.
			if err := s.writeMaster(messageType, message); err != nil {
				_ = s.master.UnderlyingConn().Close()
				return pumpResult{err: err, closeReason: "PROTOCOL_ERROR"}
			}
			return pumpResult{waitForPeer: true, closeReason: "PROTOCOL_ERROR"}
		}
		if err := s.writeMaster(messageType, message); err != nil {
			writeCloseControl(s.browser, websocket.CloseTryAgainLater, "terminal upstream is slow")
			_ = s.master.UnderlyingConn().Close()
			return pumpResult{err: err, closeReason: "SLOW_PRODUCER"}
		}
		s.bytesIn.Add(info.StdinBytes)
		select {
		case <-ctx.Done():
			return pumpResult{detached: true}
		default:
		}
	}
}

func (s *relaySession) pumpMasterToBrowser(ctx context.Context) pumpResult {
	for {
		messageType, message, err := s.master.ReadMessage()
		if err != nil {
			if s.draining.Load() {
				_ = s.writeBrowserStatus("close", "SERVER_DRAINING")
				writeCloseControl(s.browser, websocket.CloseServiceRestart, "terminal server draining")
				return pumpResult{err: err, detached: true, closeReason: "SERVER_DRAINING"}
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseNormalClosure {
				writeCloseControl(s.browser, websocket.CloseNormalClosure, "terminal closed")
				return pumpResult{}
			}
			_ = s.writeBrowserStatus("error", "INTERNAL")
			writeCloseControl(s.browser, websocket.CloseInternalServerErr, "terminal relay failed")
			return pumpResult{err: err, detached: true}
		}

		info, validationErr := ValidateServerMessage(messageType, message, s.cfg.MaxFrameBytes)
		if validationErr != nil {
			_ = s.writeBrowserStatus("error", "INTERNAL")
			writeCloseControl(s.browser, websocket.CloseInternalServerErr, "invalid terminal response")
			_ = s.master.UnderlyingConn().Close()
			return pumpResult{err: validationErr, closeReason: "INTERNAL"}
		}
		if info.Type == "opened" && info.SessionID != s.grant.SessionID {
			_ = s.writeBrowserStatus("error", "INTERNAL")
			writeCloseControl(s.browser, websocket.CloseInternalServerErr, "terminal session mismatch")
			_ = s.master.UnderlyingConn().Close()
			return pumpResult{err: errors.New("terminal opened session id mismatch"), closeReason: "INTERNAL"}
		}
		s.observeServerFrame(info)
		if info.StdoutBytes > int64(s.cfg.StdoutPendingBytes) {
			writeCloseControl(s.browser, websocket.CloseTryAgainLater, "terminal consumer is slow")
			_ = s.master.UnderlyingConn().Close()
			return pumpResult{closeReason: "SLOW_CONSUMER"}
		}
		if err := s.writeBrowser(messageType, message); err != nil {
			writeCloseControl(s.browser, websocket.CloseTryAgainLater, "terminal consumer is slow")
			_ = s.master.UnderlyingConn().Close()
			return pumpResult{err: err, closeReason: "SLOW_CONSUMER"}
		}
		s.bytesOut.Add(info.StdoutBytes)
		select {
		case <-ctx.Done():
			return pumpResult{detached: true}
		default:
		}
	}
}

func (s *relaySession) writeMaster(messageType int, message []byte) error {
	if err := s.master.SetWriteDeadline(time.Now().Add(time.Duration(s.cfg.WriteDeadlineSeconds) * time.Second)); err != nil {
		return err
	}
	return s.master.WriteMessage(messageType, message)
}

func (s *relaySession) writeBrowser(messageType int, message []byte) error {
	if err := s.browser.SetWriteDeadline(time.Now().Add(time.Duration(s.cfg.WriteDeadlineSeconds) * time.Second)); err != nil {
		return err
	}
	return s.browser.WriteMessage(messageType, message)
}

func (s *relaySession) writeBrowserStatus(kind, value string) error {
	var (
		message []byte
		err     error
	)
	if kind == "close" {
		message, err = EncodeCloseStatus(value)
	} else {
		message, err = EncodeErrorStatus(value)
	}
	if err != nil {
		return err
	}
	return s.writeBrowser(websocket.BinaryMessage, message)
}

func (s *relaySession) observeServerFrame(info ServerFrameInfo) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if info.ExitCode != nil {
		value := *info.ExitCode
		s.exitCode = &value
	}
	if info.CloseReason != "" {
		s.closeReason = info.CloseReason
	}
}

func (s *relaySession) finalState() (string, *int32) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	var exitCode *int32
	if s.exitCode != nil {
		value := *s.exitCode
		exitCode = &value
	}
	return s.closeReason, exitCode
}

func (s *relaySession) flushHeartbeat(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	if err := s.service.TouchTerminalSession(ctx, s.grant.SessionID, s.bytesIn.Load(), s.bytesOut.Load()); err != nil {
		slog.Warn("terminal audit heartbeat failed", "session_id", s.grant.SessionID, "error", err)
	}
}

func (s *relaySession) requestDrain() {
	s.drainOnce.Do(func() { close(s.drainCh) })
}

func (s *relaySession) forceClose() {
	s.forceOnce.Do(func() {
		s.draining.Store(true)
		_ = s.master.UnderlyingConn().Close()
		_ = s.browser.UnderlyingConn().Close()
	})
}

func writeCloseControl(connection *websocket.Conn, code int, reason string) {
	if connection == nil {
		return
	}
	deadline := time.Now().Add(time.Second)
	_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline)
}

func isExpectedGatewayError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return true
	}
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr)
}

func logUnexpectedGatewayError(sessionID string, err error) {
	// WebSocket errors can contain a peer-controlled close reason. Never pass
	// their text (or another transport error's text) to the logger because it
	// may contain a grant, credential, or terminal payload supplied by a client.
	errorKind := "transport"
	if errors.Is(err, context.DeadlineExceeded) {
		errorKind = "timeout"
	}
	slog.Warn("terminal gateway relay ended with error",
		"session_id", sessionID,
		"error_kind", errorKind,
	)
}

func (o relayOutcome) String() string {
	return fmt.Sprintf("detached=%t reason=%s bytes_in=%d bytes_out=%d", o.Detached, o.CloseReason, o.BytesIn, o.BytesOut)
}
