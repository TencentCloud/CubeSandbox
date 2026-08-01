// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package terminal implements the WebUI web terminal: it bridges a browser
// WebSocket to an interactive PTY inside a sandbox, reusing envd's
// process.Process Connect-JSON API through cube-proxy (the same data plane the
// SDKs use). No new RPCs are added to CubeMaster/Cubelet/CubeShim.
package terminal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	envdPort        = 49983
	envdAuth        = "Basic cm9vdDo=" // "root:"
	connectJSONType = "application/connect+json"

	// Connect stream frame flags.
	connectCompressedFlag = 0x01
	connectEndStreamFlag  = 0x02

	// unaryTimeout bounds the fire-and-forget control calls (SendInput /
	// Update / SendSignal). Streams are long-lived and are bounded by their
	// context instead.
	unaryTimeout = 15 * time.Second
)

// Client talks to envd's process.Process API inside a sandbox, routed through
// cube-proxy by Host header (<port>-<sandboxID>.<domain>), matching
// service.RunEnvdCommand. Unlike that helper it parses the Connect stream
// incrementally, which a PTY needs.
type Client struct {
	http     *http.Client
	proxyURL string
	domain   string
}

// NewClient creates an envd PTY client for the given sandbox domain.
func NewClient(domain string) *Client {
	proxyURL := os.Getenv("AGENTHUB_SANDBOX_PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1"
	}
	return &Client{
		// No global timeout: PTY streams stay open for the whole session.
		// Unary calls get a per-request context deadline instead.
		http:     &http.Client{},
		proxyURL: strings.TrimRight(proxyURL, "/"),
		domain:   domain,
	}
}

func (c *Client) hostFor(sandboxID string) string {
	return fmt.Sprintf("%d-%s.%s", envdPort, sandboxID, c.domain)
}

// Stream is a live PTY output stream. Output carries raw terminal bytes and is
// closed when the stream ends; after that, Exited/ExitCode/Err report why.
type Stream struct {
	PID    int
	Output chan []byte

	done     chan struct{}
	exited   bool
	exitCode int
	err      error
}

// Done is closed when the stream has fully terminated.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Result reports how the stream ended: whether the process exited, its exit
// code, and any transport/protocol error. Only valid after Done is closed.
func (s *Stream) Result() (exited bool, exitCode int, err error) {
	return s.exited, s.exitCode, s.err
}

// StartPTY creates a new interactive login shell PTY sized rows x cols and
// returns its output stream. The stream stays open until the process exits or
// ctx is cancelled.
func (c *Client) StartPTY(ctx context.Context, sandboxID string, rows, cols int) (*Stream, error) {
	payload := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-i", "-l"},
			"envs": map[string]string{
				"TERM":   "xterm-256color",
				"LANG":   "C.UTF-8",
				"LC_ALL": "C.UTF-8",
			},
		},
		"pty": map[string]interface{}{
			"size": map[string]int{"rows": rows, "cols": cols},
		},
	}
	return c.openStream(ctx, sandboxID, "Start", payload)
}

// ConnectPTY reattaches to a still-running PTY by pid and returns a fresh
// output stream.
func (c *Client) ConnectPTY(ctx context.Context, sandboxID string, pid int) (*Stream, error) {
	payload := map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
	}
	return c.openStream(ctx, sandboxID, "Connect", payload)
}

// SendInput writes raw bytes to the PTY master side.
func (c *Client) SendInput(ctx context.Context, sandboxID string, pid int, data []byte) error {
	return c.unary(ctx, sandboxID, "SendInput", map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"input":   map[string]string{"pty": base64.StdEncoding.EncodeToString(data)},
	})
}

// Resize changes the PTY window size.
func (c *Client) Resize(ctx context.Context, sandboxID string, pid, rows, cols int) error {
	return c.unary(ctx, sandboxID, "Update", map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"pty": map[string]interface{}{
			"size": map[string]int{"rows": rows, "cols": cols},
		},
	})
}

// Kill sends SIGKILL to the PTY process. A "not found" answer (the process
// already exited) is not an error.
func (c *Client) Kill(ctx context.Context, sandboxID string, pid int) error {
	err := c.unary(ctx, sandboxID, "SendSignal", map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"signal":  "SIGNAL_SIGKILL",
	})
	if err != nil && strings.Contains(err.Error(), "not_found") {
		return nil
	}
	return err
}

// openStream POSTs a streaming Connect-JSON request, waits for the start event
// to learn the PID, then keeps decoding output frames in a background
// goroutine.
func (c *Client) openStream(ctx context.Context, sandboxID, method string, payload interface{}) (*Stream, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", method, err)
	}

	// Connect envelope: [flags:1][len:4 BE][JSON]
	body := make([]byte, 5+len(raw))
	binary.BigEndian.PutUint32(body[1:5], uint32(len(raw)))
	copy(body[5:], raw)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.proxyURL+"/process.Process/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Host must be set via req.Host; req.Header.Set("Host", ...) is ignored.
	req.Host = c.hostFor(sandboxID)
	req.Header.Set("Content-Type", connectJSONType)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", envdAuth)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("envd %s request failed: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, fmt.Errorf("envd %s returned HTTP %d: %s", method, resp.StatusCode, string(respBody))
	}

	br := bufio.NewReader(resp.Body)
	pid, err := readStartPID(br, method)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}

	s := &Stream{
		PID:    pid,
		Output: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	go s.readLoop(ctx, br, resp.Body)
	return s, nil
}

// unary sends a plain application/json Connect request. Streaming
// content-types on unary endpoints get a 415 from envd, so the envelope is
// deliberately absent here.
func (c *Client) unary(ctx context.Context, sandboxID, method string, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	ctx, cancel := context.WithTimeout(ctx, unaryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.proxyURL+"/process.Process/"+method, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Host = c.hostFor(sandboxID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", envdAuth)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("envd %s request failed: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("envd %s returned HTTP %d: %s", method, resp.StatusCode, string(respBody))
	}
	return nil
}

// processEvent mirrors the envd ProcessEvent wire shape (only the fields the
// terminal needs).
type processEvent struct {
	Start *struct {
		PID int `json:"pid"`
	} `json:"start"`
	Data *struct {
		PTY string `json:"pty"`
	} `json:"data"`
	End *struct {
		ExitCode *int   `json:"exitCode"`
		Exited   bool   `json:"exited"`
		Status   string `json:"status"`
		Error    string `json:"error"`
	} `json:"end"`
}

type eventEnvelope struct {
	Event *processEvent `json:"event"`
}

// readFrame reads one Connect stream frame: [flags:1][len:4 BE][payload].
func readFrame(r io.Reader) (flags byte, payload []byte, err error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:5])
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// readStartPID consumes frames until the start event and returns the PID.
func readStartPID(r io.Reader, method string) (int, error) {
	for {
		flags, payload, err := readFrame(r)
		if err != nil {
			return 0, fmt.Errorf("envd %s: read stream: %w", method, err)
		}
		if flags&connectEndStreamFlag != 0 {
			return 0, fmt.Errorf("envd %s: stream closed before start event: %s", method, endStreamMessage(payload))
		}
		if flags&connectCompressedFlag != 0 {
			return 0, fmt.Errorf("envd %s: unsupported compressed stream frame", method)
		}
		var env eventEnvelope
		if json.Unmarshal(payload, &env) != nil || env.Event == nil {
			continue // keepalive / unknown frame
		}
		if env.Event.Start != nil {
			return env.Event.Start.PID, nil
		}
	}
}

// readLoop decodes output frames until the stream ends and records the outcome.
func (s *Stream) readLoop(ctx context.Context, r io.Reader, body io.Closer) {
	defer close(s.done)
	defer close(s.Output)
	defer body.Close()

	for {
		flags, payload, err := readFrame(r)
		if err != nil {
			// EOF without an end event: the transport dropped (proxy restart,
			// sandbox paused/killed, ctx cancelled). Report it unless we
			// already saw a clean exit.
			if !s.exited && ctx.Err() == nil {
				s.err = fmt.Errorf("pty stream interrupted: %w", err)
			}
			return
		}
		if flags&connectEndStreamFlag != 0 {
			if msg := endStreamMessage(payload); msg != "" && !s.exited {
				s.err = fmt.Errorf("envd stream error: %s", msg)
			}
			return
		}
		if flags&connectCompressedFlag != 0 {
			s.err = fmt.Errorf("unsupported compressed stream frame")
			return
		}

		var env eventEnvelope
		if json.Unmarshal(payload, &env) != nil || env.Event == nil {
			continue
		}
		ev := env.Event
		if ev.Data != nil && ev.Data.PTY != "" {
			raw, decErr := base64.StdEncoding.DecodeString(ev.Data.PTY)
			if decErr != nil {
				s.err = fmt.Errorf("decode pty output: %w", decErr)
				return
			}
			select {
			case s.Output <- raw:
			case <-ctx.Done():
				return
			}
		}
		if ev.End != nil {
			s.exited = true
			if ev.End.ExitCode != nil {
				s.exitCode = *ev.End.ExitCode
			}
			if ev.End.Error != "" {
				s.err = fmt.Errorf("pty exited with error: %s", ev.End.Error)
			}
		}
	}
}

// endStreamMessage extracts the error message (if any) from an end-of-stream
// frame payload.
func endStreamMessage(payload []byte) string {
	var v struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &v) != nil || v.Error == nil {
		return ""
	}
	if v.Error.Message != "" {
		return fmt.Sprintf("%s: %s", v.Error.Code, v.Error.Message)
	}
	return v.Error.Code
}
