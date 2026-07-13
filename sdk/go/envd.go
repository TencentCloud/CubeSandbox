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
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultEnvdUser is the Basic-auth user envd falls back to when none is
// specified, matching the Python SDK.
const defaultEnvdUser = "root"

type processStartRequest struct {
	Process processConfig `json:"process"`
	Stdin   *bool         `json:"stdin,omitempty"`
}

type processConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args"`
	Envs map[string]string `json:"envs"`
	Cwd  string            `json:"cwd,omitempty"`
}

type processStartResult struct {
	PID      int
	Stdout   string
	Stderr   string
	ExitCode int
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
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	PTY    string `json:"pty,omitempty"`
}

type processEndEvent struct {
	ExitCode      *int   `json:"exitCode,omitempty"`
	ExitCodeSnake *int   `json:"exit_code,omitempty"`
	Exited        bool   `json:"exited,omitempty"`
	Status        string `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
}

func (s *Sandbox) startProcess(ctx context.Context, payload processStartRequest, opts CommandOptions) (*processStartResult, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := s.newEnvdRequest(ctx, http.MethodPost, "/process.Process/Start", nil, encodeConnectEnvelope(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", connectContentType)
	req.Header.Set("Connect-Protocol-Version", connectProtocolVersion)
	req.Header.Set("Connect-Content-Encoding", "identity")
	req.Header.Set("Authorization", basicAuthUser(opts.User))
	setConnectTimeout(req, opts.Timeout)

	resp, err := s.client.dataHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, apiErrorFromResponse(resp)
	}

	result, err := parseProcessStartStream(resp.Body)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Sandbox) readFile(ctx context.Context, path string) (string, error) {
	if err := s.ensureClient(); err != nil {
		return "", err
	}

	query := url.Values{"path": []string{path}}
	req, err := s.newEnvdRequest(ctx, http.MethodGet, "/files", query, nil)
	if err != nil {
		return "", err
	}

	resp, err := s.client.dataHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		message := readErrorMessage(resp)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("failed to read %s: %s", path, message)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// writeFile uploads data through envd's POST /files API. It first tries a raw
// octet-stream body and, if the envd version rejects that, retries as a
// multipart upload — mirroring the Python SDK's fallback.
func (s *Sandbox) writeFile(ctx context.Context, path string, data []byte) error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	query := url.Values{"path": []string{path}}

	resp, err := s.doEnvdUpload(ctx, query, bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < http.StatusBadRequest {
		return nil
	}

	multipartBody, contentType, err := multipartFileBody(path, data)
	if err != nil {
		return err
	}
	resp, err = s.doEnvdUpload(ctx, query, multipartBody, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		message := readErrorMessage(resp)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("failed to write %s: %s", path, message)
	}
	return nil
}

func (s *Sandbox) doEnvdUpload(ctx context.Context, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	req, err := s.newEnvdRequest(ctx, http.MethodPost, "/files", query, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return s.client.dataHTTP.Do(req)
}

func multipartFileBody(path string, data []byte) (io.Reader, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", path)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &buf, writer.FormDataContentType(), nil
}

func (s *Sandbox) newEnvdRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	target := url.URL{
		Scheme:   s.client.config.ProxyScheme,
		Host:     s.GetHost(EnvdPort),
		Path:     path,
		RawQuery: query.Encode(),
	}

	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if s.EnvdAccessToken != "" {
		req.Header.Set("X-Access-Token", s.EnvdAccessToken)
	}
	return req, nil
}

func setConnectTimeout(req *http.Request, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	req.Header.Set("Connect-Timeout-Ms", strconv.FormatInt(timeout.Milliseconds(), 10))
}

// basicAuthUser builds the envd "Basic <user>:" auth header. An empty user
// defaults to root to match the Python SDK.
func basicAuthUser(user string) string {
	if user == "" {
		user = defaultEnvdUser
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"))
}

func parseProcessStartStream(r io.Reader) (*processStartResult, error) {
	var result processStartResult
	var stdout strings.Builder
	var stderr strings.Builder
	sawEnd := false

	for {
		flags, payload, err := readConnectEnvelope(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if flags&connectCompressedFlag != 0 {
			return nil, fmt.Errorf("unsupported compressed Connect stream message")
		}
		if flags&connectEndStreamFlag != 0 {
			if err := parseConnectEndStream(payload); err != nil {
				return nil, err
			}
			continue
		}

		var response processStartResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			return nil, fmt.Errorf("decode process event: %w", err)
		}
		if response.Event == nil {
			continue
		}
		if response.Event.Start != nil {
			result.PID = response.Event.Start.PID
		}
		if response.Event.Data != nil {
			if response.Event.Data.Stdout != "" {
				text, err := decodeProcessBytes(response.Event.Data.Stdout)
				if err != nil {
					return nil, fmt.Errorf("decode stdout: %w", err)
				}
				stdout.WriteString(text)
			}
			if response.Event.Data.Stderr != "" {
				text, err := decodeProcessBytes(response.Event.Data.Stderr)
				if err != nil {
					return nil, fmt.Errorf("decode stderr: %w", err)
				}
				stderr.WriteString(text)
			}
		}
		if response.Event.End != nil {
			exitCode, ok := response.Event.End.exitCode()
			if !ok {
				if response.Event.End.Error != "" {
					return nil, fmt.Errorf("process failed: %s", response.Event.End.Error)
				}
				return nil, fmt.Errorf("process EndEvent missing exit code")
			}
			result.ExitCode = exitCode
			sawEnd = true
		}
	}

	if !sawEnd {
		return nil, fmt.Errorf("process stream ended without EndEvent")
	}
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	return &result, nil
}

func decodeProcessBytes(value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Sandbox) filesystemRPC(ctx context.Context, method string, reqBody any) ([]byte, int, error) {
	if err := s.ensureClient(); err != nil {
		return nil, 0, err
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, err
	}
	req, err := s.newEnvdRequest(ctx, http.MethodPost, "/filesystem.Filesystem/"+method, nil, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", connectProtocolVersion)

	resp, err := s.client.dataHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (s *Sandbox) listDir(ctx context.Context, path string) ([]FileEntry, error) {
	body, status, err := s.filesystemRPC(ctx, "ListDir", map[string]string{"path": path})
	if err != nil {
		return nil, err
	}
	if status >= http.StatusBadRequest {
		return nil, fmt.Errorf("failed to list %s: %s", path, extractErrorMessage(body, status))
	}
	var result struct {
		Entries []FileEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}
	if result.Entries == nil {
		result.Entries = []FileEntry{}
	}
	return result.Entries, nil
}

func (s *Sandbox) statFile(ctx context.Context, path string) (*FileEntry, error) {
	body, status, err := s.filesystemRPC(ctx, "Stat", map[string]string{"path": path})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, &NotFoundError{Path: path, Message: fmt.Sprintf("failed to stat %s: %s", path, extractErrorMessage(body, status))}
	}
	if status >= http.StatusBadRequest {
		return nil, fmt.Errorf("failed to stat %s: %s", path, extractErrorMessage(body, status))
	}
	var result struct {
		Entry FileEntry `json:"entry"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode stat response: %w", err)
	}
	return &result.Entry, nil
}

func (s *Sandbox) removeFile(ctx context.Context, path string) error {
	body, status, err := s.filesystemRPC(ctx, "Remove", map[string]string{"path": path})
	if err != nil {
		return err
	}
	if status >= http.StatusBadRequest {
		return fmt.Errorf("failed to remove %s: %s", path, extractErrorMessage(body, status))
	}
	return nil
}

func (s *Sandbox) moveFile(ctx context.Context, source, destination string) (*FileEntry, error) {
	body, status, err := s.filesystemRPC(ctx, "Move", map[string]string{"source": source, "destination": destination})
	if err != nil {
		return nil, err
	}
	if status >= http.StatusBadRequest {
		return nil, fmt.Errorf("failed to move %s to %s: %s", source, destination, extractErrorMessage(body, status))
	}
	var result struct {
		Entry FileEntry `json:"entry"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode move response: %w", err)
	}
	return &result.Entry, nil
}

func (s *Sandbox) makeDirFile(ctx context.Context, path string) (*FileEntry, error) {
	body, status, err := s.filesystemRPC(ctx, "MakeDir", map[string]string{"path": path})
	if err != nil {
		return nil, err
	}
	if status >= http.StatusBadRequest {
		return nil, fmt.Errorf("failed to make dir %s: %s", path, extractErrorMessage(body, status))
	}
	var result struct {
		Entry FileEntry `json:"entry"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode mkdir response: %w", err)
	}
	return &result.Entry, nil
}

func extractErrorMessage(body []byte, status int) string {
	var errResp struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Message != "" {
		return errResp.Message
	}
	return fmt.Sprintf("HTTP %d", status)
}

// Watcher delivers filesystem events from an envd WatchDir stream.
type Watcher struct {
	Events <-chan WatchEvent
	Errors <-chan error

	events chan WatchEvent
	errs   chan error
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	once   sync.Once
}

// Close terminates the watcher and releases resources.
func (w *Watcher) Close() error {
	w.once.Do(func() {
		w.cancel()
		w.body.Close()
	})
	return nil
}

type watchDirFrame struct {
	Start      *struct{}     `json:"start,omitempty"`
	Filesystem *WatchEvent   `json:"filesystem,omitempty"`
	Error      *connectError `json:"error,omitempty"`
	Keepalive  *struct{}     `json:"keepalive,omitempty"`
}

func (s *Sandbox) watchDir(ctx context.Context, path string) (*Watcher, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(ctx)
	req, err := s.newEnvdRequest(streamCtx, http.MethodPost, "/filesystem.Filesystem/WatchDir", nil, encodeConnectEnvelope(payload))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", connectContentType)
	req.Header.Set("Connect-Protocol-Version", connectProtocolVersion)

	resp, err := s.client.dataHTTP.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		cancel()
		return nil, apiErrorFromResponse(resp)
	}

	events := make(chan WatchEvent, 64)
	errs := make(chan error, 1)
	w := &Watcher{
		Events: events,
		Errors: errs,
		events: events,
		errs:   errs,
		ctx:    streamCtx,
		cancel: cancel,
		body:   resp.Body,
	}

	go w.readLoop()
	return w, nil
}

func (w *Watcher) readLoop() {
	defer close(w.events)
	defer close(w.errs)
	defer w.body.Close()

	for {
		flags, payload, err := readConnectEnvelope(w.body)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				w.sendErr(err)
			}
			return
		}
		if flags&connectEndStreamFlag != 0 {
			if err := parseConnectEndStream(payload); err != nil {
				w.sendErr(err)
			}
			return
		}

		var frame watchDirFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			w.sendErr(fmt.Errorf("decode watch event: %w", err))
			return
		}

		if frame.Error != nil {
			msg := frame.Error.Message
			if msg == "" {
				msg = "watch error"
			}
			w.sendErr(fmt.Errorf("%s", msg))
			return
		}

		if frame.Filesystem != nil {
			select {
			case w.events <- *frame.Filesystem:
			case <-w.ctx.Done():
				return
			}
		}
	}
}

func (w *Watcher) sendErr(err error) {
	select {
	case w.errs <- err:
	case <-w.ctx.Done():
	}
}

func (e *processEndEvent) exitCode() (int, bool) {
	if e == nil {
		return 0, false
	}
	if e.ExitCode != nil {
		return *e.ExitCode, true
	}
	if e.ExitCodeSnake != nil {
		return *e.ExitCodeSnake, true
	}
	// envd serializes the end event as proto3 JSON, which omits a zero-valued
	// exitCode field entirely. A successful (exit 0) process therefore arrives
	// with no exitCode key at all — only status="exit status 0" and
	// exited=true. Recover the code from the status string, then fall back to
	// the exited flag so exit-0 commands don't spuriously fail.
	if s := strings.TrimSpace(e.Status); strings.HasPrefix(s, "exit status ") {
		if code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "exit status "))); err == nil {
			return code, true
		}
	}
	if e.Exited {
		return 0, true
	}
	return 0, false
}
