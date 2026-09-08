// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package tcclient provides the HTTP client for CubeMaster to submit build
// jobs to CubeTemplateCenter.
package tcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// StatusError wraps a non-2xx HTTP response from TC so callers can make
// typed decisions (retry on 429/503, treat 409 as "already submitted",
// etc.) instead of parsing error strings.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("TC returned %d: %s", e.StatusCode, e.Body)
}

// Client is the TC HTTP client.
type Client struct {
	endpoint   string
	httpClient *http.Client
}

// NewClient creates a new TC client.
func NewClient(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// setSharedTokenHeader authenticates the call with the same shared secret TC
// attaches to its status callbacks (constants.TemplateCallbackTokenEnv /
// constants.TemplateCallbackTokenHeader). TC rejects requests without it once
// the variable is set there; when it is unset here the header is simply
// omitted, matching a TC that also has none (rolling-upgrade window).
//
// Read per request rather than cached at construction so tests and config
// reloads observe the current environment.
func setSharedTokenHeader(req *http.Request) {
	if token := strings.TrimSpace(os.Getenv(constants.TemplateCallbackTokenEnv)); token != "" {
		req.Header.Set(constants.TemplateCallbackTokenHeader, token)
	}
}

// SubmitBuildJob submits a build job to TC.
// TC will pull the image, build ext4, upload to cbs, and report status back
// to CubeMaster via POST /internal/template/jobs/:job_id/status.
func (c *Client) SubmitBuildJob(ctx context.Context, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL string, envdSHA string, envdData []byte) error {
	payload := map[string]any{
		"job_id":            jobID,
		"request":           req,
		"download_base_url": downloadBaseURL,
		"envd_sha256":       envdSHA,
		"envd_data":         envdData,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal build job: %w", err)
	}

	url := fmt.Sprintf("%s/tc/api/v1/build", c.endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setSharedTokenHeader(httpReq)

	log.G(ctx).Infof("submit build job to TC: job_id=%s url=%s", jobID, url)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return &StatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	log.G(ctx).Infof("build job submitted to TC successfully: job_id=%s", jobID)
	return nil
}

// DeleteArtifact asks CubeTemplateCenter to remove an artifact's data: the
// S3/MinIO object (if uploaded), the local/shared ext4 file, and finally the
// artifact row itself. CubeMaster never touches the filesystem or S3 for an
// artifact directly -- only CubeTemplateCenter, which wrote the data, is
// allowed to remove it (see CubeTemplateCenter/pkg/build/deleter.go).
//
// Idempotent: deleting an artifact TC has already removed (or never learned
// about) returns nil. Callers should NOT hard-delete the artifact row
// themselves when this call fails -- leave it CLEANUP_PENDING so TC's own
// reconciler can sweep it later as a backstop.
func (c *Client) DeleteArtifact(ctx context.Context, artifactID string) error {
	payload := map[string]any{
		"artifact_id": artifactID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal artifact delete request: %w", err)
	}

	url := fmt.Sprintf("%s/tc/api/v1/artifact/delete", c.endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setSharedTokenHeader(httpReq)

	log.G(ctx).Infof("request TC to delete artifact: artifact_id=%s url=%s", artifactID, url)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return &StatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	log.G(ctx).Infof("artifact delete requested from TC successfully: artifact_id=%s", artifactID)
	return nil
}
