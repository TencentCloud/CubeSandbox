// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package main provides a minimal Cube Sandbox client for creating and
// killing sandboxes via the Cube API Server (E2B-compatible REST API).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is a minimal Cube API client that supports Create and Kill.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a Client from environment variables:
//
//	E2B_API_URL   — base URL of the Cube API Server (e.g. http://localhost:3000)
//	E2B_API_KEY   — API key (any non-empty string when auth is disabled)
//	SSL_CERT_FILE — optional path to a CA certificate (for HTTPS with mkcert)
func NewClient() (*Client, error) {
	baseURL := strings.TrimRight(os.Getenv("E2B_API_URL"), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("E2B_API_URL is not set")
	}
	apiKey := os.Getenv("E2B_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("E2B_API_KEY is not set")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()

	if certFile := os.Getenv("SSL_CERT_FILE"); certFile != "" {
		pem, err := os.ReadFile(certFile)
		if err != nil {
			return nil, fmt.Errorf("read SSL_CERT_FILE %q: %w", certFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no valid PEM certificates found in %q", certFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	}

	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}, nil
}

// ─── Request / response types ─────────────────────────────────────────────

type createRequest struct {
	TemplateID string `json:"templateID"`
}

// Sandbox holds the fields returned by POST /sandboxes.
type Sandbox struct {
	SandboxID  string `json:"sandboxID"`
	TemplateID string `json:"templateID"`
	ClientID   string `json:"clientID"`
}

// ─── API methods ──────────────────────────────────────────────────────────

// Create creates a new sandbox from the given template and returns it.
func (c *Client) Create(ctx context.Context, templateID string) (*Sandbox, error) {
	body, err := json.Marshal(createRequest{TemplateID: templateID})
	if err != nil {
		return nil, fmt.Errorf("marshal create request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create sandbox: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var sb Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
		return nil, fmt.Errorf("decode create response: %w", err)
	}
	return &sb, nil
}

// Kill deletes the sandbox with the given sandboxID.
func (c *Client) Kill(ctx context.Context, sandboxID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.baseURL+"/sandboxes/"+sandboxID, nil)
	if err != nil {
		return fmt.Errorf("build kill request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kill sandbox %s: %w", sandboxID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kill sandbox %s: HTTP %d: %s", sandboxID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
