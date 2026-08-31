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
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const (
	// signatureHeader mirrors CubeAPI's webhook signature header exactly, so a
	// receiver built for CubeAPI verifies CLM payloads (and vice versa).
	signatureHeader = "X-Cube-Signature-256"

	// maxResponseBody caps how much of a response we read (for connection
	// reuse) before deciding success/failure. A webhook receiver only needs to
	// return a status code, so a big body is abnormal.
	maxResponseBody = 64 << 10

	defaultHTTPTimeout = 10 * time.Second

	// retryBackoffBase / retryBackoffMax bound the plain for-loop exponential
	// backoff between delivery attempts (200ms doubling to a 1s cap), matching
	// CubeAPI's 200/500/1000ms magnitude.
	retryBackoffBase = 200 * time.Millisecond
	retryBackoffMax  = time.Second
)

// Client delivers events over HTTP with HMAC-SHA256 signing and bounded
// retries. Retry semantics match CubeAPI's webhook emitter: success is any
// HTTP 2xx, everything else (non-2xx or transport error) is retried, and the
// event is dropped after the retry budget is exhausted.
type Client struct {
	hc      *http.Client
	retries int // additional attempts after the first (0 = single attempt)
	log     *zap.Logger
}

// NewClient builds a Client. Only the retry budget is caller-tunable; the
// backoff progression is a fixed plain for-loop (see Send).
func NewClient(timeout time.Duration, retries int, log *zap.Logger) *Client {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{
		hc:      &http.Client{Timeout: timeout},
		retries: retries,
		log:     log,
	}
}

// Send delivers one event to one endpoint with a plain for-loop retry:
// at most retries+1 attempts, sleeping an exponentially growing interval
// (200ms doubling to a 1s cap) between failures. The same body bytes are
// signed and POSTed on every attempt, so the signature is stable across
// retries and receivers can dedupe on event_id.
func (c *Client) Send(ctx context.Context, ep Endpoint, ev Event) error {
	body, err := ev.MarshalBody()
	if err != nil {
		return fmt.Errorf("marshal webhook event: %w", err)
	}

	var lastErr error
	delay := retryBackoffBase
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
			if delay > retryBackoffMax {
				delay = retryBackoffMax
			}
		}
		lastErr = c.post(ctx, ep.URL, ep.Secret, body)
		if lastErr == nil {
			return nil
		}
		c.log.Warn("webhook delivery attempt failed",
			zap.String("url", ep.URL),
			zap.String("event", ev.Event),
			zap.Int("attempt", attempt+1),
			zap.Int("max", c.retries+1),
			zap.Error(lastErr))
	}
	return fmt.Errorf("webhook delivery to %s failed after %d attempt(s): %w",
		ep.URL, c.retries+1, lastErr)
}

func (c *Client) post(ctx context.Context, url, secret string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		// HMAC-SHA256 over the exact raw body bytes, lowercase hex, prefixed
		// with "sha256=" — identical to the CubeAPI receiver contract.
		req.Header.Set(signatureHeader, "sha256="+signBody(secret, body))
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Drain (up to a cap) so the connection can be reused.
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody)); err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status=%d", resp.StatusCode)
	}
	return nil
}

// signBody computes the lowercase hex HMAC-SHA256 of body keyed by secret.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
