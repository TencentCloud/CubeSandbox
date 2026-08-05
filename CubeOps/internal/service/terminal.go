// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var terminalServiceSandboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

const (
	terminalConnectContentType = "application/connect+json"
	terminalConnectVersion     = "1"
	terminalAuth               = "Basic cm9vdDo="
	terminalMaxEnvelopeSize    = 8 << 20
	terminalOutputChunkSize    = 64 << 10
	terminalSignalSIGKILL      = "SIGNAL_SIGKILL"
)

// TerminalSize is the row/column size of an interactive pseudo-terminal.
type TerminalSize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// NormalizeTerminalSize applies safe defaults and bounds values accepted from
// a browser. Bounding dimensions prevents pathological allocations in shells
// and terminal applications while still supporting very large displays.
func NormalizeTerminalSize(size TerminalSize) TerminalSize {
	if size.Rows <= 0 {
		size.Rows = 24
	}
	if size.Cols <= 0 {
		size.Cols = 80
	}
	if size.Rows > 300 {
		size.Rows = 300
	}
	if size.Cols > 500 {
		size.Cols = 500
	}
	return size
}

// TerminalExit describes why the remote PTY stream ended.
type TerminalExit struct {
	ExitCode *int
	Error    error
}

// TerminalSession is the handler-facing contract for one envd PTY. The
// concrete implementation owns the streaming response body and serializes
// control operations through envd's unary Connect endpoints.
type TerminalSession interface {
	PID() int
	Output() <-chan []byte
	Done() <-chan TerminalExit
	SendInput(context.Context, []byte) error
	Resize(context.Context, TerminalSize) error
	Kill(context.Context) error
	Close() error
}

// TerminalBackend opens terminal sessions. It is intentionally small so the
// WebSocket handler can be tested without a live sandbox.
type TerminalBackend interface {
	Open(context.Context, string, TerminalSize) (TerminalSession, error)
}

// TerminalClient connects CubeOps to envd through CubeProxy's host-routing
// path. streamHTTP has no whole-request timeout because a terminal is a
// long-lived stream; controlHTTP has a bounded timeout for input/resize/kill.
type TerminalClient struct {
	proxyURL    string
	domain      string
	streamHTTP  *http.Client
	controlHTTP *http.Client
}

// NewTerminalClient creates a production terminal client.
func NewTerminalClient(proxyURL, domain string) *TerminalClient {
	transport := &http.Transport{MaxIdleConnsPerHost: 32}
	return NewTerminalClientWithHTTP(
		proxyURL,
		domain,
		&http.Client{Transport: transport},
		&http.Client{Transport: transport, Timeout: 10 * time.Second},
	)
}

// NewTerminalClientWithHTTP creates a terminal client with injectable HTTP
// clients. It is exported for focused integration tests.
func NewTerminalClientWithHTTP(proxyURL, domain string, streamHTTP, controlHTTP *http.Client) *TerminalClient {
	if strings.TrimSpace(proxyURL) == "" {
		proxyURL = "http://127.0.0.1"
	}
	if streamHTTP == nil {
		streamHTTP = &http.Client{}
	}
	if controlHTTP == nil {
		controlHTTP = &http.Client{Timeout: 10 * time.Second}
	}
	return &TerminalClient{
		proxyURL:    strings.TrimRight(proxyURL, "/"),
		domain:      strings.TrimSpace(domain),
		streamHTTP:  streamHTTP,
		controlHTTP: controlHTTP,
	}
}

// Open starts an interactive login shell in the sandbox and returns after
// envd emits its start event, so callers know the PTY PID before announcing a
// ready WebSocket session.
func (c *TerminalClient) Open(ctx context.Context, sandboxID string, size TerminalSize) (TerminalSession, error) {
	if !terminalServiceSandboxIDPattern.MatchString(sandboxID) {
		return nil, errors.New("invalid sandbox ID")
	}
	if c.domain == "" {
		return nil, errors.New("sandbox domain is required")
	}
	size = NormalizeTerminalSize(size)
	payload := terminalStartRequest{
		Process: terminalProcessConfig{
			Cmd:  "/bin/bash",
			Args: []string{"-i", "-l"},
			Envs: map[string]string{
				"TERM":   "xterm-256color",
				"LANG":   "C.UTF-8",
				"LC_ALL": "C.UTF-8",
			},
			Cwd: "/root",
		},
		PTY: terminalPTYConfig{Size: size},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal terminal start request: %w", err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	req, err := c.newRequest(streamCtx, http.MethodPost, sandboxID, "/process.Process/Start", encodeTerminalEnvelope(raw))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", terminalConnectContentType)
	req.Header.Set("Connect-Protocol-Version", terminalConnectVersion)
	req.Header.Set("Connect-Content-Encoding", "identity")

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open envd PTY: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		cancel()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("envd PTY start returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	pid, err := readTerminalStartPID(resp.Body)
	if err != nil {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("read envd PTY start event: %w", err)
	}

	s := &envdTerminalSession{
		pid:       pid,
		sandboxID: sandboxID,
		client:    c,
		ctx:       streamCtx,
		cancel:    cancel,
		body:      resp.Body,
		output:    make(chan []byte, 64),
		done:      make(chan TerminalExit, 1),
	}
	go s.readLoop()
	return s, nil
}

func (c *TerminalClient) newRequest(ctx context.Context, method, sandboxID, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.proxyURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// net/http treats Host specially: setting req.Header["Host"] is ignored.
	req.Host = fmt.Sprintf("%d-%s.%s", EnvdPort, sandboxID, c.domain)
	req.Header.Set("Authorization", terminalAuth)
	return req, nil
}

func (c *TerminalClient) unary(ctx context.Context, sandboxID, method string, payload any, allowNotFound bool) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, sandboxID, "/process.Process/"+method, raw)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", terminalConnectVersion)
	resp, err := c.controlHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("envd %s request failed: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusBadRequest {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if allowNotFound && terminalNotFound(resp.StatusCode, body) {
		return nil
	}
	return fmt.Errorf("envd %s returned HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(body)))
}

type envdTerminalSession struct {
	pid       int
	sandboxID string
	client    *TerminalClient
	ctx       context.Context
	cancel    context.CancelFunc
	body      io.ReadCloser
	output    chan []byte
	done      chan TerminalExit
	closeOnce sync.Once
}

func (s *envdTerminalSession) PID() int                  { return s.pid }
func (s *envdTerminalSession) Output() <-chan []byte     { return s.output }
func (s *envdTerminalSession) Done() <-chan TerminalExit { return s.done }

func (s *envdTerminalSession) SendInput(ctx context.Context, data []byte) error {
	return s.client.unary(ctx, s.sandboxID, "SendInput", terminalInputRequest{
		Process: terminalProcessSelector{PID: s.pid},
		Input:   terminalInput{PTY: base64.StdEncoding.EncodeToString(data)},
	}, false)
}

func (s *envdTerminalSession) Resize(ctx context.Context, size TerminalSize) error {
	return s.client.unary(ctx, s.sandboxID, "Update", terminalUpdateRequest{
		Process: terminalProcessSelector{PID: s.pid},
		PTY:     terminalPTYConfig{Size: NormalizeTerminalSize(size)},
	}, false)
}

func (s *envdTerminalSession) Kill(ctx context.Context) error {
	return s.client.unary(ctx, s.sandboxID, "SendSignal", terminalSignalRequest{
		Process: terminalProcessSelector{PID: s.pid},
		Signal:  terminalSignalSIGKILL,
	}, true)
}

func (s *envdTerminalSession) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.cancel()
		closeErr = s.body.Close()
	})
	return closeErr
}

func (s *envdTerminalSession) readLoop() {
	defer close(s.output)
	defer close(s.done)
	defer s.Close()

	exit := TerminalExit{}
	for {
		event, endStream, err := readTerminalEvent(s.body)
		if err != nil {
			if s.ctx.Err() == nil {
				exit.Error = err
			}
			s.done <- exit
			return
		}
		if endStream {
			s.done <- exit
			return
		}
		if event == nil {
			continue
		}
		if event.Data != nil && event.Data.PTY != "" {
			data, err := base64.StdEncoding.DecodeString(event.Data.PTY)
			if err != nil {
				exit.Error = fmt.Errorf("decode envd PTY output: %w", err)
				s.done <- exit
				return
			}
			for len(data) > 0 {
				n := len(data)
				if n > terminalOutputChunkSize {
					n = terminalOutputChunkSize
				}
				chunk := append([]byte(nil), data[:n]...)
				select {
				case s.output <- chunk:
				case <-s.ctx.Done():
					s.done <- exit
					return
				}
				data = data[n:]
			}
		}
		if event.End != nil {
			if code, ok := terminalExitCode(event.End); ok {
				exit.ExitCode = &code
			}
			if event.End.Error != "" {
				exit.Error = errors.New(event.End.Error)
			}
			s.done <- exit
			return
		}
	}
}

func encodeTerminalEnvelope(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func readTerminalStartPID(r io.Reader) (int, error) {
	for {
		event, endStream, err := readTerminalEvent(r)
		if err != nil {
			return 0, err
		}
		if endStream {
			return 0, errors.New("stream closed before start event")
		}
		if event != nil && event.Start != nil && event.Start.PID > 0 {
			return event.Start.PID, nil
		}
	}
}

func readTerminalEvent(r io.Reader) (*terminalProcessEvent, bool, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, false, err
	}
	flags := header[0]
	length := binary.BigEndian.Uint32(header[1:])
	if length > terminalMaxEnvelopeSize {
		return nil, false, fmt.Errorf("envd terminal frame is too large: %d bytes", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, false, err
	}
	if flags&0x01 != 0 {
		return nil, false, errors.New("compressed envd terminal frames are unsupported")
	}
	if flags&0x02 != 0 {
		var trailer struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if len(payload) > 0 && json.Unmarshal(payload, &trailer) == nil && trailer.Error != nil {
			return nil, true, fmt.Errorf("envd terminal stream error %s: %s", trailer.Error.Code, trailer.Error.Message)
		}
		return nil, true, nil
	}
	var response terminalProcessResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, false, fmt.Errorf("decode envd terminal event: %w", err)
	}
	return response.Event, false, nil
}

func terminalNotFound(status int, body []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	var response struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(body, &response) == nil && strings.EqualFold(response.Code, "not_found")
}

func terminalExitCode(end *terminalProcessEndEvent) (int, bool) {
	if end.ExitCode != nil {
		return *end.ExitCode, true
	}
	if end.ExitCodeSnake != nil {
		return *end.ExitCodeSnake, true
	}
	if strings.HasPrefix(strings.TrimSpace(end.Status), "exit status ") {
		var code int
		if _, err := fmt.Sscanf(strings.TrimSpace(end.Status), "exit status %d", &code); err == nil {
			return code, true
		}
	}
	if end.Exited {
		return 0, true
	}
	return 0, false
}

type terminalProcessResponse struct {
	Event *terminalProcessEvent `json:"event"`
}

type terminalProcessEvent struct {
	Start     *terminalProcessStartEvent `json:"start,omitempty"`
	Data      *terminalProcessDataEvent  `json:"data,omitempty"`
	End       *terminalProcessEndEvent   `json:"end,omitempty"`
	Keepalive *struct{}                  `json:"keepalive,omitempty"`
}

type terminalProcessStartEvent struct {
	PID int `json:"pid"`
}

type terminalProcessDataEvent struct {
	PTY string `json:"pty,omitempty"`
}

type terminalProcessEndEvent struct {
	ExitCode      *int   `json:"exitCode,omitempty"`
	ExitCodeSnake *int   `json:"exit_code,omitempty"`
	Exited        bool   `json:"exited,omitempty"`
	Status        string `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
}

type terminalStartRequest struct {
	Process terminalProcessConfig `json:"process"`
	PTY     terminalPTYConfig     `json:"pty"`
}

type terminalProcessConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args"`
	Envs map[string]string `json:"envs"`
	Cwd  string            `json:"cwd,omitempty"`
}

type terminalPTYConfig struct {
	Size TerminalSize `json:"size"`
}

type terminalProcessSelector struct {
	PID int `json:"pid"`
}

type terminalInput struct {
	PTY string `json:"pty"`
}

type terminalInputRequest struct {
	Process terminalProcessSelector `json:"process"`
	Input   terminalInput           `json:"input"`
}

type terminalUpdateRequest struct {
	Process terminalProcessSelector `json:"process"`
	PTY     terminalPTYConfig       `json:"pty"`
}

type terminalSignalRequest struct {
	Process terminalProcessSelector `json:"process"`
	Signal  string                  `json:"signal"`
}
