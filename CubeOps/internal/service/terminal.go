// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

// Interactive PTY client for the in-guest envd agent. It reuses the same envd
// Connect endpoint, host addressing (`<port>-<sandboxId>.<domain>` via the
// local sandbox proxy) and Basic auth as RunEnvdCommand, but instead of
// buffering a one-shot command it opens a long-lived `process.Process/Start`
// PTY stream and drives it with the unary SendInput/Update/SendSignal calls.
//
// This is the whole backend for the WebUI terminal: no new CubeMaster/Cubelet
// RPC and no CubeAPI change — the terminal is an operational capability that
// lives entirely in CubeOps and speaks to envd, which already exposes a full
// interactive PTY API (see also sdk/go/pty.go).

import (
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
	"sync"
	"time"
)

const (
	// connectEndStreamFlag marks the trailing frame of a Connect server stream.
	connectEndStreamFlag = byte(0x02)
	// connectCompressedFlag marks a compressed frame; envd never sets it here
	// and we reject it rather than guess a codec.
	connectCompressedFlag = byte(0x01)
	// maxConnectEnvelopeSize bounds a single Connect frame so a hostile or
	// buggy envd cannot make us allocate without limit.
	maxConnectEnvelopeSize = 4 * 1024 * 1024

	// defaultTermEnvTERM etc. seed a usable interactive environment.
	//
	// terminalShell is /bin/sh rather than /bin/bash because minimal images
	// (Alpine, busybox-based) ship no bash at all and the session would fail to
	// start outright. terminalShellArgs upgrades to bash when the image has it,
	// so bash-based templates keep line editing and completion.
	terminalShell = "/bin/sh"
)

// terminalShellArgs execs an interactive login bash when the image provides one
// and otherwise stays on sh. The fallback uses an absolute /bin/sh so a broken
// or empty PATH cannot leave the session with no shell at all. exec replaces the
// wrapper in both branches, so no extra process lingers and the PTY is owned by
// the real shell.
var terminalShellArgs = []string{
	"-c",
	`b=$(command -v bash 2>/dev/null); ` +
		`if [ -n "$b" ]; then exec "$b" -i -l; else exec /bin/sh -i -l; fi`,
}

// terminalStreamHTTPClient has no overall timeout: the Start stream is
// long-lived and its lifecycle is bounded by the request context instead.
var terminalStreamHTTPClient = &http.Client{}

// terminalUnaryHTTPClient bounds the short SendInput/Update/SendSignal calls.
var terminalUnaryHTTPClient = &http.Client{Timeout: 15 * time.Second}

// PtySize is a pseudo-terminal window size.
type PtySize struct {
	Rows int
	Cols int
}

// PtyHandle is a live PTY stream inside a sandbox. Output() yields raw terminal
// bytes until the process exits or the stream is torn down; SendStdin/Resize/
// Kill drive it by PID.
type PtyHandle struct {
	pid       int
	sandboxID string
	domain    string

	output chan []byte
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	once   sync.Once

	mu       sync.Mutex
	exitCode *int
	errMsg   string
	exited   bool
}

// PID returns the PTY process id inside the sandbox.
func (h *PtyHandle) PID() int { return h.pid }

// Output returns the channel of raw PTY output chunks, closed when the stream ends.
func (h *PtyHandle) Output() <-chan []byte { return h.output }

// Done is closed once the read loop has fully finished.
func (h *PtyHandle) Done() <-chan struct{} { return h.done }

// ExitInfo reports the exit code (if envd sent one) and any error message.
func (h *PtyHandle) ExitInfo() (code int, hasCode bool, errMsg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exitCode != nil {
		return *h.exitCode, true, h.errMsg
	}
	return 0, false, h.errMsg
}

// Close tears down the stream without necessarily killing the process; callers
// that want the process gone must also call Kill.
func (h *PtyHandle) Close() {
	h.cancel()
	h.once.Do(func() { _ = h.body.Close() })
}

// SendStdin writes bytes to the PTY master side.
func (h *PtyHandle) SendStdin(ctx context.Context, data []byte) error {
	return h.unary(ctx, "SendInput", ptyInputRequest{
		Process: ptyProcessSelector{PID: h.pid},
		Input:   ptyInput{PTY: base64.StdEncoding.EncodeToString(data)},
	})
}

// Resize changes the PTY window size.
func (h *PtyHandle) Resize(ctx context.Context, size PtySize) error {
	return h.unary(ctx, "Update", ptyUpdateRequest{
		Process: ptyProcessSelector{PID: h.pid},
		PTY:     ptyConfig{Size: ptySizeWire{Rows: size.Rows, Cols: size.Cols}},
	})
}

// Kill sends SIGKILL to the PTY process. A missing process is not an error.
func (h *PtyHandle) Kill(ctx context.Context) error {
	return h.unary(ctx, "SendSignal", ptySignalRequest{
		Process: ptyProcessSelector{PID: h.pid},
		Signal:  "SIGNAL_SIGKILL",
	})
}

// OpenPTY starts an interactive login shell inside the sandbox and returns a
// handle streaming its output. maxSessionAge bounds the server-side stream
// lifetime (Connect-Timeout-Ms); the caller's ctx bounds the client side.
func OpenPTY(ctx context.Context, sandboxID, domain string, size PtySize, cwd string, maxSessionAge time.Duration) (*PtyHandle, error) {
	if size.Cols <= 0 {
		size.Cols = 80
	}
	if size.Rows <= 0 {
		size.Rows = 24
	}
	envs := map[string]string{
		"TERM":   "xterm-256color",
		"LANG":   "C.UTF-8",
		"LC_ALL": "C.UTF-8",
	}
	payload := ptyStartRequest{
		Process: ptyProcessConfig{
			Cmd:  terminalShell,
			Args: terminalShellArgs,
			Envs: envs,
			Cwd:  cwd,
		},
		PTY: ptyConfig{Size: ptySizeWire{Rows: size.Rows, Cols: size.Cols}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(ctx)
	req, err := newEnvdStreamRequest(streamCtx, sandboxID, domain, "Start", raw, maxSessionAge)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := terminalStreamHTTPClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open terminal stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("envd returned HTTP %d opening terminal: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	pid, err := readPtyStartPID(resp.Body)
	if err != nil {
		resp.Body.Close()
		cancel()
		return nil, err
	}

	h := &PtyHandle{
		pid:       pid,
		sandboxID: sandboxID,
		domain:    domain,
		output:    make(chan []byte, 64),
		done:      make(chan struct{}),
		ctx:       streamCtx,
		cancel:    cancel,
		body:      resp.Body,
	}
	go h.readLoop()
	return h, nil
}

func (h *PtyHandle) readLoop() {
	defer close(h.done)
	defer close(h.output)
	defer func() { h.once.Do(func() { _ = h.body.Close() }) }()

	for {
		ev, eos, err := readPtyEvent(h.body)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				h.recordErr(err.Error())
			}
			return
		}
		if eos {
			return
		}
		if ev == nil {
			continue
		}
		if ev.Data != nil && ev.Data.PTY != "" {
			decoded, decErr := base64.StdEncoding.DecodeString(ev.Data.PTY)
			if decErr != nil {
				h.recordErr(fmt.Sprintf("decode pty output: %v", decErr))
				return
			}
			select {
			case h.output <- decoded:
			case <-h.ctx.Done():
				return
			}
		}
		if ev.End != nil {
			h.recordEnd(ev.End)
		}
	}
}

func (h *PtyHandle) recordEnd(end *processEndEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if end.ExitCode != nil {
		h.exitCode = end.ExitCode
	} else if end.ExitCodeSnake != nil {
		h.exitCode = end.ExitCodeSnake
	} else if end.Exited {
		zero := 0
		h.exitCode = &zero
	}
	if end.Error != "" {
		h.errMsg = end.Error
	}
	h.exited = true
}

func (h *PtyHandle) recordErr(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.errMsg == "" {
		h.errMsg = msg
	}
}

// unary issues a short Connect-JSON call (plain application/json, no envelope)
// against the sandbox's envd process service.
func (h *PtyHandle) unary(ctx context.Context, method string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := newEnvdRequest(ctx, h.sandboxID, h.domain, method, bytes.NewReader(raw), false, 0)
	if err != nil {
		return err
	}
	resp, err := terminalUnaryHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("envd %s failed: HTTP %d %s", method, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

// ── envd request plumbing (mirrors RunEnvdCommand addressing) ───────────────

func envdProxyBaseURL() string {
	proxyURL := os.Getenv("AGENTHUB_SANDBOX_PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1"
	}
	return strings.TrimRight(proxyURL, "/")
}

// newEnvdRequest builds a POST to process.Process/<method>. When envelope is
// true the body is wrapped in the 5-byte Connect streaming header (Start);
// otherwise it is sent as plain JSON (unary calls).
func newEnvdRequest(ctx context.Context, sandboxID, domain, method string, body io.Reader, envelope bool, timeout time.Duration) (*http.Request, error) {
	host := fmt.Sprintf("%d-%s.%s", EnvdPort, sandboxID, domain)
	url := fmt.Sprintf("%s/process.Process/%s", envdProxyBaseURL(), method)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	// In Go's net/http the upstream Host must be set via req.Host; the local
	// sandbox proxy routes on it.
	req.Host = host
	req.Header.Set("Authorization", envdAuth)
	if envelope {
		req.Header.Set("Content-Type", connectJSON)
		req.Header.Set("Connect-Protocol-Version", "1")
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
	}
	if timeout > 0 {
		req.Header.Set("Connect-Timeout-Ms", fmt.Sprintf("%d", timeout.Milliseconds()))
	}
	return req, nil
}

func newEnvdStreamRequest(ctx context.Context, sandboxID, domain, method string, payload []byte, timeout time.Duration) (*http.Request, error) {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return newEnvdRequest(ctx, sandboxID, domain, method, bytes.NewReader(frame), true, timeout)
}

// ── Connect stream decoding ─────────────────────────────────────────────────

func readConnectEnvelope(r io.Reader) (flags byte, payload []byte, err error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maxConnectEnvelopeSize {
		return 0, nil, fmt.Errorf("connect stream message too large: %d bytes", size)
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// readPtyEvent reads one Connect frame. eos is true on the end-stream trailer;
// a trailer carrying an error is returned as err with eos true.
func readPtyEvent(r io.Reader) (event *processEvent, eos bool, err error) {
	flags, payload, err := readConnectEnvelope(r)
	if err != nil {
		return nil, false, err
	}
	if flags&connectCompressedFlag != 0 {
		return nil, false, fmt.Errorf("unsupported compressed connect frame")
	}
	if flags&connectEndStreamFlag != 0 {
		return nil, true, parseConnectEndStreamError(payload)
	}
	var resp processStartResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, false, fmt.Errorf("decode pty event: %w", err)
	}
	return resp.Event, false, nil
}

func readPtyStartPID(r io.Reader) (int, error) {
	for {
		ev, eos, err := readPtyEvent(r)
		if err != nil {
			return 0, err
		}
		if eos {
			return 0, fmt.Errorf("terminal stream closed before start event")
		}
		if ev != nil && ev.Start != nil {
			return ev.Start.PID, nil
		}
		// Skip keepalive / non-start events while waiting for the PID.
	}
}

func parseConnectEndStreamError(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var end struct {
		Error *struct {
			Code    string `json:"code,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &end); err != nil {
		return nil // a non-error trailer that we cannot parse is not fatal
	}
	if end.Error == nil {
		return nil
	}
	msg := strings.TrimSpace(end.Error.Message)
	if msg == "" {
		msg = "terminal stream error"
	}
	if end.Error.Code != "" {
		return fmt.Errorf("%s: %s", end.Error.Code, msg)
	}
	return fmt.Errorf("%s", msg)
}

// ── envd process.Process wire types (subset; see sdk/go/pty.go) ──────────────

type ptyStartRequest struct {
	Process ptyProcessConfig `json:"process"`
	PTY     ptyConfig        `json:"pty"`
}

type ptyProcessConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args"`
	Envs map[string]string `json:"envs"`
	Cwd  string            `json:"cwd,omitempty"`
}

type ptyConfig struct {
	Size ptySizeWire `json:"size"`
}

type ptySizeWire struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

type ptyProcessSelector struct {
	PID int `json:"pid"`
}

type ptySignalRequest struct {
	Process ptyProcessSelector `json:"process"`
	Signal  string             `json:"signal"`
}

type ptyInput struct {
	PTY string `json:"pty"`
}

type ptyInputRequest struct {
	Process ptyProcessSelector `json:"process"`
	Input   ptyInput           `json:"input"`
}

type ptyUpdateRequest struct {
	Process ptyProcessSelector `json:"process"`
	PTY     ptyConfig          `json:"pty"`
}

type processStartResponse struct {
	Event *processEvent `json:"event"`
}

type processEvent struct {
	Start     *processStartEvent `json:"start,omitempty"`
	Data      *processDataEvent  `json:"data,omitempty"`
	End       *processEndEvent   `json:"end,omitempty"`
	Keepalive *struct{}          `json:"keepalive,omitempty"`
}

type processStartEvent struct {
	PID int `json:"pid"`
}

type processDataEvent struct {
	PTY string `json:"pty,omitempty"`
}

type processEndEvent struct {
	ExitCode      *int   `json:"exitCode,omitempty"`
	ExitCodeSnake *int   `json:"exit_code,omitempty"`
	Exited        bool   `json:"exited,omitempty"`
	Status        string `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
}
