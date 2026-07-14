// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultPtyTimeout mirrors the Python/Node SDKs' 60s default. It is applied two
// ways, matching them: sent to envd as Connect-Timeout-Ms (a server-side
// deadline) and used as a client-side idle abort that resets on every frame
// received. A timeout <= 0 uses this default.
const defaultPtyTimeout = 60 * time.Second

// signalSIGKILL is the Connect-JSON Signal enum name. The wire format uses the
// string name rather than the protobuf integer.
const signalSIGKILL = "SIGNAL_SIGKILL"

// PtySize describes a pseudo-terminal window size.
type PtySize struct {
	Rows int
	Cols int
}

// PtyCreateOptions configures Pty.Create.
type PtyCreateOptions struct {
	// User authenticates the envd process call (Basic auth). Empty defaults to
	// "root" to match the Python/Node SDKs.
	User string
	// Cwd is the working directory for the shell. Empty uses envd's default.
	Cwd string
	// Envs are extra environment variables. TERM/LANG/LC_ALL are seeded with
	// sensible interactive defaults unless overridden here.
	Envs map[string]string
	// Timeout is the server-side deadline for the stream (Connect-Timeout-Ms).
	// Zero uses defaultPtyTimeout.
	Timeout time.Duration
}

// PtyConnectOptions configures Pty.Connect.
type PtyConnectOptions struct {
	// Timeout is the server-side deadline for the stream (Connect-Timeout-Ms).
	// Zero uses defaultPtyTimeout.
	Timeout time.Duration
}

// Pty is the entry point for interacting with pseudo-terminals in a sandbox.
//
// It mirrors the E2B / Python / Node "sandbox.pty" namespace: Create starts a
// new interactive shell, Connect reattaches to an existing one, and
// Kill/SendStdin/Resize control a PTY by PID without holding a PtyHandle.
type Pty struct {
	sandbox *Sandbox
}

// Pty returns the PTY namespace for this sandbox.
func (s *Sandbox) Pty() *Pty {
	return &Pty{sandbox: s}
}

// Create starts a new PTY running an interactive login bash shell ("/bin/bash
// -i -l") sized to size. The returned PtyHandle streams raw terminal output
// until the process exits or the caller disconnects.
func (p *Pty) Create(ctx context.Context, size PtySize, opts PtyCreateOptions) (*PtyHandle, error) {
	if p == nil || p.sandbox == nil {
		return nil, fmt.Errorf("pty is not attached to a sandbox")
	}

	envs := map[string]string{}
	for k, v := range opts.Envs {
		envs[k] = v
	}
	setDefaultEnv(envs, "TERM", "xterm-256color")
	setDefaultEnv(envs, "LANG", "C.UTF-8")
	setDefaultEnv(envs, "LC_ALL", "C.UTF-8")

	user := opts.User
	if user == "" {
		user = defaultEnvdUser
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultPtyTimeout
	}

	payload := ptyStartRequest{
		Process: ptyProcessConfig{
			Cmd:  "/bin/bash",
			Args: []string{"-i", "-l"},
			Envs: envs,
			Cwd:  opts.Cwd,
		},
		PTY: ptyConfig{Size: ptySizeWire{Rows: size.Rows, Cols: size.Cols}},
	}
	return p.openStream(ctx, "Start", payload, user, timeout)
}

// Connect reattaches to an already-running PTY identified by pid, returning a
// fresh PtyHandle that streams its output. The PTY is not affected if a
// previous handle was disconnected.
func (p *Pty) Connect(ctx context.Context, pid int, opts PtyConnectOptions) (*PtyHandle, error) {
	if p == nil || p.sandbox == nil {
		return nil, fmt.Errorf("pty is not attached to a sandbox")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultPtyTimeout
	}
	payload := ptySelectorRequest{Process: ptyProcessSelector{PID: pid}}
	return p.openStream(ctx, "Connect", payload, "", timeout)
}

// Kill sends SIGKILL to the PTY process identified by pid. It reports false
// (not an error) when the PID could not be found — e.g. it already exited.
func (p *Pty) Kill(ctx context.Context, pid int) (bool, error) {
	if p == nil || p.sandbox == nil {
		return false, fmt.Errorf("pty is not attached to a sandbox")
	}
	return p.unary(ctx, "SendSignal", ptySignalRequest{
		Process: ptyProcessSelector{PID: pid},
		Signal:  signalSIGKILL,
	}, true)
}

// SendStdin writes data to the master side of the PTY identified by pid.
func (p *Pty) SendStdin(ctx context.Context, pid int, data []byte) error {
	if p == nil || p.sandbox == nil {
		return fmt.Errorf("pty is not attached to a sandbox")
	}
	_, err := p.unary(ctx, "SendInput", ptyInputRequest{
		Process: ptyProcessSelector{PID: pid},
		Input:   ptyInput{PTY: base64.StdEncoding.EncodeToString(data)},
	}, false)
	return err
}

// Resize changes the window size of the PTY identified by pid.
func (p *Pty) Resize(ctx context.Context, pid int, size PtySize) error {
	if p == nil || p.sandbox == nil {
		return fmt.Errorf("pty is not attached to a sandbox")
	}
	_, err := p.unary(ctx, "Update", ptyUpdateRequest{
		Process: ptyProcessSelector{PID: pid},
		PTY:     ptyConfig{Size: ptySizeWire{Rows: size.Rows, Cols: size.Cols}},
	}, false)
	return err
}

// PtyHandle is a handle to a running PTY. Consume its output either by ranging
// over Output() or by calling Wait with an on-data callback (use one, not
// both). Kill/SendStdin/Resize operate on this handle's PID.
type PtyHandle struct {
	pid     int
	pty     *Pty
	output  chan []byte
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	control *ptyStreamControl
	body    io.ReadCloser
	once    sync.Once

	mu       sync.Mutex
	exitCode *int
	errMsg   string
	exited   bool
	readErr  error
}

// PID returns the PTY process ID.
func (h *PtyHandle) PID() int { return h.pid }

// Output returns the channel of raw PTY output chunks. It is closed when the
// stream ends (process exit, disconnect, or error).
func (h *PtyHandle) Output() <-chan []byte { return h.output }

// ExitCode returns the PTY's exit code once known. The second result is false
// while the process is still running or if envd never reported one.
func (h *PtyHandle) ExitCode() (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exitCode == nil {
		return 0, false
	}
	return *h.exitCode, true
}

// ErrorMessage returns the error envd reported for the PTY (e.g. "signal:
// killed"), or an empty string if none.
func (h *PtyHandle) ErrorMessage() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.errMsg
}

// Kill sends SIGKILL to this PTY. See Pty.Kill.
func (h *PtyHandle) Kill(ctx context.Context) (bool, error) {
	return h.pty.Kill(ctx, h.pid)
}

// SendStdin writes data to this PTY's master side.
func (h *PtyHandle) SendStdin(ctx context.Context, data []byte) error {
	return h.pty.SendStdin(ctx, h.pid, data)
}

// Resize changes this PTY's window size.
func (h *PtyHandle) Resize(ctx context.Context, size PtySize) error {
	return h.pty.Resize(ctx, h.pid, size)
}

// Disconnect stops receiving output without killing the PTY. The process keeps
// running inside the sandbox and can be reattached via Pty.Connect.
func (h *PtyHandle) Disconnect() error {
	h.control.disconnect()
	return h.closeBody()
}

// closeBody closes the response body exactly once, whether the stream ended
// naturally (readLoop) or was torn down early (Disconnect / openStream errors).
// io.ReadCloser does not guarantee a tolerant double-close, so we gate it here.
// The close error is returned; callers that do not care (readLoop) discard it.
func (h *PtyHandle) closeBody() error {
	var err error
	h.once.Do(func() { err = h.body.Close() })
	return err
}

// Wait blocks until the PTY exits and returns its exit code. onData, if
// non-nil, is invoked with each output chunk as it arrives. It returns an error
// if the stream ended without an end event or if envd reported a PTY error
// (e.g. the process was killed).
func (h *PtyHandle) Wait(onData func([]byte)) (int, error) {
	for chunk := range h.output {
		if onData != nil {
			onData(chunk)
		}
	}
	<-h.done

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.readErr != nil {
		return 0, h.readErr
	}
	if !h.exited {
		return 0, fmt.Errorf("PTY stream ended without an end event")
	}
	code := 0
	if h.exitCode != nil {
		code = *h.exitCode
	}
	if h.errMsg != "" {
		return code, fmt.Errorf("PTY exited with error: %s", h.errMsg)
	}
	return code, nil
}

func (h *PtyHandle) readLoop() {
	defer close(h.output)
	defer close(h.done)
	defer func() { _ = h.closeBody() }()
	defer h.control.clearIdle()
	defer h.cancel()

	for {
		ev, eos, err := readPtyEvent(h.body)
		if err != nil {
			// A cancelled context (Disconnect / idle timeout) is an expected
			// stop; setReadErr suppresses or reclassifies it accordingly.
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				h.setReadErr(err)
			}
			return
		}
		h.control.reset()
		if eos {
			return
		}
		if ev == nil {
			continue
		}
		if ev.Data != nil && ev.Data.PTY != "" {
			raw, decErr := base64.StdEncoding.DecodeString(ev.Data.PTY)
			if decErr != nil {
				h.setReadErr(fmt.Errorf("decode pty output: %w", decErr))
				return
			}
			select {
			case h.output <- raw:
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
	if code, ok := end.exitCode(); ok {
		h.exitCode = &code
	} else if code, ok := exitCodeFromStatus(end.Status); ok {
		h.exitCode = &code
	} else if end.Exited {
		zero := 0
		h.exitCode = &zero
	}
	if end.Error != "" {
		h.errMsg = end.Error
	}
	h.exited = true
}

func (h *PtyHandle) setReadErr(err error) {
	// Classify the read error while holding h.mu so the disconnect / idle-timeout
	// checks are atomic with the write: otherwise the idle timer could fire in
	// the window between the check and the lock, and the descriptive timeout
	// message would be lost to the raw I/O error. No lock-ordering inversion —
	// the control methods only take c.mu, never h.mu.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.readErr != nil {
		return
	}
	// A user-initiated Disconnect is a clean stop, not a failure.
	if h.control.isDisconnected() {
		return
	}
	if h.control.idleFired() {
		h.readErr = fmt.Errorf("PTY stream timed out after %s of inactivity", h.control.timeout)
		return
	}
	h.readErr = err
}

// openStream opens a streaming Connect RPC (Start/Connect), reads frames until
// the start event to learn the PID, then hands the still-open body to a
// background read loop.
func (p *Pty) openStream(ctx context.Context, method string, payload any, user string, timeout time.Duration) (*PtyHandle, error) {
	s := p.sandbox
	if err := s.ensureClient(); err != nil {
		return nil, err
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(ctx)
	control := newPtyStreamControl(timeout, cancel)

	req, err := s.newEnvdRequest(streamCtx, http.MethodPost, "/process.Process/"+method, nil, encodeConnectEnvelope(raw))
	if err != nil {
		control.disconnect()
		return nil, err
	}
	req.Header.Set("Content-Type", connectContentType)
	req.Header.Set("Connect-Protocol-Version", connectProtocolVersion)
	req.Header.Set("Connect-Content-Encoding", "identity")
	if user != "" {
		req.Header.Set("Authorization", basicAuthUser(user))
	}
	setConnectTimeout(req, timeout)

	resp, err := s.client.dataHTTP.Do(req)
	if err != nil {
		control.disconnect()
		if control.idleFired() {
			return nil, fmt.Errorf("%s timed out after %s", method, timeout)
		}
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		control.disconnect()
		return nil, apiErrorFromResponse(resp)
	}

	pid, err := readPtyStartPID(resp.Body, method, control)
	if err != nil {
		resp.Body.Close()
		control.disconnect()
		if control.idleFired() {
			return nil, fmt.Errorf("%s timed out after %s", method, timeout)
		}
		return nil, err
	}

	h := &PtyHandle{
		pid:     pid,
		pty:     p,
		output:  make(chan []byte, 64),
		done:    make(chan struct{}),
		ctx:     streamCtx,
		cancel:  cancel,
		control: control,
		body:    resp.Body,
	}
	go h.readLoop()
	return h, nil
}

// unary sends a unary Connect-JSON request (plain application/json, no 5-byte
// envelope) and reports success. When allowNotFound is set, an HTTP 404 or a
// Connect "not_found" body yields (false, nil) instead of an error.
func (p *Pty) unary(ctx context.Context, method string, payload any, allowNotFound bool) (bool, error) {
	s := p.sandbox
	if err := s.ensureClient(); err != nil {
		return false, err
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	req, err := s.newEnvdRequest(ctx, http.MethodPost, "/process.Process/"+method, nil, bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", connectProtocolVersion)

	resp, err := s.client.dataHTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if allowNotFound && isConnectNotFound(resp.StatusCode, body) {
			return false, nil
		}
		return false, apiErrorFromStatus(resp.StatusCode, fmt.Sprintf("%s failed: %s", method, extractErrorMessage(body, resp.StatusCode)))
	}
	return true, nil
}

func readPtyStartPID(r io.Reader, method string, control *ptyStreamControl) (int, error) {
	for {
		ev, eos, err := readPtyEvent(r)
		if err != nil {
			return 0, err
		}
		control.reset()
		if eos {
			return 0, fmt.Errorf("%s: stream closed before start event", method)
		}
		if ev != nil && ev.Start != nil {
			return ev.Start.PID, nil
		}
		// Skip keepalive / non-start events while waiting for the PID.
	}
}

// ptyStreamControl drives the client-side idle timeout and manual disconnect
// for a streaming PTY, mirroring the Node SDK's StreamControl. A single reusable
// timer guards the stream; each received frame stamps a "last activity" time
// instead of touching the timer, so the hot path is allocation-free. When the
// timer fires it re-arms itself for the remaining window if a frame arrived in
// the meantime, otherwise it aborts the request (via cancel) and records
// idleFired so callers can surface a timeout error.
type ptyStreamControl struct {
	timeout time.Duration
	cancel  context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	last  time.Time
	fired bool
	disc  bool
}

func newPtyStreamControl(timeout time.Duration, cancel context.CancelFunc) *ptyStreamControl {
	c := &ptyStreamControl{timeout: timeout, cancel: cancel, last: time.Now()}
	if timeout > 0 {
		c.mu.Lock()
		c.timer = time.AfterFunc(timeout, c.onFire)
		c.mu.Unlock()
	}
	return c
}

// onFire runs when the single idle timer elapses. If a frame arrived within the
// timeout window (recorded by reset), it re-arms for the remaining time rather
// than aborting. This makes the timeout race-free: a just-received frame can
// never be lost to a timer that fired a moment earlier, because the timer owns
// the decision under c.mu and re-checks the activity timestamp before firing.
func (c *ptyStreamControl) onFire() {
	c.mu.Lock()
	if c.disc || c.timer == nil {
		c.mu.Unlock()
		return
	}
	if idle := time.Since(c.last); idle < c.timeout {
		c.timer.Reset(c.timeout - idle)
		c.mu.Unlock()
		return
	}
	c.fired = true
	c.mu.Unlock()
	c.cancel()
}

// reset records that a frame just arrived. It only stamps the activity time;
// the timer is re-armed lazily by onFire, keeping this O(1) and allocation-free.
func (c *ptyStreamControl) reset() {
	if c.timeout <= 0 {
		return
	}
	c.mu.Lock()
	c.last = time.Now()
	c.mu.Unlock()
}

// clearIdle stops the idle timer without aborting the request.
func (c *ptyStreamControl) clearIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

// disconnect marks the stream as intentionally closed and aborts the request.
func (c *ptyStreamControl) disconnect() {
	c.mu.Lock()
	already := c.disc
	c.disc = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	if !already {
		c.cancel()
	}
}

func (c *ptyStreamControl) idleFired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

func (c *ptyStreamControl) isDisconnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disc
}

var (
	reExitStatus   = regexp.MustCompile(`(?:exit status|exited with code)\s+(-?\d+)`)
	reSignalStatus = regexp.MustCompile(`(?:signal|terminated by signal)\s+(\d+)`)
)

// exitCodeFromStatus best-effort parses an exit code from envd's free-form
// end-event "status" string, mirroring the Python/Node SDKs. Signals map to
// 128+signal, matching shell conventions.
func exitCodeFromStatus(status string) (int, bool) {
	if status == "" {
		return 0, false
	}
	if m := reExitStatus.FindStringSubmatch(status); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n, true
		}
	}
	if m := reSignalStatus.FindStringSubmatch(status); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return 128 + n, true
		}
	}
	if status == "exited" {
		return 0, true
	}
	return 0, false
}

// readPtyEvent reads one Connect frame and decodes its ProcessEvent. eos is
// true when an end-stream trailer was seen; a trailer carrying an error is
// returned as err with eos true.
func readPtyEvent(r io.Reader) (event *processEvent, eos bool, err error) {
	flags, payload, err := readConnectEnvelope(r)
	if err != nil {
		return nil, false, err
	}
	if flags&connectCompressedFlag != 0 {
		return nil, false, fmt.Errorf("unsupported compressed Connect stream message")
	}
	if flags&connectEndStreamFlag != 0 {
		if err := parseConnectEndStream(payload); err != nil {
			return nil, true, err
		}
		return nil, true, nil
	}

	var response processStartResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, false, fmt.Errorf("decode pty event: %w", err)
	}
	return response.Event, false, nil
}

func isConnectNotFound(status int, body []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	var parsed struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &parsed) == nil && strings.EqualFold(parsed.Code, "not_found") {
		return true
	}
	return false
}

func setDefaultEnv(m map[string]string, key, value string) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

// --- Connect-JSON request bodies --------------------------------------

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

type ptySelectorRequest struct {
	Process ptyProcessSelector `json:"process"`
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
