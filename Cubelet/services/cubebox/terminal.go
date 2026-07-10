// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Interactive terminal endpoint for CubeAPI. The listener is intentionally
// disabled until a shared proxy token is configured; it is never a public
// sandbox endpoint.

package cubebox

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	terminalProxyTokenEnv = "CUBE_TERMINAL_PROXY_TOKEN"
	terminalListenEnv     = "CUBELET_TERMINAL_LISTEN_ADDR"
	terminalIdleEnv       = "CUBELET_TERMINAL_IDLE_TIMEOUT_SECS"
	defaultTerminalListen = "0.0.0.0:10555"
	defaultTerminalIdle   = 30 * time.Minute
	terminalFIFOPath      = "/data/cubelet/fifo"
)

var terminalUpgrader = websocket.Upgrader{
	// CubeAPI, not a browser, is the only supported peer. The mandatory shared
	// credential below is the authorization boundary, so origin is irrelevant.
	CheckOrigin: func(*http.Request) bool { return true },
}

type terminalControl struct {
	Type string `json:"type"`
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

type lockedTerminalConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *lockedTerminalConn) write(messageType int, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(messageType, payload)
}

func (s *service) startTerminalServer() {
	secret := strings.TrimSpace(os.Getenv(terminalProxyTokenEnv))
	if secret == "" {
		CubeLog.Infof("Cubelet terminal server disabled: %s is not configured", terminalProxyTokenEnv)
		return
	}
	address := strings.TrimSpace(os.Getenv(terminalListenEnv))
	if address == "" {
		address = defaultTerminalListen
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		CubeLog.Errorf("Cubelet terminal server disabled: listen on %s failed: %v", address, err)
		return
	}

	server := &http.Server{
		Handler:           http.HandlerFunc(s.serveTerminal),
		ReadHeaderTimeout: 10 * time.Second,
	}
	CubeLog.Infof("Cubelet terminal server listening on %s", address)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			CubeLog.Errorf("Cubelet terminal server stopped: %v", err)
		}
	}()
}

func (s *service) serveTerminal(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/terminal" {
		http.NotFound(w, r)
		return
	}
	if !terminalProxyAuthorized(r.Header.Get("X-Cube-Terminal-Token")) {
		http.Error(w, "terminal proxy authorization failed", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	sandboxID := strings.TrimSpace(query.Get("sandbox_id"))
	containerID := strings.TrimSpace(query.Get("container_id"))
	shell, err := terminalShell(query.Get("shell"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sandboxID == "" || containerID == "" {
		http.Error(w, "sandbox_id and container_id are required", http.StatusBadRequest)
		return
	}

	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if err := s.runTerminal(conn, sandboxID, containerID, shell); err != nil {
		_ = (&lockedTerminalConn{conn: conn}).write(websocket.TextMessage, terminalEvent("error", err.Error()))
	}
}

func terminalProxyAuthorized(candidate string) bool {
	expected := strings.TrimSpace(os.Getenv(terminalProxyTokenEnv))
	if expected == "" || candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(candidate)) == 1
}

func terminalShell(value string) (string, error) {
	shell := strings.TrimSpace(value)
	if shell == "" {
		shell = "/bin/sh"
	}
	switch shell {
	case "/bin/sh", "/bin/bash":
		return shell, nil
	default:
		return "", fmt.Errorf("shell must be /bin/sh or /bin/bash")
	}
}

func terminalIdleTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv(terminalIdleEnv))
	if value == "" {
		return defaultTerminalIdle
	}
	seconds, err := time.ParseDuration(value + "s")
	if err != nil || seconds <= 0 {
		return defaultTerminalIdle
	}
	return seconds
}

func (s *service) runTerminal(conn *websocket.Conn, sandboxID, containerID, shell string) error {
	ctx := context.Background()
	sandbox, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("sandbox %s not found: %w", sandboxID, err)
	}
	if sandbox.Namespace != "" {
		ctx = namespaces.WithNamespace(ctx, sandbox.Namespace)
	}
	container, err := sandbox.Get(containerID)
	if err != nil {
		return fmt.Errorf("container %s not found: %w", containerID, err)
	}
	task, err := container.Container.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("container %s is not running: %w", containerID, err)
	}

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	request := &cubebox.ExecCubeSandboxRequest{
		RequestID:   "web-terminal-" + uuid.NewString(),
		SandboxId:   sandboxID,
		ContainerId: containerID,
		Terminal:    true,
		Args:        []string{shell, "-i"},
	}
	processSpec, err := generateExecProcessSpec(ctx, task, request)
	if err != nil {
		return fmt.Errorf("create terminal process spec: %w", err)
	}
	creator := cio.NewCreator(
		cio.WithStreams(stdinReader, stdoutWriter, nil),
		cio.WithTerminal,
		cio.WithFIFODir(terminalFIFOPath),
	)
	process, err := task.Exec(ctx, "web-terminal-"+uuid.NewString(), processSpec, creator)
	if err != nil {
		return fmt.Errorf("create terminal process: %w", err)
	}
	if err := process.Start(ctx); err != nil {
		return fmt.Errorf("start terminal process: %w", err)
	}
	defer func() {
		_ = stdinWriter.Close()
		_ = stdoutWriter.Close()
		_ = process.CloseIO(ctx, containerd.WithStdinCloser)
		_, _ = process.Delete(ctx, containerd.WithProcessKill)
	}()

	terminal := &lockedTerminalConn{conn: conn}
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			count, readErr := stdoutReader.Read(buffer)
			if count > 0 {
				if err := terminal.write(websocket.BinaryMessage, buffer[:count]); err != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	wait, err := process.Wait(ctx)
	if err == nil {
		go func() {
			status := <-wait
			_ = terminal.write(websocket.TextMessage, terminalEvent("exit", fmt.Sprintf("process exited (%d)", status.ExitCode())))
		}()
	}

	idleTimeout := terminalIdleTimeout()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		messageType, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			if websocket.IsUnexpectedCloseError(readErr, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.G(ctx).Warnf("terminal websocket closed for sandbox %s container %s: %v", sandboxID, containerID, readErr)
			}
			return nil
		}
		if messageType == websocket.TextMessage && applyTerminalControl(process, payload) {
			continue
		}
		if messageType == websocket.TextMessage || messageType == websocket.BinaryMessage {
			if _, err := stdinWriter.Write(payload); err != nil {
				return fmt.Errorf("write terminal input: %w", err)
			}
		}
	}
}

func applyTerminalControl(process containerd.Process, payload []byte) bool {
	var control terminalControl
	if json.Unmarshal(payload, &control) != nil || control.Type != "resize" {
		return false
	}
	if control.Cols == 0 || control.Rows == 0 || control.Cols > 500 || control.Rows > 1_000 {
		return true
	}
	_ = process.Resize(context.Background(), control.Cols, control.Rows)
	return true
}

func terminalEvent(kind, message string) []byte {
	payload, _ := json.Marshal(map[string]string{"type": kind, "message": message})
	return payload
}
