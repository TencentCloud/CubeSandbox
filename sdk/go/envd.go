// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	connectProtocolVersion = "1"
	connectContentType     = "application/connect+json"
	connectEndStreamFlag   = byte(0x02)
	connectCompressedFlag  = byte(0x01)
	maxConnectEnvelopeSize = 64 * 1024 * 1024
	defaultEnvdUser        = "root"
)

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

type connectEndStream struct {
	Error *connectError `json:"error,omitempty"`
}

type connectError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
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

	req, err := s.newEnvdRequest(ctx, http.MethodPost, "/process.Process/Start", nil, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", connectContentType)
	req.Header.Set("Connect-Protocol-Version", connectProtocolVersion)
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
		Host:     s.GetHost(JupyterPort),
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

func readConnectEnvelope(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return 0, nil, err
		}
		return 0, nil, err
	}

	size := binary.BigEndian.Uint32(header[1:])
	if size > maxConnectEnvelopeSize {
		return 0, nil, fmt.Errorf("Connect stream message too large: %d bytes", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func parseConnectEndStream(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}

	var end connectEndStream
	if err := json.Unmarshal(raw, &end); err != nil {
		return fmt.Errorf("decode Connect end stream: %w", err)
	}
	if end.Error == nil {
		return nil
	}
	message := strings.TrimSpace(end.Error.Message)
	if message == "" {
		message = "Connect stream error"
	}
	if end.Error.Code != "" {
		return fmt.Errorf("%s: %s", end.Error.Code, message)
	}
	return fmt.Errorf("%s", message)
}

func decodeProcessBytes(value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
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
	return 0, false
}
