// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package inner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/terminalprotocol"
)

func init() {
	// The terminalTargetHost cache lookup touches wrapredis, which panics on a
	// nil config; initialize the global config from the checked-in conf.yaml
	// (same pattern as pkg/localcache tests) so Redis lookups fail closed.
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		_, file, _, _ := runtime.Caller(0)
		cubeMasterDir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file)))))
		os.Setenv("CUBE_MASTER_CONFIG_PATH", filepath.Join(cubeMasterDir, "conf.yaml"))
	}
	if _, err := config.Init(); err != nil {
		panic(err)
	}
}

const (
	testTerminalToken     = "terminal-internal-secret"
	testTerminalSessionID = "11111111-1111-4111-8111-111111111111"
)

var terminalRelayHooksMu sync.Mutex
var terminalRelayLogHooksMu sync.Mutex

type fakeTerminalRecv struct {
	frame *cubebox.TerminalServerFrame
	err   error
}

type fakeTerminalRelayStream struct {
	ctx context.Context

	sent chan *cubebox.TerminalClientFrame
	recv chan fakeTerminalRecv

	closeSendOnce sync.Once
	closeOnce     sync.Once
	closeSendDone chan struct{}
	closeDone     chan struct{}
}

func newFakeTerminalRelayStream() *fakeTerminalRelayStream {
	return &fakeTerminalRelayStream{
		sent:          make(chan *cubebox.TerminalClientFrame, 16),
		recv:          make(chan fakeTerminalRecv, 16),
		closeSendDone: make(chan struct{}),
		closeDone:     make(chan struct{}),
	}
}

func (f *fakeTerminalRelayStream) Send(frame *cubebox.TerminalClientFrame) error {
	ctx := f.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case f.sent <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeTerminalRelayStream) Recv() (*cubebox.TerminalServerFrame, error) {
	ctx := f.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case result := <-f.recv:
		return result.frame, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeTerminalRelayStream) CloseSend() error {
	f.closeSendOnce.Do(func() { close(f.closeSendDone) })
	return nil
}

func (f *fakeTerminalRelayStream) Close() error {
	f.closeOnce.Do(func() {
		_ = f.CloseSend()
		close(f.closeDone)
	})
	return nil
}

func testTerminalRelaySettings() terminalRelaySettings {
	return terminalRelaySettings{
		InternalToken: testTerminalToken,
		MaxFrameBytes: 64 << 10,
		WriteDeadline: time.Second,
		CloseTimeout:  time.Second,
	}
}

func validTerminalHeaders() http.Header {
	headers := make(http.Header)
	headers.Set(HeaderInternalToken, testTerminalToken)
	headers.Set(HeaderTerminalSandbox, "sandbox-short")
	headers.Set(HeaderTerminalContainer, "container-a")
	headers.Set(HeaderTerminalSession, testTerminalSessionID)
	headers.Set(HeaderTerminalCols, "100")
	headers.Set(HeaderTerminalRows, "40")
	headers.Set(HeaderTerminalResume, "12")
	// Keep the input independent from CubeMaster's implementation constant so
	// this test catches request-ID header drift with the CubeOps gateway.
	headers.Set("X-RequestID", "request-a")
	headers.Set("Sec-WebSocket-Protocol", terminalprotocol.Subprotocol)
	return headers
}

func installTerminalRelayHooks(
	t *testing.T,
	settings terminalRelaySettings,
	resolver func(context.Context, string) (string, string, error),
	opener func(context.Context, string) (terminalRelayClient, error),
) {
	t.Helper()
	terminalRelayHooksMu.Lock()
	oldSettingsProvider := terminalRelaySettingsProvider
	oldResolver := terminalTargetResolver
	oldOpener := terminalRelayOpener
	t.Cleanup(func() {
		terminalRelaySettingsProvider = oldSettingsProvider
		terminalTargetResolver = oldResolver
		terminalRelayOpener = oldOpener
		terminalRelayHooksMu.Unlock()
	})
	terminalRelaySettingsProvider = func() terminalRelaySettings { return settings }
	terminalTargetResolver = resolver
	terminalRelayOpener = opener
}

func installTerminalRelayLogHooks(
	t *testing.T,
	opened func(context.Context, terminalRelayOpenedEvent),
	warning func(context.Context, terminalRelayWarning),
) {
	t.Helper()
	terminalRelayLogHooksMu.Lock()
	oldOpenedLogger := terminalRelayOpenedLogger
	oldWarningLogger := terminalRelayWarningLogger
	t.Cleanup(func() {
		terminalRelayOpenedLogger = oldOpenedLogger
		terminalRelayWarningLogger = oldWarningLogger
		terminalRelayLogHooksMu.Unlock()
	})
	terminalRelayOpenedLogger = opened
	terminalRelayWarningLogger = warning
}

func TestServeTerminalRelayRejectsInvalidRequestsBeforeUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		settings   terminalRelaySettings
		mutate     func(http.Header)
		resolveErr error
		openErr    error
		wantStatus int
	}{
		{
			name:       "relay disabled",
			settings:   terminalRelaySettings{},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:     "missing token",
			settings: testTerminalRelaySettings(),
			mutate: func(headers http.Header) {
				headers.Del(HeaderInternalToken)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:     "wrong token",
			settings: testTerminalRelaySettings(),
			mutate: func(headers http.Header) {
				headers.Set(HeaderInternalToken, "wrong")
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:     "missing subprotocol",
			settings: testTerminalRelaySettings(),
			mutate: func(headers http.Header) {
				headers.Del("Sec-WebSocket-Protocol")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:     "invalid session id",
			settings: testTerminalRelaySettings(),
			mutate: func(headers http.Header) {
				headers.Set(HeaderTerminalSession, "not-a-uuid")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:     "invalid columns",
			settings: testTerminalRelaySettings(),
			mutate: func(headers http.Header) {
				headers.Set(HeaderTerminalCols, "1001")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:     "invalid rows",
			settings: testTerminalRelaySettings(),
			mutate: func(headers http.Header) {
				headers.Set(HeaderTerminalRows, "0")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:     "duplicate resume offset",
			settings: testTerminalRelaySettings(),
			mutate: func(headers http.Header) {
				headers[http.CanonicalHeaderKey(HeaderTerminalResume)] = []string{"1", "2"}
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "target not found",
			settings:   testTerminalRelaySettings(),
			resolveErr: errors.New("not found"),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "target unhealthy",
			settings:   testTerminalRelaySettings(),
			resolveErr: errTerminalTargetUnhealthy,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "cubelet open failed",
			settings:   testTerminalRelaySettings(),
			openErr:    errors.New("dial failed"),
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := func(context.Context, string) (string, string, error) {
				if tt.resolveErr != nil {
					return "", "", tt.resolveErr
				}
				return "sandbox-canonical", "127.0.0.1:9999", nil
			}
			opener := func(context.Context, string) (terminalRelayClient, error) {
				if tt.openErr != nil {
					return nil, tt.openErr
				}
				return nil, errors.New("unexpected terminal opener call")
			}
			installTerminalRelayHooks(t, tt.settings, resolver, opener)

			headers := validTerminalHeaders()
			if tt.mutate != nil {
				tt.mutate(headers)
			}
			req := httptest.NewRequest(http.MethodGet, "http://cubemaster/internal/terminal/relay", nil)
			req.Header = headers
			response := httptest.NewRecorder()

			serveTerminalRelay(response, req)

			require.Equal(t, tt.wantStatus, response.Code)
		})
	}
}

type openedTerminalRelay struct {
	ctx      context.Context
	endpoint string
}

func startTerminalRelayTestServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		serveTerminalRelay(w, r)
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server, done
}

func dialTerminalRelay(t *testing.T, server *httptest.Server, headers http.Header) *websocket.Conn {
	t.Helper()
	headers = headers.Clone()
	headers.Del("Sec-WebSocket-Protocol")
	dialer := websocket.Dialer{
		HandshakeTimeout: time.Second,
		Subprotocols:     []string{terminalprotocol.Subprotocol},
	}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + TerminalRelayAction
	connection, response, err := dialer.Dial(wsURL, headers)
	if response != nil {
		t.Cleanup(func() { _ = response.Body.Close() })
	}
	require.NoError(t, err)
	require.Equal(t, terminalprotocol.Subprotocol, connection.Subprotocol())
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func waitTerminalClientFrame(t *testing.T, stream *fakeTerminalRelayStream) *cubebox.TerminalClientFrame {
	t.Helper()
	select {
	case frame := <-stream.sent:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for terminal client frame")
		return nil
	}
}

func pushTerminalServerFrame(t *testing.T, stream *fakeTerminalRelayStream, frame *cubebox.TerminalServerFrame) {
	t.Helper()
	select {
	case stream.recv <- fakeTerminalRecv{frame: frame}:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending terminal server frame")
	}
}

func readTerminalBinary(t *testing.T, connection *websocket.Conn) []byte {
	t.Helper()
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(2*time.Second)))
	messageType, message, err := connection.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.BinaryMessage, messageType)
	return message
}

func readTerminalUntilClose(t *testing.T, connection *websocket.Conn) ([][]byte, int) {
	t.Helper()
	var binaryMessages [][]byte
	for {
		require.NoError(t, connection.SetReadDeadline(time.Now().Add(2*time.Second)))
		messageType, message, err := connection.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr)
			return binaryMessages, closeErr.Code
		}
		require.Equal(t, websocket.BinaryMessage, messageType)
		binaryMessages = append(binaryMessages, message)
	}
}

func waitTerminalSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestServeTerminalRelayForwardsTypedFrames(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	opened := make(chan openedTerminalRelay, 1)
	requestedSandbox := make(chan string, 1)
	installTerminalRelayHooks(t, testTerminalRelaySettings(),
		func(_ context.Context, sandboxID string) (string, string, error) {
			requestedSandbox <- sandboxID
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, endpoint string) (terminalRelayClient, error) {
			stream.ctx = ctx
			opened <- openedTerminalRelay{ctx: ctx, endpoint: endpoint}
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())

	require.Equal(t, "sandbox-short", <-requestedSandbox)
	openCall := <-opened
	require.Equal(t, "10.0.0.7:9999", openCall.endpoint)
	md, ok := metadata.FromOutgoingContext(openCall.ctx)
	require.True(t, ok)
	require.Equal(t, []string{"request-a"}, md.Get(terminalMetadataRequestID))
	require.Equal(t, []string{testTerminalSessionID}, md.Get(terminalMetadataSessionID))

	open := waitTerminalClientFrame(t, stream).GetOpen()
	require.NotNil(t, open)
	require.Equal(t, "request-a", open.GetRequestId())
	require.Equal(t, "sandbox-canonical", open.GetSandboxId())
	require.Equal(t, "container-a", open.GetContainerId())
	require.Equal(t, testTerminalSessionID, open.GetSessionId())
	require.Equal(t, uint32(100), open.GetCols())
	require.Equal(t, uint32(40), open.GetRows())
	require.Equal(t, testTerminalSessionID, open.GetResume().GetSessionId())
	require.Equal(t, uint64(12), open.GetResume().GetLastOffset())

	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Opened{Opened: &cubebox.TerminalOpened{
			SessionId:       testTerminalSessionID,
			ReplayFrom:      12,
			ReplayTruncated: true,
		}},
	})
	openedMessage := readTerminalBinary(t, connection)
	require.Equal(t, terminalprotocol.ChannelStatus, openedMessage[0])
	require.JSONEq(t, "{\"type\":\"opened\",\"sessionId\":\"11111111-1111-4111-8111-111111111111\",\"replay\":{\"from\":12,\"truncated\":true}}", string(openedMessage[1:]))

	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage,
		append([]byte{terminalprotocol.ChannelStdin}, []byte("echo ready\n")...)))
	require.Equal(t, []byte("echo ready\n"), waitTerminalClientFrame(t, stream).GetStdin())

	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage,
		append([]byte{terminalprotocol.ChannelResize}, []byte("{\"cols\":120,\"rows\":50}")...)))
	resize := waitTerminalClientFrame(t, stream).GetResize()
	require.Equal(t, uint32(120), resize.GetCols())
	require.Equal(t, uint32(50), resize.GetRows())

	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Stdout{Stdout: &cubebox.TerminalStdout{
			Data:   []byte("hello"),
			Offset: 12,
		}},
	})
	require.Equal(t, append([]byte{terminalprotocol.ChannelStdout}, []byte("hello")...), readTerminalBinary(t, connection))

	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Exit{Exit: &cubebox.TerminalExit{ExitCode: 7}},
	})
	exitMessage := readTerminalBinary(t, connection)
	require.JSONEq(t, "{\"type\":\"exit\",\"exitCode\":7}", string(exitMessage[1:]))

	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: "RUNTIME_EXITED"}},
	})
	closeMessage := readTerminalBinary(t, connection)
	require.JSONEq(t, "{\"type\":\"close\",\"reason\":\"RUNTIME_EXITED\"}", string(closeMessage[1:]))
	_, closeCode := readTerminalUntilClose(t, connection)
	require.Equal(t, websocket.CloseNormalClosure, closeCode)
	waitTerminalSignal(t, handlerDone, "relay handler")
	waitTerminalSignal(t, stream.closeDone, "stream close")
}

func TestServeTerminalRelayLogsOpenedOnceAfterCubeletAcknowledgement(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	openedEvents := make(chan terminalRelayOpenedEvent, 2)
	installTerminalRelayLogHooks(t,
		func(_ context.Context, event terminalRelayOpenedEvent) { openedEvents <- event },
		func(context.Context, terminalRelayWarning) {},
	)
	installTerminalRelayHooks(t, testTerminalRelaySettings(),
		func(context.Context, string) (string, string, error) {
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, _ string) (terminalRelayClient, error) {
			stream.ctx = ctx
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())
	require.NotNil(t, waitTerminalClientFrame(t, stream).GetOpen())

	openedFrame := &cubebox.TerminalServerFrame{Frame: &cubebox.TerminalServerFrame_Opened{Opened: &cubebox.TerminalOpened{
		SessionId: testTerminalSessionID,
	}}}
	pushTerminalServerFrame(t, stream, openedFrame)
	_ = readTerminalBinary(t, connection)
	event := <-openedEvents
	require.Equal(t, "request-a", event.RequestID)
	require.Equal(t, testTerminalSessionID, event.SessionID)
	require.Equal(t, "sandbox-canonical", event.SandboxID)
	require.Equal(t, "container-a", event.ContainerID)
	require.True(t, event.Resume)

	pushTerminalServerFrame(t, stream, openedFrame)
	_ = readTerminalBinary(t, connection)
	select {
	case duplicate := <-openedEvents:
		t.Fatalf("duplicate TerminalOpened logged a second success event: %+v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}

	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: terminalCloseUser}},
	})
	_, closeCode := readTerminalUntilClose(t, connection)
	require.Equal(t, websocket.CloseNormalClosure, closeCode)
	waitTerminalSignal(t, handlerDone, "relay handler")
}

func TestServeTerminalRelayDoesNotLogOpenedWhenCubeletRejectsOpen(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	openedEvents := make(chan terminalRelayOpenedEvent, 1)
	installTerminalRelayLogHooks(t,
		func(_ context.Context, event terminalRelayOpenedEvent) { openedEvents <- event },
		func(context.Context, terminalRelayWarning) {},
	)
	installTerminalRelayHooks(t, testTerminalRelaySettings(),
		func(context.Context, string) (string, string, error) {
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, _ string) (terminalRelayClient, error) {
			stream.ctx = ctx
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())
	require.NotNil(t, waitTerminalClientFrame(t, stream).GetOpen())

	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Error{Error: &cubebox.TerminalError{Code: terminalCodeInternal}},
	})
	message := readTerminalBinary(t, connection)
	require.JSONEq(t, "{\"type\":\"error\",\"code\":\"INTERNAL\"}", string(message[1:]))
	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: terminalCloseUser}},
	})
	_, closeCode := readTerminalUntilClose(t, connection)
	require.Equal(t, websocket.CloseNormalClosure, closeCode)
	waitTerminalSignal(t, handlerDone, "relay handler")
	select {
	case unexpected := <-openedEvents:
		t.Fatalf("terminal rejection logged a success event: %+v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTerminalRelayErrorKindDoesNotPreservePeerText(t *testing.T) {
	peerErr := status.Error(codes.Internal, "Bearer peer-secret terminal payload")
	warning := terminalRelayWarning{
		RequestID: "request-a",
		SessionID: testTerminalSessionID,
		SandboxID: "sandbox-canonical",
		ErrorKind: terminalRelayErrorKind(peerErr),
	}
	require.Equal(t, "DOWNSTREAM_ERROR", warning.ErrorKind)
	require.NotContains(t, warning.ErrorKind, "peer-secret")
	require.NotContains(t, warning.ErrorKind, "payload")
	require.Equal(t, "request-a", warning.RequestID)
	require.Equal(t, testTerminalSessionID, warning.SessionID)
}

func TestServeTerminalRelayNormalClientCloseWaitsForCubeletClose(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	installTerminalRelayHooks(t, testTerminalRelaySettings(),
		func(context.Context, string) (string, string, error) {
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, _ string) (terminalRelayClient, error) {
			stream.ctx = ctx
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())
	require.NotNil(t, waitTerminalClientFrame(t, stream).GetOpen())

	require.NoError(t, connection.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)))
	closeFrame := waitTerminalClientFrame(t, stream).GetClose()
	require.NotNil(t, closeFrame)
	require.Equal(t, terminalCloseUser, closeFrame.GetReason())
	waitTerminalSignal(t, stream.closeSendDone, "stream half-close")
	select {
	case <-handlerDone:
		t.Fatal("relay returned before cubelet acknowledged terminal close")
	case <-time.After(100 * time.Millisecond):
	}

	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: terminalCloseUser}},
	})
	_, closeCode := readTerminalUntilClose(t, connection)
	require.Equal(t, websocket.CloseNormalClosure, closeCode)
	waitTerminalSignal(t, handlerDone, "relay handler")
	waitTerminalSignal(t, stream.closeDone, "stream close")
}

func TestServeTerminalRelayCloseTimeoutCancelsDownstream(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	settings := testTerminalRelaySettings()
	settings.CloseTimeout = 50 * time.Millisecond
	installTerminalRelayHooks(t, settings,
		func(context.Context, string) (string, string, error) {
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, _ string) (terminalRelayClient, error) {
			stream.ctx = ctx
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())
	require.NotNil(t, waitTerminalClientFrame(t, stream).GetOpen())

	require.NoError(t, connection.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)))
	closeFrame := waitTerminalClientFrame(t, stream).GetClose()
	require.NotNil(t, closeFrame)
	require.Equal(t, terminalCloseUser, closeFrame.GetReason())
	waitTerminalSignal(t, stream.closeSendDone, "stream half-close")
	waitTerminalSignal(t, handlerDone, "relay close timeout")
	waitTerminalSignal(t, stream.closeDone, "stream close")
}

func TestServeTerminalRelayProtocolErrorClosesBothTransports(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	installTerminalRelayHooks(t, testTerminalRelaySettings(),
		func(context.Context, string) (string, string, error) {
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, _ string) (terminalRelayClient, error) {
			stream.ctx = ctx
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())
	require.NotNil(t, waitTerminalClientFrame(t, stream).GetOpen())

	require.NoError(t, connection.WriteMessage(websocket.TextMessage, []byte("not binary")))
	closeFrame := waitTerminalClientFrame(t, stream).GetClose()
	require.NotNil(t, closeFrame)
	require.Equal(t, terminalCloseProtocol, closeFrame.GetReason())
	waitTerminalSignal(t, stream.closeSendDone, "stream half-close")
	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		// Cubelet treats an untrusted client close reason as USER_CLOSED. The
		// relay must still report its own WebSocket framing error accurately.
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: terminalCloseUser}},
	})

	messages, closeCode := readTerminalUntilClose(t, connection)
	require.Equal(t, websocket.ClosePolicyViolation, closeCode)
	require.Len(t, messages, 1)
	require.JSONEq(t, "{\"type\":\"close\",\"reason\":\"PROTOCOL_ERROR\"}", string(messages[0][1:]))
	waitTerminalSignal(t, handlerDone, "relay handler")
	waitTerminalSignal(t, stream.closeDone, "stream close")
}

func TestServeTerminalRelayOversizedFrameUsesClose1009(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	settings := testTerminalRelaySettings()
	settings.MaxFrameBytes = 8
	installTerminalRelayHooks(t, settings,
		func(context.Context, string) (string, string, error) {
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, _ string) (terminalRelayClient, error) {
			stream.ctx = ctx
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())
	require.NotNil(t, waitTerminalClientFrame(t, stream).GetOpen())

	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage,
		append([]byte{terminalprotocol.ChannelStdin}, []byte("123456789")...)))
	closeFrame := waitTerminalClientFrame(t, stream).GetClose()
	require.NotNil(t, closeFrame)
	require.Equal(t, terminalCloseProtocol, closeFrame.GetReason())
	waitTerminalSignal(t, stream.closeSendDone, "stream half-close")
	pushTerminalServerFrame(t, stream, &cubebox.TerminalServerFrame{
		Frame: &cubebox.TerminalServerFrame_Close{Close: &cubebox.TerminalClose{Reason: terminalCloseProtocol}},
	})

	messages, closeCode := readTerminalUntilClose(t, connection)
	require.Equal(t, websocket.CloseMessageTooBig, closeCode)
	require.Empty(t, messages)
	waitTerminalSignal(t, handlerDone, "relay handler")
	waitTerminalSignal(t, stream.closeDone, "stream close")
}

func TestServeTerminalRelayAbnormalDisconnectCancelsAndJoinsPumps(t *testing.T) {
	stream := newFakeTerminalRelayStream()
	installTerminalRelayHooks(t, testTerminalRelaySettings(),
		func(context.Context, string) (string, string, error) {
			return "sandbox-canonical", "10.0.0.7:9999", nil
		},
		func(ctx context.Context, _ string) (terminalRelayClient, error) {
			stream.ctx = ctx
			return stream, nil
		},
	)
	server, handlerDone := startTerminalRelayTestServer(t)
	connection := dialTerminalRelay(t, server, validTerminalHeaders())
	require.NotNil(t, waitTerminalClientFrame(t, stream).GetOpen())

	require.NoError(t, connection.UnderlyingConn().Close())
	waitTerminalSignal(t, stream.closeSendDone, "stream half-close")
	waitTerminalSignal(t, handlerDone, "relay handler")
	waitTerminalSignal(t, stream.closeDone, "stream close")
	select {
	case frame := <-stream.sent:
		t.Fatalf("abnormal disconnect must detach without terminal close frame: %v", frame)
	default:
	}
}

// installTerminalSandboxRedis points the local cache at an in-memory Redis so
// the proxy-map lookup in terminalTargetHost misses cleanly (the cache is
// stale) instead of panicking on a nil Redis configuration.
func installTerminalSandboxRedis(t *testing.T) {
	t.Helper()
	server := miniredis.RunT(t)
	oldRedisConf := config.GetConfig().RedisConf
	config.GetConfig().RedisConf = &config.RedisConf{
		Nodes:       server.Addr(),
		MaxActive:   4,
		MaxIdle:     1,
		MaxRetry:    1,
		DbNo:        0,
		IdleTimeout: 30,
	}
	t.Cleanup(func() {
		config.GetConfig().RedisConf = oldRedisConf
	})
}

// TestResolveTerminalTargetBoundsStaleCacheResolution proves the stale-cache
// fallback cannot hang the relay: when the sandbox resolver blocks (a slow
// cluster scan), resolveTerminalTarget must return within
// terminalTargetResolveTimeout instead of waiting for the request lifetime.
func TestResolveTerminalTargetBoundsStaleCacheResolution(t *testing.T) {
	installTerminalSandboxRedis(t)
	oldSandboxResolver := terminalSandboxIDResolver
	oldTimeout := terminalTargetResolveTimeout
	terminalTargetResolveTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		terminalSandboxIDResolver = oldSandboxResolver
		terminalTargetResolveTimeout = oldTimeout
	})
	// A stale cache misses terminalTargetHost, so the fallback runs. Block
	// until the bounded context is cancelled, then surface the cancellation
	// as the underlying resolution error.
	terminalSandboxIDResolver = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	start := time.Now()
	_, _, err := resolveTerminalTarget(context.Background(), uuid.NewString())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, errTerminalTargetNotFound)
	require.GreaterOrEqual(t, elapsed, terminalTargetResolveTimeout,
		"resolution must wait for the bounded timeout before failing")
	require.Less(t, elapsed, 2*terminalTargetResolveTimeout,
		"resolution must not exceed the bounded timeout")
}

// TestServeTerminalRelayResolveTimeoutReturns404 proves the whole relay path
// is bounded: a stale cache that falls into a slow cluster scan fails the
// handshake with 404 (target not found) inside the resolve timeout instead of
// hanging the request, and never reaches the terminal opener.
func TestServeTerminalRelayResolveTimeoutReturns404(t *testing.T) {
	installTerminalSandboxRedis(t)
	oldSandboxResolver := terminalSandboxIDResolver
	oldTimeout := terminalTargetResolveTimeout
	terminalTargetResolveTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		terminalSandboxIDResolver = oldSandboxResolver
		terminalTargetResolveTimeout = oldTimeout
	})
	terminalSandboxIDResolver = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	installTerminalRelayHooks(t, testTerminalRelaySettings(), terminalTargetResolver,
		func(context.Context, string) (terminalRelayClient, error) {
			t.Fatal("terminal opener must not be reached when resolution times out")
			return nil, errors.New("unexpected opener call")
		})

	headers := validTerminalHeaders()
	headers.Set(HeaderTerminalSandbox, uuid.NewString())
	req := httptest.NewRequest(http.MethodGet, "http://cubemaster/internal/terminal/relay", nil)
	req.Header = headers
	response := httptest.NewRecorder()

	start := time.Now()
	serveTerminalRelay(response, req)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusNotFound, response.Code)
	require.GreaterOrEqual(t, elapsed, terminalTargetResolveTimeout,
		"relay must wait for the bounded resolution timeout before failing")
	require.Less(t, elapsed, 2*terminalTargetResolveTimeout,
		"relay must not hang beyond the bounded resolution timeout")
}
