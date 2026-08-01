// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package inner

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxservice "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/terminalprotocol"
)

const (
	HeaderInternalToken       = "X-Cube-Internal-Token"
	HeaderTerminalSandbox     = "X-Cube-Terminal-Sandbox"
	HeaderTerminalContainer   = "X-Cube-Terminal-Container"
	HeaderTerminalSession     = "X-Cube-Terminal-Session"
	HeaderTerminalCols        = "X-Cube-Terminal-Cols"
	HeaderTerminalRows        = "X-Cube-Terminal-Rows"
	HeaderTerminalResume      = "X-Cube-Terminal-Resume-Offset"
	terminalCloseUser         = "USER_CLOSED"
	terminalCloseProtocol     = "PROTOCOL_ERROR"
	terminalCodeInternal      = "INTERNAL"
	terminalMetadataRequestID = "x-cube-request-id"
	terminalMetadataSessionID = "x-cube-terminal-session-id"
	terminalEventOpened       = "terminal_opened"
)

// terminalTargetResolveTimeout bounds the sandbox resolution fallback. In the
// normal flow the target is already cached (CubeOps resolved it moments
// earlier to issue the grant); only a stale cache reaches the sandbox
// resolver, whose cluster-scan fallback must never block the WebSocket
// handshake for the full request lifetime. It is a variable so tests can
// shrink the window.
var terminalTargetResolveTimeout = 10 * time.Second

var (
	errTerminalTargetNotFound  = errors.New("terminal target not found")
	errTerminalTargetUnhealthy = errors.New("terminal target is unavailable")
)

type terminalRelaySettings struct {
	InternalToken string
	MaxFrameBytes int
	WriteDeadline time.Duration
	CloseTimeout  time.Duration
}

type terminalRelayClient interface {
	Send(*cubebox.TerminalClientFrame) error
	Recv() (*cubebox.TerminalServerFrame, error)
	CloseSend() error
	Close() error
}

type terminalPumpResult struct {
	err              error
	waitForPeer      bool
	closeCode        int
	closeReason      string
	localCloseReason string
}

type terminalRelayOpenedEvent struct {
	RequestID   string
	SessionID   string
	SandboxID   string
	ContainerID string
	Resume      bool
}

type terminalRelayWarning struct {
	RequestID string
	SessionID string
	SandboxID string
	ErrorKind string
}

var (
	terminalRelaySettingsProvider = currentTerminalRelaySettings
	terminalTargetResolver        = resolveTerminalTarget
	// terminalSandboxIDResolver is the stale-cache fallback inside
	// resolveTerminalTarget; it is a variable so tests can exercise the
	// bounded resolution timeout without a real cluster scan.
	terminalSandboxIDResolver = sandboxservice.ResolveSandboxID
	terminalRelayOpener       = func(ctx context.Context, endpoint string) (terminalRelayClient, error) {
		return cubelet.Terminal(ctx, endpoint)
	}
	terminalRelayOpenedLogger = func(ctx context.Context, event terminalRelayOpenedEvent) {
		log.G(ctx).WithFields(map[string]interface{}{
			"event":        terminalEventOpened,
			"request_id":   event.RequestID,
			"session_id":   event.SessionID,
			"sandbox_id":   event.SandboxID,
			"container_id": event.ContainerID,
			"resume":       event.Resume,
		}).Info("terminal opened")
	}
	terminalRelayWarningLogger = func(ctx context.Context, warning terminalRelayWarning) {
		log.G(ctx).WithFields(map[string]interface{}{
			"request_id": warning.RequestID,
			"session_id": warning.SessionID,
			"sandbox_id": warning.SandboxID,
			"error_kind": warning.ErrorKind,
		}).Warn("terminal relay ended")
	}
)

var terminalRelayUpgrader = websocket.Upgrader{
	ReadBufferSize:    32 << 10,
	WriteBufferSize:   32 << 10,
	EnableCompression: false,
	Subprotocols:      []string{terminalprotocol.Subprotocol},
	CheckOrigin: func(*http.Request) bool {
		// Browser Origin validation belongs to the CubeOps gateway. This route is
		// service-to-service and is protected by a dedicated internal token.
		return true
	},
}

func terminalRelayGinHandler(c *gin.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.G(c.Request.Context()).Error("terminal relay panic")
		}
	}()
	serveTerminalRelay(c.Writer, c.Request)
}

func serveTerminalRelay(w http.ResponseWriter, r *http.Request) {
	settings := terminalRelaySettingsProvider()
	if settings.InternalToken == "" || settings.MaxFrameBytes <= 0 || settings.WriteDeadline <= 0 || settings.CloseTimeout <= 0 {
		http.Error(w, "terminal relay is unavailable", http.StatusServiceUnavailable)
		return
	}
	if !secureTokenEqual(settings.InternalToken, r.Header.Get(HeaderInternalToken)) {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	if !slices.Contains(websocket.Subprotocols(r), terminalprotocol.Subprotocol) {
		http.Error(w, "unsupported terminal subprotocol", http.StatusBadRequest)
		return
	}

	open, err := terminalOpenFromHeaders(r)
	if err != nil {
		http.Error(w, "invalid terminal request", http.StatusBadRequest)
		return
	}
	canonicalSandboxID, endpoint, err := terminalTargetResolver(r.Context(), open.GetSandboxId())
	if err != nil {
		switch {
		case errors.Is(err, errTerminalTargetUnhealthy):
			http.Error(w, "terminal target is unavailable", http.StatusServiceUnavailable)
		default:
			http.Error(w, "terminal target was not found", http.StatusNotFound)
		}
		return
	}
	open.SandboxId = canonicalSandboxID

	relayCtx, relayCancel := context.WithCancel(r.Context())
	defer relayCancel()
	relayCtx = metadata.AppendToOutgoingContext(relayCtx,
		terminalMetadataRequestID, open.GetRequestId(),
		terminalMetadataSessionID, open.GetSessionId(),
	)
	stream, err := terminalRelayOpener(relayCtx, endpoint)
	if err != nil {
		http.Error(w, "terminal target connection failed", http.StatusBadGateway)
		return
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil && !isExpectedTerminalRelayError(closeErr) {
			terminalRelayWarningLogger(r.Context(), terminalRelayWarning{
				RequestID: open.GetRequestId(),
				SessionID: open.GetSessionId(),
				SandboxID: open.GetSandboxId(),
				ErrorKind: terminalRelayErrorKind(closeErr),
			})
		}
	}()

	connection, err := terminalRelayUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	connection.SetReadLimit(int64(settings.MaxFrameBytes + 1))

	if err := stream.Send(&cubebox.TerminalClientFrame{
		Frame: &cubebox.TerminalClientFrame_Open{Open: open},
	}); err != nil {
		_ = writeTerminalError(connection, settings, terminalCodeInternal)
		closeTerminalConnection(connection, websocket.CloseInternalServerErr, "terminal open failed")
		return
	}

	openedEvent := terminalRelayOpenedEvent{
		RequestID:   open.GetRequestId(),
		SessionID:   open.GetSessionId(),
		SandboxID:   open.GetSandboxId(),
		ContainerID: open.GetContainerId(),
		Resume:      open.GetResume() != nil,
	}
	if err := relayTerminal(relayCtx, relayCancel, connection, stream, settings, openedEvent); err != nil {
		terminalRelayWarningLogger(r.Context(), terminalRelayWarning{
			RequestID: open.GetRequestId(),
			SessionID: open.GetSessionId(),
			SandboxID: open.GetSandboxId(),
			ErrorKind: terminalRelayErrorKind(err),
		})
	}
}

func terminalOpenFromHeaders(r *http.Request) (*cubebox.TerminalOpen, error) {
	sandboxID := strings.TrimSpace(r.Header.Get(HeaderTerminalSandbox))
	containerID := strings.TrimSpace(r.Header.Get(HeaderTerminalContainer))
	sessionID := strings.TrimSpace(r.Header.Get(HeaderTerminalSession))
	requestID := strings.TrimSpace(r.Header.Get(constants.RequestID))
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if sandboxID == "" || len(sandboxID) > 256 || len(containerID) > 256 || len(requestID) > 256 {
		return nil, errors.New("terminal identifiers are invalid")
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return nil, errors.New("terminal session id is invalid")
	}
	cols, err := parseTerminalDimension(r.Header.Get(HeaderTerminalCols))
	if err != nil {
		return nil, err
	}
	rows, err := parseTerminalDimension(r.Header.Get(HeaderTerminalRows))
	if err != nil {
		return nil, err
	}

	open := &cubebox.TerminalOpen{
		RequestId:   requestID,
		SandboxId:   sandboxID,
		ContainerId: containerID,
		SessionId:   sessionID,
		Cols:        cols,
		Rows:        rows,
	}
	if rawOffset, present := r.Header[http.CanonicalHeaderKey(HeaderTerminalResume)]; present {
		if len(rawOffset) != 1 || strings.TrimSpace(rawOffset[0]) == "" {
			return nil, errors.New("terminal resume offset is invalid")
		}
		offset, err := strconv.ParseUint(strings.TrimSpace(rawOffset[0]), 10, 64)
		if err != nil {
			return nil, errors.New("terminal resume offset is invalid")
		}
		open.Resume = &cubebox.TerminalResume{SessionId: sessionID, LastOffset: offset}
	}
	return open, nil
}

func parseTerminalDimension(value string) (uint32, error) {
	dimension, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil || dimension == 0 || dimension > 1000 {
		return 0, errors.New("terminal dimension is out of range")
	}
	return uint32(dimension), nil
}

func currentTerminalRelaySettings() terminalRelaySettings {
	cfg := config.GetConfig()
	if cfg == nil || cfg.TerminalConf == nil {
		return terminalRelaySettings{}
	}
	return terminalRelaySettings{
		InternalToken: cfg.TerminalConf.InternalToken,
		MaxFrameBytes: cfg.TerminalConf.MaxFrameBytes,
		WriteDeadline: time.Duration(cfg.TerminalConf.WriteDeadlineInSec) * time.Second,
		CloseTimeout:  time.Duration(cfg.TerminalConf.CloseTimeoutInSec) * time.Second,
	}
}

func secureTokenEqual(expected, provided string) bool {
	if expected == "" || provided == "" {
		return false
	}
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

func resolveTerminalTarget(ctx context.Context, requestedSandboxID string) (string, string, error) {
	requestedSandboxID = strings.TrimSpace(requestedSandboxID)
	if requestedSandboxID == "" {
		return "", "", errTerminalTargetNotFound
	}
	if hostIP, ok := terminalTargetHost(ctx, requestedSandboxID); ok {
		return terminalEndpointForHost(requestedSandboxID, hostIP)
	}
	resolveCtx, resolveCancel := context.WithTimeout(ctx, terminalTargetResolveTimeout)
	canonicalSandboxID, err := terminalSandboxIDResolver(resolveCtx, requestedSandboxID)
	resolveCancel()
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errTerminalTargetNotFound, err)
	}
	hostIP, ok := terminalTargetHost(ctx, canonicalSandboxID)
	if !ok {
		return "", "", errTerminalTargetNotFound
	}
	return terminalEndpointForHost(canonicalSandboxID, hostIP)
}

func terminalTargetHost(ctx context.Context, sandboxID string) (string, bool) {
	if cached := localcache.GetSandboxCache(sandboxID); cached != nil && cached.HostIP != "" {
		return cached.HostIP, true
	}
	if proxy, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok && proxy != nil && proxy.HostIP != "" {
		return proxy.HostIP, true
	}
	return "", false
}

func terminalEndpointForHost(sandboxID, hostIP string) (string, string, error) {
	node, ok := localcache.GetNodesByIp(hostIP)
	if !ok {
		return "", "", errTerminalTargetNotFound
	}
	if !node.Healthy {
		return "", "", errTerminalTargetUnhealthy
	}
	return sandboxID, cubelet.GetCubeletAddr(hostIP), nil
}

func relayTerminal(
	ctx context.Context,
	cancel context.CancelFunc,
	connection *websocket.Conn,
	stream terminalRelayClient,
	settings terminalRelaySettings,
	openedEvent terminalRelayOpenedEvent,
) error {
	results := make(chan terminalPumpResult, 2)
	var discardWrites atomic.Bool
	go func() {
		results <- pumpTerminalClient(connection, stream, settings, &discardWrites)
	}()
	go func() {
		results <- pumpTerminalServer(ctx, connection, stream, settings, &discardWrites, openedEvent)
	}()

	first := <-results
	closeCode, closeReason := first.closeCode, first.closeReason
	closeControlWritten := false
	var second terminalPumpResult
	if first.waitForPeer {
		timer := time.NewTimer(settings.CloseTimeout)
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
			closeTerminalConnection(connection, websocket.CloseInternalServerErr, "terminal close timeout")
			closeControlWritten = true
			second = <-results
			closeCode = 0
		case <-ctx.Done():
			cancel()
			_ = connection.Close()
			second = <-results
			closeCode = 0
		}
	} else {
		cancel()
		if closeCode != 0 && !discardWrites.Load() {
			writeTerminalCloseControl(connection, closeCode, closeReason)
			closeControlWritten = true
		}
		_ = connection.Close()
		second = <-results
		closeCode = 0
	}

	cancel()
	localResult := first
	if localResult.localCloseReason == "" {
		localResult = second
	}
	var localCloseErr error
	if localResult.localCloseReason != "" && !closeControlWritten {
		localCloseErr = writeTerminalCloseStatus(connection, settings, localResult.localCloseReason)
		if localResult.closeCode != 0 {
			writeTerminalCloseControl(connection, localResult.closeCode, localResult.closeReason)
		}
		closeControlWritten = true
		closeCode = 0
	}
	if closeCode == 0 {
		closeCode, closeReason = second.closeCode, second.closeReason
	}
	if closeCode != 0 && !closeControlWritten && !discardWrites.Load() {
		writeTerminalCloseControl(connection, closeCode, closeReason)
	}
	_ = connection.Close()
	return errors.Join(unexpectedTerminalRelayError(first.err), unexpectedTerminalRelayError(second.err), localCloseErr)
}

func pumpTerminalClient(
	connection *websocket.Conn,
	stream terminalRelayClient,
	settings terminalRelaySettings,
	discardWrites *atomic.Bool,
) terminalPumpResult {
	for {
		messageType, message, err := connection.ReadMessage()
		if err != nil {
			if errors.Is(err, websocket.ErrReadLimit) {
				discardWrites.Store(true)
				return closeCubeletTerminal(stream, terminalCloseProtocol, websocket.CloseMessageTooBig, "terminal frame too large")
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseNormalClosure {
				discardWrites.Store(true)
				return closeCubeletTerminal(stream, terminalCloseUser, websocket.CloseNormalClosure, "terminal closed")
			}
			_ = stream.CloseSend()
			return terminalPumpResult{}
		}
		if messageType != websocket.BinaryMessage {
			return closeCubeletTerminalWithLocalStatus(stream, terminalCloseProtocol,
				websocket.ClosePolicyViolation, "binary terminal frames required", discardWrites)
		}
		frame, err := terminalprotocol.DecodeClientFrame(message, settings.MaxFrameBytes)
		if err != nil {
			return closeCubeletTerminalWithLocalStatus(stream, terminalCloseProtocol,
				websocket.ClosePolicyViolation, "invalid terminal frame", discardWrites)
		}
		if err := stream.Send(frame); err != nil {
			return terminalPumpResult{err: err, closeCode: websocket.CloseInternalServerErr, closeReason: "terminal upstream failed"}
		}
	}
}

func closeCubeletTerminalWithLocalStatus(
	stream terminalRelayClient,
	reason string,
	closeCode int,
	closeReason string,
	discardWrites *atomic.Bool,
) terminalPumpResult {
	// CubeMaster owns WebSocket framing errors. Suppress the downstream close
	// status while the gRPC stream drains, then emit the authoritative reason
	// after both pumps have joined so there is still only one data writer.
	discardWrites.Store(true)
	result := closeCubeletTerminal(stream, reason, closeCode, closeReason)
	result.localCloseReason = reason
	return result
}

func closeCubeletTerminal(stream terminalRelayClient, reason string, closeCode int, closeReason string) terminalPumpResult {
	err := stream.Send(&cubebox.TerminalClientFrame{
		Frame: &cubebox.TerminalClientFrame_Close{Close: &cubebox.TerminalClose{Reason: reason}},
	})
	err = errors.Join(err, stream.CloseSend())
	return terminalPumpResult{
		err:         err,
		waitForPeer: true,
		closeCode:   closeCode,
		closeReason: closeReason,
	}
}

func pumpTerminalServer(
	ctx context.Context,
	connection *websocket.Conn,
	stream terminalRelayClient,
	settings terminalRelaySettings,
	discardWrites *atomic.Bool,
	openedEvent terminalRelayOpenedEvent,
) terminalPumpResult {
	openedLogged := false
	for {
		frame, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
				return terminalPumpResult{}
			}
			return terminalPumpResult{err: err, closeCode: websocket.CloseInternalServerErr, closeReason: "terminal downstream failed"}
		}
		message, err := terminalprotocol.EncodeServerFrame(frame, settings.MaxFrameBytes)
		if err != nil {
			return terminalPumpResult{err: err, closeCode: websocket.CloseInternalServerErr, closeReason: "invalid terminal response"}
		}
		if !discardWrites.Load() {
			if err := connection.SetWriteDeadline(time.Now().Add(settings.WriteDeadline)); err != nil {
				return terminalPumpResult{err: err}
			}
			if err := connection.WriteMessage(websocket.BinaryMessage, message); err != nil {
				return terminalPumpResult{}
			}
			if frame.GetOpened() != nil && !openedLogged {
				terminalRelayOpenedLogger(ctx, openedEvent)
				openedLogged = true
			}
		}
		if terminalprotocol.IsCloseFrame(frame) {
			return terminalPumpResult{
				waitForPeer: discardWrites.Load(),
				closeCode:   websocket.CloseNormalClosure,
				closeReason: "terminal closed",
			}
		}
		select {
		case <-ctx.Done():
			return terminalPumpResult{}
		default:
		}
	}
}

func terminalRelayErrorKind(err error) string {
	switch status.Code(err) {
	case codes.Canceled:
		return "DOWNSTREAM_CANCELED"
	case codes.DeadlineExceeded:
		return "DOWNSTREAM_TIMEOUT"
	case codes.InvalidArgument:
		return "DOWNSTREAM_PROTOCOL"
	case codes.Unavailable:
		return "DOWNSTREAM_UNAVAILABLE"
	default:
		return "DOWNSTREAM_ERROR"
	}
}

func writeTerminalError(connection *websocket.Conn, settings terminalRelaySettings, code string) error {
	return writeTerminalStatusFrame(connection, settings, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Error{Error: &cubebox.TerminalError{Code: code}},
	})
}

func writeTerminalCloseStatus(connection *websocket.Conn, settings terminalRelaySettings, reason string) error {
	return writeTerminalStatusFrame(connection, settings, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: reason}},
	})
}

func writeTerminalStatusFrame(connection *websocket.Conn, settings terminalRelaySettings, frame *cubebox.TerminalServerFrame) error {
	message, err := terminalprotocol.EncodeServerFrame(frame, settings.MaxFrameBytes)
	if err != nil {
		return err
	}
	if err := connection.SetWriteDeadline(time.Now().Add(settings.WriteDeadline)); err != nil {
		return err
	}
	return connection.WriteMessage(websocket.BinaryMessage, message)
}

func closeTerminalConnection(connection *websocket.Conn, code int, reason string) {
	writeTerminalCloseControl(connection, code, reason)
	_ = connection.Close()
}

func writeTerminalCloseControl(connection *websocket.Conn, code int, reason string) {
	if connection == nil || code == 0 {
		return
	}
	deadline := time.Now().Add(time.Second)
	_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline)
}

func unexpectedTerminalRelayError(err error) error {
	if isExpectedTerminalRelayError(err) {
		return nil
	}
	return err
}

func isExpectedTerminalRelayError(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return true
	}
	return status.Code(err) == codes.Canceled
}
