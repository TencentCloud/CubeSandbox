// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// JupyterPort hosts the code-interpreter (`/execute`), while EnvdPort hosts the
// envd data-plane RPCs (commands, files, filesystem, pty). They mirror the
// Python/Node SDKs (JUPYTER_PORT=49999, ENVD_PORT=49983); routing an envd RPC
// to JupyterPort returns 404.
const (
	JupyterPort = 49999
	EnvdPort    = 49983
)

func (s *Sandbox) GetHost(port int) string {
	domain := s.Domain
	if domain == "" && s.client != nil {
		domain = s.client.config.SandboxDomain
	}
	return fmt.Sprintf("%d-%s.%s", port, s.SandboxID, domain)
}

func (s *Sandbox) GetInfo(ctx context.Context) (*SandboxInfo, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}

	var info SandboxInfo
	path := "/sandboxes/" + url.PathEscape(s.SandboxID)
	if err := s.client.doJSON(ctx, http.MethodGet, path, nil, &info, http.StatusOK); err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *Sandbox) Pause(ctx context.Context, opts PauseOptions) error {
	if err := s.ensureClient(); err != nil {
		return err
	}

	path := "/sandboxes/" + url.PathEscape(s.SandboxID) + "/pause"
	if err := s.client.doJSON(ctx, http.MethodPost, path, nil, nil, http.StatusOK, http.StatusNoContent); err != nil {
		return err
	}
	if !pauseShouldWait(opts) {
		return nil
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	interval := opts.Interval
	if interval < 0 {
		interval = 0
	}
	if interval == 0 {
		interval = time.Second
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		info, err := s.GetInfo(ctx)
		if err != nil {
			return err
		}
		if info.State == "paused" {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("sandbox %q did not reach 'paused' state within %s", s.SandboxID, timeout)
		case <-ticker.C:
		}
	}
}

// Resume resumes a paused sandbox.
//
// Deprecated: use Client.Connect instead, which auto-resumes paused sandboxes
// and returns a fresh Sandbox instance.
// The timeout is optional; nil omits it. See docs/guide/lifecycle.md.
func (s *Sandbox) Resume(ctx context.Context, timeout *time.Duration) error {
	if err := s.ensureClient(); err != nil {
		return err
	}

	path := "/sandboxes/" + url.PathEscape(s.SandboxID) + "/resume"
	payload := map[string]any{}
	if timeout != nil {
		payload["timeout"] = timeoutPayloadSeconds(*timeout)
	}
	return s.client.doJSON(ctx, http.MethodPost, path, payload, nil, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

// SetTimeout updates the sandbox idle timeout (POST /sandboxes/:id/timeout).
//
// Positive values set a new TTL in seconds. Zero requests immediate expiry.
// NeverTimeout (-1) disables idle timeout entirely. Sub-second durations are
// rounded up because the wire protocol uses integer seconds. Values other
// than NeverTimeout that are < 0 are rejected with a descriptive error.
//
// Errors wrap ErrSandboxNotFound (404) or an *APIError for other HTTP errors.
//
// See docs/guide/lifecycle.md for timeout semantics.
func (s *Sandbox) SetTimeout(ctx context.Context, timeout time.Duration) error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	if timeout < 0 && timeout != NeverTimeout {
		return fmt.Errorf("cubesandbox: timeout must be >= 0 or NeverTimeout (-1), got %v", timeout)
	}

	seconds := timeoutPayloadSeconds(timeout)
	path := "/sandboxes/" + url.PathEscape(s.SandboxID) + "/timeout"
	payload := map[string]any{"timeout": seconds}
	return s.client.doJSON(ctx, http.MethodPost, path, payload, nil, http.StatusNoContent)
}

func (s *Sandbox) Kill(ctx context.Context) error {
	if err := s.ensureClient(); err != nil {
		return err
	}

	path := "/sandboxes/" + url.PathEscape(s.SandboxID)
	return s.client.doJSON(ctx, http.MethodDelete, path, nil, nil, http.StatusOK, http.StatusNoContent)
}

// Close releases idle HTTP connections used by this sandbox's client. It does
// not pause or kill the remote sandbox.
//
// Deprecated: use Client.Close for SDK client cleanup. Use Sandbox.Kill or
// Sandbox.Pause for remote sandbox lifecycle.
func (s *Sandbox) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *Sandbox) RunCode(ctx context.Context, code string, opts RunCodeOptions) (*Execution, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	payload := map[string]any{
		"code":     code,
		"language": nil,
		"env_vars": nil,
	}
	if opts.Language != "" {
		payload["language"] = opts.Language
	}
	if opts.Envs != nil {
		payload["env_vars"] = opts.Envs
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.client.config.ProxyScheme+"://"+s.GetHost(JupyterPort)+"/execute", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.dataHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, apiErrorFromStatus(resp.StatusCode, fmt.Sprintf("execute failed: HTTP %d", resp.StatusCode))
	}

	execution := &Execution{}
	if err := parseStream(resp.Body, execution, opts); err != nil {
		return nil, err
	}
	return execution, nil
}

func (s *Sandbox) Commands() *Commands {
	return &Commands{starter: s}
}

func (s *Sandbox) Files() *Files {
	return &Files{reader: s, writer: s, filer: s}
}

func (s *Sandbox) ensureClient() error {
	if s == nil || s.client == nil {
		return fmt.Errorf("sandbox is not attached to a client")
	}
	return nil
}

func pauseShouldWait(opts PauseOptions) bool {
	return opts.Wait == nil || *opts.Wait
}
