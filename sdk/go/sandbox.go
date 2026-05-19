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

const JupyterPort = 49999

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
func (s *Sandbox) Resume(ctx context.Context, timeout time.Duration) error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = s.client.config.Timeout
	}

	path := "/sandboxes/" + url.PathEscape(s.SandboxID) + "/resume"
	payload := map[string]any{"timeout": durationSeconds(timeout)}
	return s.client.doJSON(ctx, http.MethodPost, path, payload, nil, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

func (s *Sandbox) Kill(ctx context.Context) error {
	if err := s.ensureClient(); err != nil {
		return err
	}

	path := "/sandboxes/" + url.PathEscape(s.SandboxID)
	return s.client.doJSON(ctx, http.MethodDelete, path, nil, nil, http.StatusOK, http.StatusNoContent)
}

func (s *Sandbox) Close() error {
	if s.client == nil {
		return nil
	}
	if s.client.dataHTTP != nil {
		s.client.dataHTTP.CloseIdleConnections()
	}
	if s.client.controlHTTP != nil {
		s.client.controlHTTP.CloseIdleConnections()
	}
	return nil
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
	return &Files{reader: s}
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
