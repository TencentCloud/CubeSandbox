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
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

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

	log.G(ctx).Infof("submit build job to TC: job_id=%s url=%s", jobID, url)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("TC returned %d: %s", resp.StatusCode, string(respBody))
	}

	log.G(ctx).Infof("build job submitted to TC successfully: job_id=%s", jobID)
	return nil
}
