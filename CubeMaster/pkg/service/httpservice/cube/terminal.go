// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

const (
	terminalIdleTimeout  = 30 * time.Minute
	terminalWriteTimeout = 10 * time.Second
	terminalMaxFrame     = 64 * 1024
	terminalOpenError    = "unable to open terminal"
)

var terminalUpgrader = websocket.Upgrader{
	// This is an authenticated server-to-server hop, so the browser Origin is
	// not an authorization boundary and CubeAPI may omit it. CubeMaster must
	// remain on a private network: exposing this endpoint would let a client
	// with a stolen gateway token connect from any browser origin.
	CheckOrigin: func(*http.Request) bool { return true },
}

type terminalClientFrame struct {
	Type        string `json:"type"`
	SandboxID   string `json:"sandboxId,omitempty"`
	ContainerID string `json:"containerId,omitempty"`
	Data        string `json:"data,omitempty"`
	Cols        uint32 `json:"cols,omitempty"`
	Rows        uint32 `json:"rows,omitempty"`
}

type terminalServerFrame struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode int32  `json:"exitCode,omitempty"`
}

// TerminalWebSocketHandler bridges the authenticated API gateway WebSocket to
// Cubelet's AttachTerminal stream. The initial frame binds the session to one
// sandbox/container; later frames can only carry input, resize, or keepalive.
func TerminalWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	if !terminalGatewayAuthorized(r) {
		http.Error(w, "terminal gateway is not authorized", http.StatusUnauthorized)
		return
	}
	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(terminalMaxFrame)
	_ = conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
	})

	var first terminalClientFrame
	if err := readTerminalFrame(conn, &first); err != nil {
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: err.Error()})
		return
	}
	if first.Type != "open" || first.SandboxID == "" || first.ContainerID == "" {
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: "open frame must provide sandboxId and containerId"})
		return
	}

	hostIP, ok := terminalSandboxHost(r.Context(), first.SandboxID)
	if !ok {
		log.G(r.Context()).Warnf("terminal sandbox lookup failed: sandbox_id=%s", first.SandboxID)
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: terminalOpenError})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, closeStream, err := openTerminalStream(ctx, hostIP)
	if err != nil {
		log.G(r.Context()).Warnf("terminal backend connection failed: sandbox_id=%s host_ip=%s error=%v", first.SandboxID, hostIP, err)
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: terminalOpenError})
		return
	}
	defer closeStream()
	if err := stream.Send(&cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Open{Open: &cubebox.TerminalOpenRequest{
		RequestId: uuid.NewString(), SandboxId: first.SandboxID, ContainerId: first.ContainerID,
		Cols: first.Cols, Rows: first.Rows,
	}}}); err != nil {
		log.G(r.Context()).Warnf("terminal open request failed: sandbox_id=%s container_id=%s error=%v", first.SandboxID, first.ContainerID, err)
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: terminalOpenError})
		return
	}

	var writeMu sync.Mutex
	write := func(frame terminalServerFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeTerminalFrame(conn, frame)
	}
	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		for {
			message, recvErr := stream.Recv()
			if recvErr != nil {
				if !errors.Is(recvErr, io.EOF) && ctx.Err() == nil {
					_ = write(terminalServerFrame{Type: "error", Message: "terminal backend disconnected"})
				}
				return
			}
			switch payload := message.GetPayload().(type) {
			case *cubebox.TerminalServerMessage_Output:
				if err := write(terminalServerFrame{Type: "output", Data: string(payload.Output)}); err != nil {
					return
				}
			case *cubebox.TerminalServerMessage_Exit:
				_ = write(terminalServerFrame{Type: "exit", ExitCode: payload.Exit.GetExitCode()})
				return
			case *cubebox.TerminalServerMessage_Error:
				_ = write(terminalServerFrame{Type: "error", Message: payload.Error.GetMessage()})
				return
			}
		}
	}()

	clientFrames := make(chan terminalClientFrame)
	clientReadDone := make(chan error, 1)
	go func() {
		for {
			var frame terminalClientFrame
			if err := readTerminalFrame(conn, &frame); err != nil {
				clientReadDone <- err
				return
			}
			if err := conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout)); err != nil {
				clientReadDone <- err
				return
			}
			select {
			case clientFrames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-backendDone:
			return
		case <-clientReadDone:
			return
		case frame := <-clientFrames:
			switch frame.Type {
			case "input":
				if err := stream.Send(&cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Stdin{Stdin: []byte(frame.Data)}}); err != nil {
					return
				}
			case "resize":
				if err := stream.Send(&cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Resize{Resize: &cubebox.TerminalResize{Cols: frame.Cols, Rows: frame.Rows}}}); err != nil {
					return
				}
			case "keepalive":
				// The reader refreshed the idle deadline; no backend operation needed.
			default:
				_ = write(terminalServerFrame{Type: "error", Message: "unsupported terminal frame"})
				return
			}
		}
	}
}

func terminalGatewayAuthorized(r *http.Request) bool {
	expected := config.GetTerminalGatewayToken()
	return terminalGatewayAuthorizedWithToken(r, expected)
}

func terminalGatewayAuthorizedWithToken(r *http.Request, expected string) bool {
	return expected != "" && r.Header.Get("X-Cube-Terminal-Gateway") == expected
}

func readTerminalFrame(conn *websocket.Conn, frame *terminalClientFrame) error {
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if messageType != websocket.TextMessage {
		return errors.New("terminal frames must be JSON text")
	}
	if err := json.Unmarshal(data, frame); err != nil {
		return errors.New("invalid terminal frame")
	}
	return nil
}

func writeTerminalFrame(conn *websocket.Conn, frame terminalServerFrame) error {
	if err := conn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(frame)
}

func terminalSandboxHost(ctx context.Context, sandboxID string) (string, bool) {
	if sandbox := localcache.GetSandboxCache(sandboxID); sandbox != nil && sandbox.HostIP != "" {
		return sandbox.HostIP, true
	}
	if proxy, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok && proxy != nil && proxy.HostIP != "" {
		return proxy.HostIP, true
	}
	return "", false
}

func openTerminalStream(ctx context.Context, hostIP string) (cubebox.CubeboxMgr_AttachTerminalClient, func(), error) {
	conn, err := grpcconn.GetWorkerConn(ctx, cubelet.GetCubeletAddr(hostIP))
	if err != nil {
		return nil, func() {}, err
	}
	stream, err := cubebox.NewCubeboxMgrClient(conn.Value()).AttachTerminal(ctx)
	if err != nil {
		conn.Close()
		return nil, func() {}, err
	}
	return stream, func() { _ = conn.Close() }, nil
}
