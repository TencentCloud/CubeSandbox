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
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	envdConnectProtocolVersion = "1"
	envdConnectEndStreamFlag   = byte(0x02)
	envdConnectCompressedFlag  = byte(0x01)
	envdMaxEnvelopeSize        = 64 * 1024 * 1024
)

var (
	envdExitStatusPattern   = regexp.MustCompile(`(?:exit status|exited with code)\s+(-?\d+)`)
	envdSignalStatusPattern = regexp.MustCompile(`(?:signal|terminated by signal)\s+(\d+)`)
)

// EnvdPTYClient is a thin adapter over envd's existing Process Connect API.
// It deliberately reuses the same sandbox proxy route as other CubeOps envd
// operations instead of adding another CubeMaster or Cubelet RPC.
type EnvdPTYClient struct {
	httpClient *http.Client
	proxyBase  string
	host       string
}

type EnvdPTYStartOptions struct {
	Rows uint32
	Cols uint32
}

type EnvdPTYEvent struct {
	Started   bool
	PID       int
	Output    []byte
	Exited    bool
	ExitCode  *int
	Error     string
	StreamEnd bool
}

var defaultEnvdPTYHTTPClient = func() *http.Client {
	// Streaming responses must not use the 60-second timeout of the shared
	// command client. ResponseHeaderTimeout bounds only session startup and
	// leaves the response body free to stream for the terminal lifetime.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	return &http.Client{Transport: transport}
}()

func EnvdProxyURL() string {
	proxyBase := strings.TrimRight(os.Getenv("AGENTHUB_SANDBOX_PROXY_URL"), "/")
	if proxyBase == "" {
		return "http://127.0.0.1"
	}
	return proxyBase
}

func NewEnvdPTYClient(httpClient *http.Client, proxyBase, sandboxID, domain string) *EnvdPTYClient {
	if httpClient == nil {
		httpClient = defaultEnvdPTYHTTPClient
	}
	if proxyBase == "" {
		proxyBase = "http://127.0.0.1"
	}
	return &EnvdPTYClient{
		httpClient: httpClient,
		proxyBase:  proxyBase,
		host:       fmt.Sprintf("%d-%s.%s", EnvdPort, sandboxID, domain),
	}
}

func (c *EnvdPTYClient) Start(ctx context.Context, opts EnvdPTYStartOptions) (io.ReadCloser, error) {
	process := map[string]interface{}{
		"cmd":  "/bin/bash",
		"args": []string{"-i", "-l"},
		"envs": map[string]string{
			"TERM":   "xterm-256color",
			"LANG":   "C.UTF-8",
			"LC_ALL": "C.UTF-8",
		},
	}
	payload := map[string]interface{}{
		"process": process,
		"pty": map[string]interface{}{
			"size": map[string]uint32{"rows": opts.Rows, "cols": opts.Cols},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal envd PTY start request: %w", err)
	}

	req, err := c.newRequest(ctx, "Start", connectEnvelope(raw), connectJSON)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connect-Content-Encoding", "identity")
	req.Header.Set("Authorization", envdAuth)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("envd PTY start failed: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("envd PTY start returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return resp.Body, nil
}

func (c *EnvdPTYClient) SendInput(ctx context.Context, pid int, data []byte) error {
	return c.unary(ctx, "SendInput", map[string]interface{}{
		"process": map[string]int{"pid": pid},
		"input":   map[string]string{"pty": base64.StdEncoding.EncodeToString(data)},
	}, false)
}

func (c *EnvdPTYClient) Resize(ctx context.Context, pid int, rows, cols uint32) error {
	return c.unary(ctx, "Update", map[string]interface{}{
		"process": map[string]int{"pid": pid},
		"pty": map[string]interface{}{
			"size": map[string]uint32{"rows": rows, "cols": cols},
		},
	}, false)
}

func (c *EnvdPTYClient) Kill(ctx context.Context, pid int) error {
	return c.unary(ctx, "SendSignal", map[string]interface{}{
		"process": map[string]int{"pid": pid},
		"signal":  "SIGNAL_SIGKILL",
	}, true)
}

func (c *EnvdPTYClient) unary(ctx context.Context, method string, payload interface{}, allowNotFound bool) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal envd PTY %s request: %w", method, err)
	}
	req, err := c.newRequest(ctx, method, bytes.NewReader(raw), "application/json")
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", envdAuth)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("envd PTY %s failed: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusBadRequest {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if allowNotFound && (resp.StatusCode == http.StatusNotFound || bytes.Contains(detail, []byte(`"not_found"`))) {
		return nil
	}
	return fmt.Errorf("envd PTY %s returned HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(detail)))
}

func (c *EnvdPTYClient) newRequest(
	ctx context.Context,
	method string,
	body io.Reader,
	contentType string,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/process.Process/%s", c.proxyBase, method),
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("create envd PTY %s request: %w", method, err)
	}
	req.Host = c.host
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", envdConnectProtocolVersion)
	return req, nil
}

func ReadEnvdPTYEvent(reader io.Reader) (EnvdPTYEvent, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return EnvdPTYEvent{}, err
	}
	if header[0]&envdConnectCompressedFlag != 0 {
		return EnvdPTYEvent{}, errors.New("compressed envd PTY frames are not supported")
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > envdMaxEnvelopeSize {
		return EnvdPTYEvent{}, fmt.Errorf("envd PTY frame exceeds %d bytes", envdMaxEnvelopeSize)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return EnvdPTYEvent{}, err
	}

	if header[0]&envdConnectEndStreamFlag != 0 {
		if err := parseEnvdEndStream(payload); err != nil {
			return EnvdPTYEvent{}, err
		}
		return EnvdPTYEvent{StreamEnd: true}, nil
	}

	var envelope struct {
		Event map[string]json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return EnvdPTYEvent{}, fmt.Errorf("decode envd PTY event: %w", err)
	}
	event := EnvdPTYEvent{}
	if raw := envelope.Event["start"]; len(raw) > 0 {
		var start map[string]interface{}
		if err := json.Unmarshal(raw, &start); err != nil {
			return EnvdPTYEvent{}, fmt.Errorf("decode envd PTY start event: %w", err)
		}
		pid, ok := envdInt(start["pid"])
		if !ok {
			return EnvdPTYEvent{}, errors.New("envd PTY start event is missing pid")
		}
		event.Started = true
		event.PID = pid
	}
	if raw := envelope.Event["data"]; len(raw) > 0 {
		var data struct {
			PTY string `json:"pty"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return EnvdPTYEvent{}, fmt.Errorf("decode envd PTY data event: %w", err)
		}
		if data.PTY != "" {
			decoded, err := base64.StdEncoding.DecodeString(data.PTY)
			if err != nil {
				return EnvdPTYEvent{}, fmt.Errorf("decode envd PTY output: %w", err)
			}
			event.Output = decoded
		}
	}
	if raw := envelope.Event["end"]; len(raw) > 0 {
		var end map[string]interface{}
		if err := json.Unmarshal(raw, &end); err != nil {
			return EnvdPTYEvent{}, fmt.Errorf("decode envd PTY end event: %w", err)
		}
		event.Exited = true
		if code, ok := envdInt(end["exitCode"]); ok {
			event.ExitCode = &code
		} else if code, ok := envdInt(end["exit_code"]); ok {
			event.ExitCode = &code
		} else if code, ok := envdExitCodeFromStatus(envdString(end["status"])); ok {
			event.ExitCode = &code
		} else if exited, _ := end["exited"].(bool); exited {
			code := 0
			event.ExitCode = &code
		}
		event.Error = envdString(end["error"])
	}
	return event, nil
}

func connectEnvelope(payload []byte) io.Reader {
	body := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)
	return bytes.NewReader(body)
}

func parseEnvdEndStream(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var trailer struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &trailer); err != nil {
		return fmt.Errorf("decode envd PTY end stream: %w", err)
	}
	if len(trailer.Error) > 0 && string(trailer.Error) != "null" {
		return fmt.Errorf("envd PTY stream error: %s", trailer.Error)
	}
	return nil
}

func envdInt(value interface{}) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(typed)
		return parsed, err == nil
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		return parsed, err == nil
	default:
		return 0, false
	}
}

func envdString(value interface{}) string {
	text, _ := value.(string)
	return text
}

func envdExitCodeFromStatus(status string) (int, bool) {
	if match := envdExitStatusPattern.FindStringSubmatch(status); match != nil {
		code, err := strconv.Atoi(match[1])
		return code, err == nil
	}
	if match := envdSignalStatusPattern.FindStringSubmatch(status); match != nil {
		signal, err := strconv.Atoi(match[1])
		return 128 + signal, err == nil
	}
	if status == "exited" {
		return 0, true
	}
	return 0, false
}
