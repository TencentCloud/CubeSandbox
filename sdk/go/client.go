// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type ClientOption func(*Client)

// WithHTTPClient injects an HTTP client for SDK requests. It is primarily
// useful in tests or when the caller owns transport configuration. If the
// injected client is shared elsewhere, Client.Close also closes that client's
// idle connections.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		if httpClient == nil {
			return
		}
		c.controlHTTP = httpClient
		c.dataHTTP = httpClient
	}
}

type Client struct {
	config      Config
	controlHTTP *http.Client
	dataHTTP    *http.Client
}

func NewClient(config Config, opts ...ClientOption) *Client {
	config = normalizeConfig(config)
	client := &Client{
		config:      config,
		controlHTTP: newControlHTTPClient(config),
		dataHTTP:    newDataHTTPClient(config),
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

// Close releases idle HTTP connections used by this client. It does not pause
// or kill any remote sandboxes.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	if c.controlHTTP != nil {
		c.controlHTTP.CloseIdleConnections()
	}
	if c.dataHTTP != nil && c.dataHTTP != c.controlHTTP {
		c.dataHTTP.CloseIdleConnections()
	}
	return nil
}

func (c *Client) Create(ctx context.Context, opts CreateOptions) (*Sandbox, error) {
	payload, err := c.createPayload(opts)
	if err != nil {
		return nil, err
	}

	var sandbox Sandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes", payload, &sandbox, http.StatusOK, http.StatusCreated); err != nil {
		return nil, err
	}
	c.attachSandbox(&sandbox)
	return &sandbox, nil
}

func (c *Client) Connect(ctx context.Context, sandboxID string) (*Sandbox, error) {
	// Do not fabricate a timeout on connect: with no caller-provided value we
	// omit the field entirely and let the server keep its own timeout policy.
	payload := map[string]any{}
	var sandbox Sandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(sandboxID)+"/connect", payload, &sandbox, http.StatusOK); err != nil {
		return nil, err
	}
	c.attachSandbox(&sandbox)
	return &sandbox, nil
}

func (c *Client) List(ctx context.Context) ([]SandboxInfo, error) {
	var sandboxes []SandboxInfo
	if err := c.doJSON(ctx, http.MethodGet, "/sandboxes", nil, &sandboxes, http.StatusOK); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

func (c *Client) ListV2(ctx context.Context) ([]SandboxInfo, error) {
	var sandboxes []SandboxInfo
	if err := c.doJSON(ctx, http.MethodGet, "/v2/sandboxes", nil, &sandboxes, http.StatusOK); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	var health map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/health", nil, &health, http.StatusOK); err != nil {
		return nil, err
	}
	return health, nil
}

func (c *Client) createPayload(opts CreateOptions) (map[string]any, error) {
	templateID := opts.TemplateID
	if templateID == "" {
		templateID = c.config.TemplateID
	}
	if templateID == "" {
		return nil, fmt.Errorf("template is required. Set CUBE_TEMPLATE_ID or pass TemplateID")
	}

	payload := map[string]any{
		"templateID": templateID,
	}
	// Omitted when nil; see docs/guide/lifecycle.md.
	if opts.Timeout != nil {
		payload["timeout"] = timeoutPayloadSeconds(*opts.Timeout)
	}
	if len(opts.EnvVars) > 0 {
		payload["envVars"] = opts.EnvVars
	}
	if len(opts.Metadata) > 0 {
		payload["metadata"] = opts.Metadata
	}
	internetAccessDisabled := opts.AllowInternetAccess != nil && !*opts.AllowInternetAccess
	if internetAccessDisabled {
		payload["allowInternetAccess"] = false
	}

	// Mirror the server-side contract: domain allowOut requires either
	// allowInternetAccess=false or an explicit deny-all CIDR in denyOut.
	if err := validateAllowOutDomainsRequireDenyAll(opts.Network.AllowOut, opts.Network.DenyOut, internetAccessDisabled); err != nil {
		return nil, err
	}

	network := map[string]any{}
	if opts.Network.AllowPublicTraffic != nil {
		network["allowPublicTraffic"] = *opts.Network.AllowPublicTraffic
	}
	if len(opts.Network.AllowOut) > 0 {
		network["allowOut"] = opts.Network.AllowOut
	}
	if len(opts.Network.DenyOut) > 0 {
		network["denyOut"] = opts.Network.DenyOut
	}
	if len(opts.Network.Rules) > 0 {
		network["rules"] = opts.Network.Rules
	}
	if len(network) > 0 {
		payload["network"] = network
	}

	for key, value := range opts.Extra {
		payload[key] = value
	}

	return payload, nil
}

func (c *Client) attachSandbox(sandbox *Sandbox) {
	sandbox.client = c
	if sandbox.Domain == "" {
		sandbox.Domain = c.config.SandboxDomain
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any, okStatuses ...int) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}

	resp, err := c.controlHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if !statusOK(resp.StatusCode, okStatuses) {
		return apiErrorFromResponse(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.config.APIURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}
	return req, nil
}

func statusOK(statusCode int, okStatuses []int) bool {
	for _, ok := range okStatuses {
		if statusCode == ok {
			return true
		}
	}
	return false
}
