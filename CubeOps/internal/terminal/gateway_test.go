// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

func TestLogUnexpectedGatewayErrorDoesNotLogPeerText(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	peerControlled := "cube-grant.sensitive-marker terminal-payload-marker"
	logUnexpectedGatewayError(testSessionID, errors.New(peerControlled))

	logged := output.String()
	if strings.Contains(logged, peerControlled) || strings.Contains(logged, "sensitive-marker") || strings.Contains(logged, "terminal-payload-marker") {
		t.Fatalf("gateway log contains peer-controlled error text: %q", logged)
	}
	if !strings.Contains(logged, "error_kind=transport") || !strings.Contains(logged, "session_id="+testSessionID) {
		t.Fatalf("gateway log is missing safe diagnostic fields: %q", logged)
	}
}

const (
	testRawGrant  = "AAECAwQFBgcICQoLDA0ODw"
	testSessionID = "de305d54-75b4-431b-adb2-eb6b9e546014"
)

type terminalAuditCall struct {
	sessionID string
	reason    string
	exitCode  *int32
	bytesIn   int64
	bytesOut  int64
}

type fakeGatewayService struct {
	mu sync.Mutex

	grant      *service.ConsumedTerminalGrant
	consumeErr *service.TerminalError
	prepareErr *service.TerminalError

	consumeCalls int
	consumeRaw   string
	consumeStart chan struct{}
	consumeAllow chan struct{}
	prepareCalls int
	touches      []terminalAuditCall
	closes       []terminalAuditCall
	touchStart   chan struct{}
	touchAllow   chan struct{}
}

func (f *fakeGatewayService) ConsumeTerminalGrant(ctx context.Context, raw string) (*service.ConsumedTerminalGrant, *service.TerminalError) {
	f.mu.Lock()
	f.consumeCalls++
	f.consumeRaw = raw
	var grant *service.ConsumedTerminalGrant
	if f.grant != nil {
		copy := *f.grant
		grant = &copy
	}
	consumeErr := f.consumeErr
	consumeStart := f.consumeStart
	consumeAllow := f.consumeAllow
	f.mu.Unlock()
	if consumeStart != nil {
		select {
		case consumeStart <- struct{}{}:
		default:
		}
	}
	if consumeAllow != nil {
		select {
		case <-consumeAllow:
		case <-ctx.Done():
			return nil, &service.TerminalError{Status: http.StatusRequestTimeout, Code: "INTERNAL", Cause: ctx.Err()}
		}
	}
	return grant, consumeErr
}

func (f *fakeGatewayService) PrepareTerminalSession(context.Context, *service.ConsumedTerminalGrant) *service.TerminalError {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls++
	return f.prepareErr
}

func (f *fakeGatewayService) TouchTerminalSession(ctx context.Context, sessionID string, bytesIn, bytesOut int64) error {
	f.mu.Lock()
	touchStart := f.touchStart
	touchAllow := f.touchAllow
	f.mu.Unlock()
	if touchStart != nil {
		select {
		case touchStart <- struct{}{}:
		default:
		}
	}
	if touchAllow != nil {
		select {
		case <-touchAllow:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.touches = append(f.touches, terminalAuditCall{sessionID: sessionID, bytesIn: bytesIn, bytesOut: bytesOut})
	f.mu.Unlock()
	return nil
}

func (f *fakeGatewayService) CloseTerminalSession(_ context.Context, sessionID, reason string, exitCode *int32, bytesIn, bytesOut int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var copiedExit *int32
	if exitCode != nil {
		value := *exitCode
		copiedExit = &value
	}
	f.closes = append(f.closes, terminalAuditCall{
		sessionID: sessionID,
		reason:    reason,
		exitCode:  copiedExit,
		bytesIn:   bytesIn,
		bytesOut:  bytesOut,
	})
	return nil
}

func (f *fakeGatewayService) RunTerminalMaintenance(ctx context.Context) { <-ctx.Done() }

func (f *fakeGatewayService) auditSnapshot() (int, string, int, []terminalAuditCall, []terminalAuditCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consumeCalls, f.consumeRaw, f.prepareCalls,
		append([]terminalAuditCall(nil), f.touches...), append([]terminalAuditCall(nil), f.closes...)
}

type observedMasterHeaders struct {
	tokenMatched bool
	sandboxID    string
	containerID  string
	sessionID    string
	cols         string
	rows         string
	resume       string
	requestIDSet bool
	userAuthSet  bool
}

const cubeMasterRequestIDHeader = "X-RequestID"

func TestGatewayRelaysTypedFramesAndWritesAudit(t *testing.T) {
	cfg := gatewayTestConfig()
	grant := gatewayTestGrant("open")
	terminalService := &fakeGatewayService{grant: grant}
	headersCh := make(chan observedMasterHeaders, 1)
	masterErrCh := make(chan error, 1)

	masterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersCh <- observeMasterHeaders(r, cfg.InternalToken)
		connection, err := testMasterUpgrader().Upgrade(w, r, nil)
		if err != nil {
			masterErrCh <- err
			return
		}
		defer connection.Close()
		if err := connection.WriteMessage(websocket.BinaryMessage, statusMessage(`{"type":"opened","sessionId":"`+grant.SessionID+`","replay":{"from":0,"truncated":false}}`)); err != nil {
			masterErrCh <- err
			return
		}
		messageType, message, err := connection.ReadMessage()
		if err != nil {
			masterErrCh <- err
			return
		}
		if messageType != websocket.BinaryMessage || string(message) != string(append([]byte{ChannelStdin}, []byte("echo ready\n")...)) {
			masterErrCh <- errors.New("unexpected browser frame at internal relay")
			return
		}
		for _, message := range [][]byte{
			append([]byte{ChannelStdout}, []byte("ready\r\n")...),
			statusMessage(`{"type":"exit","exitCode":17}`),
			statusMessage(`{"type":"close","reason":"RUNTIME_EXITED"}`),
		} {
			if err := connection.WriteMessage(websocket.BinaryMessage, message); err != nil {
				masterErrCh <- err
				return
			}
		}
		if err := connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "terminal closed"), time.Now().Add(time.Second)); err != nil {
			masterErrCh <- err
			return
		}
		masterErrCh <- nil
	}))
	defer masterServer.Close()

	gateway := NewGateway(cfg, masterServer.URL, terminalService)
	browserServer := httptest.NewServer(gateway)
	defer browserServer.Close()
	browser := dialBrowser(t, browserServer.URL, testRawGrant, browserServer.URL)
	defer browser.Close()
	if browser.Subprotocol() != Subprotocol {
		t.Fatalf("selected subprotocol = %q", browser.Subprotocol())
	}
	if strings.Contains(browser.Subprotocol(), testRawGrant) {
		t.Fatal("grant was reflected in the selected subprotocol")
	}

	messageType, opened, err := browser.ReadMessage()
	if err != nil {
		t.Fatalf("read opened: %v", err)
	}
	openedInfo, err := ValidateServerMessage(messageType, opened, cfg.MaxFrameBytes)
	if err != nil || openedInfo.Type != "opened" || openedInfo.SessionID != grant.SessionID {
		t.Fatalf("opened frame = %+v err=%v", openedInfo, err)
	}
	stdin := append([]byte{ChannelStdin}, []byte("echo ready\n")...)
	if err := browser.WriteMessage(websocket.BinaryMessage, stdin); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	var stdout string
	var exitCode *int32
	var closeReason string
	for range 3 {
		messageType, message, err := browser.ReadMessage()
		if err != nil {
			t.Fatalf("read relayed frame: %v", err)
		}
		info, err := ValidateServerMessage(messageType, message, cfg.MaxFrameBytes)
		if err != nil {
			t.Fatalf("validate relayed frame: %v", err)
		}
		switch info.Type {
		case "stdout":
			stdout = string(message[1:])
		case "exit":
			exitCode = info.ExitCode
		case "close":
			closeReason = info.CloseReason
		}
	}
	if stdout != "ready\r\n" || exitCode == nil || *exitCode != 17 || closeReason != "RUNTIME_EXITED" {
		t.Fatalf("relay result stdout=%q exit=%v close=%q", stdout, exitCode, closeReason)
	}

	if err := <-masterErrCh; err != nil {
		t.Fatalf("fake CubeMaster: %v", err)
	}
	headers := <-headersCh
	if !headers.tokenMatched || headers.sandboxID != grant.SandboxID || headers.containerID != grant.ContainerID ||
		headers.sessionID != grant.SessionID || headers.cols != "80" || headers.rows != "24" || headers.resume != "" || !headers.requestIDSet || headers.userAuthSet {
		t.Fatalf("internal headers = %+v", headers)
	}

	waitForGatewayAudit(t, terminalService, func(touches, closes []terminalAuditCall) bool { return len(closes) == 1 })
	consumeCalls, consumedRaw, prepareCalls, touches, closes := terminalService.auditSnapshot()
	if consumeCalls != 1 || consumedRaw != testRawGrant || prepareCalls != 1 || len(touches) != 0 || len(closes) != 1 {
		t.Fatalf("service calls consume=%d prepare=%d touches=%+v closes=%+v", consumeCalls, prepareCalls, touches, closes)
	}
	closed := closes[0]
	if closed.sessionID != grant.SessionID || closed.reason != "RUNTIME_EXITED" || closed.exitCode == nil || *closed.exitCode != 17 ||
		closed.bytesIn != int64(len("echo ready\n")) || closed.bytesOut != int64(len("ready\r\n")) {
		t.Fatalf("close audit = %+v", closed)
	}
}

func TestGatewayResumeDialCarriesOnlyTargetBinding(t *testing.T) {
	cfg := gatewayTestConfig()
	grant := gatewayTestGrant("resume")
	grant.ResumeOffset = 42
	headersCh := make(chan observedMasterHeaders, 1)
	masterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersCh <- observeMasterHeaders(r, cfg.InternalToken)
		connection, err := testMasterUpgrader().Upgrade(w, r, nil)
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer masterServer.Close()

	gateway := NewGateway(cfg, masterServer.URL, &fakeGatewayService{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, terminalErr := gateway.dialMaster(ctx, grant)
	if terminalErr != nil {
		t.Fatalf("dialMaster: %v", terminalErr)
	}
	_ = connection.Close()
	headers := <-headersCh
	if !headers.tokenMatched || headers.resume != "42" || headers.sessionID != grant.SessionID || headers.userAuthSet {
		t.Fatalf("resume headers = %+v", headers)
	}
}

func TestGatewayNormalAndAbnormalBrowserCloseSemantics(t *testing.T) {
	t.Run("normal close finalizes USER_CLOSED", func(t *testing.T) {
		cfg := gatewayTestConfig()
		terminalService := &fakeGatewayService{grant: gatewayTestGrant("open")}
		masterCloseCode := make(chan int, 1)
		masterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connection, err := testMasterUpgrader().Upgrade(w, r, nil)
			if err != nil {
				masterCloseCode <- -1
				return
			}
			defer connection.Close()
			_, _, err = connection.ReadMessage()
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				masterCloseCode <- closeErr.Code
				return
			}
			masterCloseCode <- -1
		}))
		defer masterServer.Close()
		gateway := NewGateway(cfg, masterServer.URL, terminalService)
		browserServer := httptest.NewServer(gateway)
		defer browserServer.Close()
		browser := dialBrowser(t, browserServer.URL, testRawGrant, browserServer.URL)
		if err := browser.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)); err != nil {
			t.Fatalf("write normal close: %v", err)
		}
		defer browser.Close()
		if code := <-masterCloseCode; code != websocket.CloseNormalClosure {
			t.Fatalf("CubeMaster close code = %d, want 1000", code)
		}
		waitForGatewayAudit(t, terminalService, func(touches, closes []terminalAuditCall) bool { return len(closes) == 1 })
		_, _, _, touches, closes := terminalService.auditSnapshot()
		if len(touches) != 0 || len(closes) != 1 || closes[0].reason != "USER_CLOSED" {
			t.Fatalf("normal close audit touches=%+v closes=%+v", touches, closes)
		}
	})

	t.Run("abnormal close preserves detached session", func(t *testing.T) {
		cfg := gatewayTestConfig()
		terminalService := &fakeGatewayService{grant: gatewayTestGrant("open")}
		masterEnded := make(chan struct{}, 1)
		masterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connection, err := testMasterUpgrader().Upgrade(w, r, nil)
			if err != nil {
				masterEnded <- struct{}{}
				return
			}
			defer connection.Close()
			_, _, _ = connection.ReadMessage()
			masterEnded <- struct{}{}
		}))
		defer masterServer.Close()
		gateway := NewGateway(cfg, masterServer.URL, terminalService)
		browserServer := httptest.NewServer(gateway)
		defer browserServer.Close()
		browser := dialBrowser(t, browserServer.URL, testRawGrant, browserServer.URL)
		if err := browser.UnderlyingConn().Close(); err != nil {
			t.Fatalf("abrupt browser close: %v", err)
		}
		<-masterEnded
		waitForGatewayAudit(t, terminalService, func(touches, closes []terminalAuditCall) bool { return len(touches) == 1 })
		_, _, _, touches, closes := terminalService.auditSnapshot()
		if len(touches) != 1 || len(closes) != 0 || touches[0].sessionID != testSessionID {
			t.Fatalf("abnormal close audit touches=%+v closes=%+v", touches, closes)
		}
	})
}

func TestGatewayRejectsOpenedSessionMismatch(t *testing.T) {
	cfg := gatewayTestConfig()
	terminalService := &fakeGatewayService{grant: gatewayTestGrant("open")}
	masterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := testMasterUpgrader().Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteMessage(websocket.BinaryMessage, statusMessage(`{"type":"opened","sessionId":"00000000-0000-0000-0000-000000000000","replay":{"from":0,"truncated":false}}`))
		_, _, _ = connection.ReadMessage()
	}))
	defer masterServer.Close()
	gateway := NewGateway(cfg, masterServer.URL, terminalService)
	browserServer := httptest.NewServer(gateway)
	defer browserServer.Close()
	browser := dialBrowser(t, browserServer.URL, testRawGrant, browserServer.URL)
	defer browser.Close()
	messageType, message, err := browser.ReadMessage()
	if err != nil {
		t.Fatalf("read mismatch status: %v", err)
	}
	info, err := ValidateServerMessage(messageType, message, cfg.MaxFrameBytes)
	if err != nil || info.Type != "error" || info.ErrorCode != "INTERNAL" {
		t.Fatalf("mismatch status = %+v err=%v", info, err)
	}
	waitForGatewayAudit(t, terminalService, func(touches, closes []terminalAuditCall) bool { return len(closes) == 1 })
	_, _, _, touches, closes := terminalService.auditSnapshot()
	if len(touches) != 0 || len(closes) != 1 || closes[0].reason != "INTERNAL" {
		t.Fatalf("mismatch audit touches=%+v closes=%+v", touches, closes)
	}
}

func TestGatewayDrainSignalsBrowserAndPreservesResume(t *testing.T) {
	cfg := gatewayTestConfig()
	touchStart := make(chan struct{}, 1)
	touchAllow := make(chan struct{})
	terminalService := &fakeGatewayService{
		grant:      gatewayTestGrant("open"),
		touchStart: touchStart,
		touchAllow: touchAllow,
	}
	masterConnected := make(chan struct{}, 1)
	masterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := testMasterUpgrader().Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		masterConnected <- struct{}{}
		_, _, _ = connection.ReadMessage()
	}))
	defer masterServer.Close()
	gateway := NewGateway(cfg, masterServer.URL, terminalService)
	browserServer := httptest.NewServer(gateway)
	defer browserServer.Close()
	browser := dialBrowser(t, browserServer.URL, testRawGrant, browserServer.URL)
	defer browser.Close()
	<-masterConnected
	waitForRegisteredSession(t, gateway)

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownErr <- gateway.Shutdown(ctx)
	}()

	messageType, message, err := browser.ReadMessage()
	if err != nil {
		t.Fatalf("read drain status: %v", err)
	}
	info, err := ValidateServerMessage(messageType, message, cfg.MaxFrameBytes)
	if err != nil || info.Type != "close" || info.CloseReason != "SERVER_DRAINING" {
		t.Fatalf("drain status = %+v err=%v", info, err)
	}
	_, _, err = browser.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("drain close error = %T %v", err, err)
	}
	select {
	case <-touchStart:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for detached audit update to start")
	}
	select {
	case err := <-shutdownErr:
		t.Fatalf("Gateway.Shutdown returned before audit completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(touchAllow)
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Gateway.Shutdown: %v", err)
	}
	_, _, _, touches, closes := terminalService.auditSnapshot()
	if len(touches) != 1 || len(closes) != 0 {
		t.Fatalf("drain audit touches=%+v closes=%+v", touches, closes)
	}
}

func TestGatewayShutdownWaitsForPreRegisterRequest(t *testing.T) {
	cfg := gatewayTestConfig()
	consumeStart := make(chan struct{}, 1)
	consumeAllow := make(chan struct{})
	terminalService := &fakeGatewayService{
		grant:        gatewayTestGrant("open"),
		consumeStart: consumeStart,
		consumeAllow: consumeAllow,
	}
	masterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := testMasterUpgrader().Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_, _, _ = connection.ReadMessage()
	}))
	defer masterServer.Close()

	gateway := NewGateway(cfg, masterServer.URL, terminalService)
	browserServer := httptest.NewServer(gateway)
	defer browserServer.Close()
	dialResult := make(chan error, 1)
	go func() {
		dialer := websocket.Dialer{
			HandshakeTimeout: 2 * time.Second,
			Subprotocols:     []string{Subprotocol, GrantPrefix + testRawGrant},
		}
		connection, response, err := dialer.Dial(
			"ws"+strings.TrimPrefix(browserServer.URL, "http")+"/api/v1/terminal/ws",
			http.Header{"Origin": []string{browserServer.URL}},
		)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if connection != nil {
			defer connection.Close()
			_, _, _ = connection.ReadMessage()
		}
		dialResult <- err
	}()

	select {
	case <-consumeStart:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for grant consumption")
	}
	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownErr <- gateway.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownErr:
		t.Fatalf("Gateway.Shutdown returned before pre-register request completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(consumeAllow)
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Gateway.Shutdown: %v", err)
	}
	if err := <-dialResult; err != nil {
		t.Fatalf("browser WebSocket dial: %v", err)
	}
	_, _, _, touches, closes := terminalService.auditSnapshot()
	if len(touches) != 1 || len(closes) != 0 {
		t.Fatalf("pre-register drain audit touches=%+v closes=%+v", touches, closes)
	}
}

func TestGatewayOriginAndSubprotocolValidation(t *testing.T) {
	cfg := gatewayTestConfig()
	terminalService := &fakeGatewayService{grant: gatewayTestGrant("open")}
	gateway := NewGateway(cfg, "http://127.0.0.1:1", terminalService)

	for _, test := range []struct {
		name    string
		origin  string
		host    string
		want    bool
		allowed []string
	}{
		{name: "same origin", origin: "https://ops.example.com", want: true},
		{name: "same origin with non-default port", origin: "https://ops.example.com:12088", host: "ops.example.com:12088", want: true},
		{name: "cross origin", origin: "https://evil.example.com", want: false},
		{name: "explicit cross origin", origin: "https://web.example.com", allowed: []string{"https://web.example.com"}, want: true},
		{name: "missing origin", want: false},
		{name: "origin with path", origin: "https://ops.example.com/path", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway.cfg.AllowedOrigins = test.allowed
			request := httptest.NewRequest(http.MethodGet, "https://ops.example.com/api/v1/terminal/ws", nil)
			request.Host = test.host
			if request.Host == "" {
				request.Host = "ops.example.com"
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if got := gateway.originAllowed(request); got != test.want {
				t.Fatalf("originAllowed = %t, want %t", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name       string
		protocols  []string
		wantGrant  string
		wantStatus int
		wantCode   string
	}{
		{name: "exact pair", protocols: []string{Subprotocol, GrantPrefix + testRawGrant}, wantGrant: testRawGrant},
		{name: "missing protocol", protocols: []string{GrantPrefix + testRawGrant}, wantStatus: http.StatusBadRequest, wantCode: "PROTOCOL_ERROR"},
		{name: "missing grant", protocols: []string{Subprotocol}, wantStatus: http.StatusUnauthorized, wantCode: "GRANT_INVALID"},
		{name: "duplicate grant", protocols: []string{Subprotocol, GrantPrefix + testRawGrant, GrantPrefix + "other"}, wantStatus: http.StatusUnauthorized, wantCode: "GRANT_INVALID"},
		{name: "unknown extra", protocols: []string{Subprotocol, GrantPrefix + testRawGrant, "extra"}, wantStatus: http.StatusBadRequest, wantCode: "PROTOCOL_ERROR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant, status, code := parseTerminalSubprotocols(test.protocols)
			if grant != test.wantGrant || status != test.wantStatus || code != test.wantCode {
				t.Fatalf("parse = grant:%q status:%d code:%q", grant, status, code)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "http://ops.example.com/api/v1/terminal/ws", nil)
	request.Host = "ops.example.com"
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Protocol", Subprotocol+", "+GrantPrefix+testRawGrant)
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing Origin status = %d", recorder.Code)
	}
	consumeCalls, _, _, _, _ := terminalService.auditSnapshot()
	if consumeCalls != 0 {
		t.Fatalf("grant consumed before Origin validation: calls=%d", consumeCalls)
	}
}

func gatewayTestConfig() config.TerminalConfig {
	return config.TerminalConfig{
		Enabled:               true,
		GrantTTLSeconds:       60,
		HandshakeTimeoutSec:   2,
		PingIntervalSeconds:   60,
		PongTimeoutSeconds:    10,
		WriteDeadlineSeconds:  2,
		ReconnectGraceSeconds: 30,
		MaxFrameBytes:         64 << 10,
		MaxSessionsPerUser:    5,
		MaxSessionsPerReplica: 5,
		DrainTimeoutSeconds:   2,
		InternalToken:         "test-internal-token-32-bytes-long",
	}
}

func gatewayTestGrant(kind string) *service.ConsumedTerminalGrant {
	return &service.ConsumedTerminalGrant{
		ID:          "grant-id",
		Kind:        kind,
		UserID:      "admin",
		SandboxID:   "sandbox-a",
		ContainerID: "container-a",
		SessionID:   testSessionID,
		Cols:        80,
		Rows:        24,
	}
}

func testMasterUpgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		Subprotocols: []string{Subprotocol},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
}

func observeMasterHeaders(r *http.Request, expectedToken string) observedMasterHeaders {
	return observedMasterHeaders{
		tokenMatched: r.Header.Get(headerInternalToken) == expectedToken,
		sandboxID:    r.Header.Get(headerTerminalSandbox),
		containerID:  r.Header.Get(headerTerminalContainer),
		sessionID:    r.Header.Get(headerTerminalSession),
		cols:         r.Header.Get(headerTerminalCols),
		rows:         r.Header.Get(headerTerminalRows),
		resume:       r.Header.Get(headerTerminalResume),
		// Keep the assertion independent from the gateway implementation
		// constant so this test catches a cross-module header drift.
		requestIDSet: r.Header.Get(cubeMasterRequestIDHeader) != "",
		userAuthSet:  r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "",
	}
}

func dialBrowser(t *testing.T, serverURL, rawGrant, origin string) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{
		HandshakeTimeout: 2 * time.Second,
		Subprotocols:     []string{Subprotocol, GrantPrefix + rawGrant},
	}
	connection, response, err := dialer.Dial("ws"+strings.TrimPrefix(serverURL, "http")+"/api/v1/terminal/ws", http.Header{"Origin": []string{origin}})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial browser: %v (status=%d)", err, status)
	}
	return connection
}

func waitForGatewayAudit(t *testing.T, terminalService *fakeGatewayService, ready func([]terminalAuditCall, []terminalAuditCall) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, _, touches, closes := terminalService.auditSnapshot()
		if ready(touches, closes) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, _, touches, closes := terminalService.auditSnapshot()
	t.Fatalf("timed out waiting for audit: touches=%+v closes=%+v", touches, closes)
}

func waitForRegisteredSession(t *testing.T, gateway *Gateway) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gateway.mu.Lock()
		registered := len(gateway.sessions)
		gateway.mu.Unlock()
		if registered == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for registered gateway session")
}
