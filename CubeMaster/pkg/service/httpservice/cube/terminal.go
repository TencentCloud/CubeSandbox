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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"google.golang.org/grpc"
)

const (
	terminalIdleTimeout = 30 * time.Minute
	terminalMaxFrame    = 64 * 1024
)

var terminalUpgrader = websocket.Upgrader{
	// CubeAPI is the only public caller in the standard deployment. It has
	// already authenticated the browser before opening this internal hop.
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
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: "sandbox not found"})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, closeStream, err := openTerminalStream(ctx, hostIP)
	if err != nil {
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: "terminal backend unavailable"})
		return
	}
	defer closeStream()
	if err := stream.Send(&cubebox.TerminalClientMessage{Payload: &cubebox.TerminalClientMessage_Open{Open: &cubebox.TerminalOpenRequest{
		RequestId: uuid.NewString(), SandboxId: first.SandboxID, ContainerId: first.ContainerID,
		Cols: first.Cols, Rows: first.Rows,
	}}}); err != nil {
		writeTerminalFrame(conn, terminalServerFrame{Type: "error", Message: "unable to start terminal"})
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
				_ = write(terminalServerFrame{Type: "output", Data: string(payload.Output)})
			case *cubebox.TerminalServerMessage_Exit:
				_ = write(terminalServerFrame{Type: "exit", ExitCode: payload.Exit.GetExitCode()})
				return
			case *cubebox.TerminalServerMessage_Error:
				_ = write(terminalServerFrame{Type: "error", Message: payload.Error.GetMessage()})
				return
			}
		}
	}()

	for {
		var frame terminalClientFrame
		if err := readTerminalFrame(conn, &frame); err != nil {
			return
		}
		if err := conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout)); err != nil {
			return
		}
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
			// The read deadline above is the keepalive; no backend operation needed.
		default:
			_ = write(terminalServerFrame{Type: "error", Message: "unsupported terminal frame"})
			return
		}
		select {
		case <-backendDone:
			return
		default:
		}
	}
}

func terminalGatewayAuthorized(r *http.Request) bool {
	expected := config.GetConfig().Common.TerminalGatewayToken
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
	stream, err := cubebox.NewCubeboxMgrClient(conn.Value()).AttachTerminal(ctx, grpc.WaitForReady(true))
	if err != nil {
		conn.Close()
		return nil, func() {}, err
	}
	return stream, func() { _ = conn.Close() }, nil
}
