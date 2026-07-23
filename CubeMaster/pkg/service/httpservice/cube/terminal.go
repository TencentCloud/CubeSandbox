// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/terminalprotocol"
)

const (
	terminalGatewayTokenEnv    = "CUBE_TERMINAL_GATEWAY_TOKEN"
	terminalGatewayTokenHeader = "X-Cube-Terminal-Gateway"
	terminalMaxFrameBytes      = 64 * 1024
	terminalReadTimeout        = 35 * time.Minute
)

var terminalUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	// The gateway token validated in handleTerminalAction is the authoritative
	// authentication boundary. Requiring an empty Origin additionally rejects
	// direct browser connections as defense in depth.
	CheckOrigin: func(request *http.Request) bool {
		return request.Header.Get("Origin") == ""
	},
}

func handleTerminalAction(c *gin.Context) {
	expectedToken := strings.TrimSpace(os.Getenv(terminalGatewayTokenEnv))
	if expectedToken == "" {
		http.Error(c.Writer, "terminal gateway is not configured", http.StatusServiceUnavailable)
		return
	}
	if !terminalprotocol.GatewayTokenMatches(expectedToken, c.GetHeader(terminalGatewayTokenHeader)) {
		http.Error(c.Writer, "terminal gateway is not authorized", http.StatusUnauthorized)
		return
	}
	// This is defense in depth only: the gateway token above authenticates the
	// trusted CubeOps caller even if a proxy strips or rewrites Origin.
	if c.GetHeader("Origin") != "" {
		http.Error(c.Writer, "browser-originated terminal connections are not allowed", http.StatusForbidden)
		return
	}

	connection, err := terminalUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	connection.SetReadLimit(terminalMaxFrameBytes)
	factory := &terminalBackendFactory{}
	if err := terminalprotocol.Relay(c.Request.Context(), &terminalDeadlineSocket{Conn: connection}, factory); err != nil {
		log.G(c.Request.Context()).Warnf("terminal relay closed with error: %v", err)
	}
}

// terminalDeadlineSocket detects dead transports independently of the
// application-level idle timeout in terminalprotocol.Relay. CubeOps sends a
// text keepalive every 20 seconds, so a live connection refreshes this deadline
// without counting keepalives as terminal activity.
type terminalDeadlineSocket struct {
	*websocket.Conn
}

func (socket *terminalDeadlineSocket) ReadMessage() (int, []byte, error) {
	if err := socket.SetReadDeadline(time.Now().Add(terminalReadTimeout)); err != nil {
		return 0, nil, err
	}
	return socket.Conn.ReadMessage()
}

type terminalBackendFactory struct{}

func (f *terminalBackendFactory) Open(ctx context.Context, open terminalprotocol.ClientControl) (terminalprotocol.Backend, error) {
	hostIP, ok := terminalSandboxHost(ctx, open.SandboxID)
	if !ok {
		return nil, errors.New("terminal target is unavailable")
	}
	stream, err := cubelet.OpenTerminal(ctx, cubelet.GetCubeletAddr(hostIP))
	if err != nil {
		return nil, errors.New("terminal backend is unavailable")
	}
	log.G(ctx).WithFields(map[string]interface{}{
		"requestID":   open.RequestID,
		"sessionID":   open.SessionID,
		"sandboxID":   open.SandboxID,
		"containerID": open.ContainerID,
		"hostIP":      hostIP,
	}).Info("terminal relay opened")
	return stream, nil
}

func terminalSandboxHost(ctx context.Context, sandboxID string) (string, bool) {
	if sandbox := localcache.GetSandboxCache(sandboxID); sandbox != nil && strings.TrimSpace(sandbox.HostIP) != "" {
		return sandbox.HostIP, true
	}
	if proxy, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok && proxy != nil && strings.TrimSpace(proxy.HostIP) != "" {
		return proxy.HostIP, true
	}
	return "", false
}
