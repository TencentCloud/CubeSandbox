// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

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

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
)

const (
	// reportMaxAttempts bounds the retry loop. With reportBaseBackoff=500ms and
	// doubling, 6 attempts span ~15s of wall time — long enough to ride out a
	// CubeMaster rolling restart without holding the build goroutine forever.
	reportMaxAttempts = 6
	// terminalReportMaxAttempts is used for terminal reports (BUILT / FAILED).
	// Losing a terminal report leaves the job stuck in RUNNING until the
	// reconciler times it out, so it is worth retrying much harder.
	terminalReportMaxAttempts = 12
)

// Backoff schedule. These are variables rather than constants purely so tests
// can shrink them; nothing in production reassigns them.
var (
	reportBaseBackoff = 500 * time.Millisecond
	reportMaxBackoff  = 5 * time.Second
)

// Reporter reports build status back to CubeMaster.
// CubeMaster receives the callback and writes DB.
type Reporter struct {
	masterURL  string
	httpClient *http.Client
}

// NewReporter creates a new status reporter. The CubeMaster base URL comes from
// CUBE_MASTER_ADDR (default http://localhost:8089, which is correct for the
// single-host one-click layout). The retired CUBE_TEMPLATE_CENTER_MASTER_ENDPOINT
// and CUBE_MASTER_ENDPOINT spellings still work; see pkg/tcconfig.
func NewReporter() *Reporter {
	return &Reporter{
		masterURL: tcconfig.MasterEndpoint(),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Report sends a status update to CubeMaster, retrying with exponential
// backoff on transport errors and 5xx responses.
//
// CubeMaster's POST /internal/template/jobs/:job_id/status handler receives
// this. Terminal reports (BUILT / FAILED) get a larger retry budget because
// dropping one leaves the job stuck in RUNNING until the reconciler
// (pkg/reconcile) times it out.
func (r *Reporter) Report(ctx context.Context, jobID string, fields map[string]any) error {
	attempts := reportMaxAttempts
	if isTerminalReport(fields) {
		attempts = terminalReportMaxAttempts
	}

	body, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	url := fmt.Sprintf("%s/internal/template/jobs/%s/status", r.masterURL, jobID)

	log.G(ctx).Infof("report status to master: job_id=%s url=%s fields=%+v", jobID, url, fields)

	backoff := reportBaseBackoff
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		retryable, err := r.postOnce(ctx, url, body)
		if err == nil {
			if attempt > 1 {
				log.G(ctx).Infof("report status succeeded on attempt %d: job_id=%s", attempt, jobID)
			}
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
		if attempt == attempts {
			break
		}
		log.G(ctx).Warnf("report status attempt %d/%d failed (retrying in %s): job_id=%s err=%v",
			attempt, attempts, backoff, jobID, err)

		// Use a timer rather than time.Sleep so a canceled context aborts
		// the wait immediately.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("report status aborted: %w (last error: %v)", ctx.Err(), lastErr)
		case <-timer.C:
		}
		if backoff = backoff * 2; backoff > reportMaxBackoff {
			backoff = reportMaxBackoff
		}
	}
	return fmt.Errorf("report status gave up after %d attempts: %w", attempts, lastErr)
}

// postOnce performs one POST. retryable is true for transport errors, 429 and
// 5xx; it is false for 4xx (other than 429), which indicate a payload/routing
// bug that retrying cannot fix.
func (r *Reporter) postOnce(ctx context.Context, url string, body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		// Never retry once the caller's context is done.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, fmt.Errorf("http post: %w", err)
		}
		return true, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	err = fmt.Errorf("master returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return true, err
	}
	return false, err
}

// isTerminalReport reports whether fields carry a terminal job status.
func isTerminalReport(fields map[string]any) bool {
	status, _ := fields["status"].(string)
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "BUILT", "FAILED", "READY":
		return true
	}
	return false
}

// Close cleans up the reporter.
func (r *Reporter) Close() {
	r.httpClient.CloseIdleConnections()
}
