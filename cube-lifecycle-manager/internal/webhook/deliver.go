// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// DeliveryConfig tunes the retry behaviour of a single event delivery.
type DeliveryConfig struct {
	// MaxRetries is the number of retries after the initial HTTP request.
	MaxRetries int
	// Timeout bounds each individual HTTP attempt.
	Timeout time.Duration
	// RetryBase is the initial exponential-backoff delay.
	RetryBase time.Duration
	// MaxDuration caps the whole delivery including retries and backoff.
	MaxDuration time.Duration
}

// DefaultDeliveryConfig matches the values the CubeAPI-side Webhook
// implementation shipped with, keeping behaviour consistent for operators.
func DefaultDeliveryConfig() DeliveryConfig {
	return DeliveryConfig{
		MaxRetries:  3,
		Timeout:     5 * time.Second,
		RetryBase:   250 * time.Millisecond,
		MaxDuration: 30 * time.Second,
	}
}

// Deliverer POSTs prepared event bodies to endpoints with bounded retries.
// It is safe for concurrent use by all endpoint workers.
type Deliverer struct {
	cfg   DeliveryConfig
	httpc *http.Client
	log   *zap.Logger
}

// NewDeliverer builds a Deliverer. Redirects are disabled so a configured
// public endpoint cannot bounce delivery to an internal service (SSRF).
func NewDeliverer(cfg DeliveryConfig, log *zap.Logger) *Deliverer {
	return &Deliverer{
		cfg: cfg,
		httpc: &http.Client{
			Timeout: cfg.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log: log,
	}
}

// Deliver posts body to the endpoint until it succeeds, is permanently
// rejected, or the retry budget runs out. A nil return means the receiver
// acknowledged the event with a 2xx. Any non-nil return is terminal for this
// delivery attempt — the caller decides whether the stream entry is ACKed
// (terminal failure) or kept pending (shutdown interrupted the delivery).
func (d *Deliverer) Deliver(ctx context.Context, ep Endpoint, body []byte, event, timestamp string) error {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.MaxDuration)
	defer cancel()

	// The body is immutable across attempts, so the signature is computed
	// once up front.
	var signature string
	if ep.Secret != "" {
		signature = sign(ep.Secret, body)
	}
	label := ep.Label()

	for attempt := 0; ; attempt++ {
		err := d.post(ctx, ep.URL, body, event, timestamp, signature)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// Total deadline hit or the process is shutting down; the
			// caller tells the two apart via its own context.
			return fmt.Errorf("webhook delivery of %s to %s interrupted: %w", event, label, ctx.Err())
		}
		var rejected *rejectedError
		if errors.As(err, &rejected) {
			d.log.Warn("webhook delivery rejected without retry",
				zap.String("url", label),
				zap.String("event", event),
				zap.Int("status", rejected.status))
			return err
		}
		d.log.Warn("webhook delivery failed",
			zap.String("url", label),
			zap.String("event", event),
			zap.Int("attempt", attempt),
			zap.Error(err))
		if attempt >= d.cfg.MaxRetries {
			d.log.Error("webhook delivery exhausted retries",
				zap.String("url", label),
				zap.String("event", event),
				zap.Int("attempts", attempt+1))
			return fmt.Errorf("webhook delivery of %s to %s exhausted %d retries: %w",
				event, label, d.cfg.MaxRetries, err)
		}
		timer := time.NewTimer(retryDelay(d.cfg.RetryBase, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// rejectedError marks a non-retryable HTTP response (4xx other than 408/429,
// or a 3xx left unfollowed because redirects are disabled).
type rejectedError struct {
	status int
	body   string
}

func (e *rejectedError) Error() string {
	return fmt.Sprintf("status=%d body=%q", e.status, e.body)
}

// post performs one HTTP attempt. A nil error means 2xx.
func (d *Deliverer) post(ctx context.Context, url string, body []byte, event, timestamp, signature string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// The URL was validated at config load; this is unreachable in
		// practice, but treat it as permanent rather than retrying forever.
		return &rejectedError{status: 0, body: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cube-Event", event)
	req.Header.Set("X-Cube-Timestamp", timestamp)
	if signature != "" {
		req.Header.Set("X-Cube-Signature-256", signature)
	}

	resp, err := d.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if !shouldRetryStatus(resp.StatusCode) {
		return &rejectedError{status: resp.StatusCode, body: string(respBody)}
	}
	return fmt.Errorf("status=%d body=%q", resp.StatusCode, respBody)
}

// shouldRetryStatus classifies transient receiver failures: server errors,
// request timeout, and rate limiting. Every other status is permanent.
func shouldRetryStatus(status int) bool {
	return status/100 == 5 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}

// retryDelay returns base * 2^attempt plus up to 50% jitter, mirroring the
// CubeAPI-side implementation so retry cadence is identical for operators.
func retryDelay(base time.Duration, attempt int) time.Duration {
	shift := min(attempt, 16)
	exponential := base << shift
	maxJitter := int64(exponential / 2)
	if maxJitter <= 0 {
		return exponential
	}
	return exponential + time.Duration(rand.Int64N(maxJitter+1))
}

// sign returns the X-Cube-Signature-256 value for the exact request body.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
