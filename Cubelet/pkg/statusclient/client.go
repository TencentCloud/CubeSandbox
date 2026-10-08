// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package statusclient posts sandbox restart status to CubeMaster.
// A failed post never affects the restart itself. The JSON names match
// CubeMaster's /internal/v1/sandbox-status:batch handler.
package statusclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
)

const reportPath = "/internal/v1/sandbox-status:batch"

// Item is one sandbox's restart status.
type Item struct {
	SandboxID               string     `json:"sandbox_id"`
	HostID                  string     `json:"host_id"`
	Phase                   string     `json:"phase"`
	RestartPolicy           string     `json:"restart_policy"`
	RestartState            string     `json:"restart_state"`
	RestartCount            int32      `json:"restart_count"`
	LastExitCode            *int32     `json:"last_exit_code,omitempty"`
	LastExitReason          string     `json:"last_exit_reason,omitempty"`
	LastRestartAt           *time.Time `json:"last_restart_at,omitempty"`
	LastSuccessfulRestartAt *time.Time `json:"last_successful_restart_at,omitempty"`
	LastFailedRestartAt     *time.Time `json:"last_failed_restart_at,omitempty"`
	NextRestartAt           *time.Time `json:"next_restart_at,omitempty"`
	StatusSeq               int64      `json:"status_seq"`
}

type batchRequest struct {
	HostID string `json:"host_id"`
	Items  []Item `json:"items"`
}

// Client posts batches to CubeMaster. The sandbox network cannot reach this
// address, so the request carries no token.
type Client struct {
	base string
	http *http.Client
}

// NewFromConfig builds a client from the node config.
func NewFromConfig() *Client {
	return &Client{
		base: masterBase(),
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

func masterBase() string {
	addr := ""
	if c := config.GetConfig(); c != nil {
		addr = strings.TrimSpace(c.MetaServerConfig.CubeMasterHTTPAddr)
	}
	if addr == "" {
		addr = "127.0.0.1:8089"
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

// Report posts items. An empty batch is a no-op.
func (c *Client) Report(ctx context.Context, hostID string, items []Item) error {
	if c == nil {
		return errors.New("nil status client")
	}
	if len(items) == 0 {
		return nil
	}
	body, err := json.Marshal(batchRequest{HostID: hostID, Items: items})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+reportPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	rsp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, rsp.Body)
		return nil
	}
	slurp, _ := io.ReadAll(io.LimitReader(rsp.Body, 512))
	return fmt.Errorf("sandbox status report: http %d: %s", rsp.StatusCode, strings.TrimSpace(string(slurp)))
}
