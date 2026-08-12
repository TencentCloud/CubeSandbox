// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package reporter asynchronously reports EvictionEvents to CubeMaster via
// POST /event/eviction. Failures are retried with exponential back-off up to
// maxAttempts times. A failed report after all retries is logged and dropped —
// the local NDJSON store always has a copy, and the eviction denial itself is
// unaffected.
package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/auth"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

const (
	reportPath  = "/event/eviction"
	httpTimeout = 5 * time.Second
)

const (
	defaultMaxAttempts = 3
	defaultBaseDelay   = time.Second
)

// Option customizes a Reporter.
type Option func(*Reporter)

// WithRetry customizes retry behavior. Non-positive values keep the defaults.
func WithRetry(maxAttempts int, baseDelay time.Duration) Option {
	return func(r *Reporter) {
		if maxAttempts > 0 {
			r.maxAttempts = maxAttempts
		}
		if baseDelay > 0 {
			r.baseDelay = baseDelay
		}
	}
}

// Reporter sends EvictionEvents to CubeMaster.
type Reporter struct {
	cubeMasterURL string
	userID        string
	secretKey     string
	httpClient    *http.Client
	authEnabled   bool
	maxAttempts   int
	baseDelay     time.Duration
}

// New creates a Reporter. When authEnabled is false the HMAC headers are
// omitted (useful for test environments where CubeMaster has auth disabled).
func New(cubeMasterURL, userID, secretKey string, authEnabled bool, opts ...Option) *Reporter {
	r := &Reporter{
		cubeMasterURL: cubeMasterURL,
		userID:        userID,
		secretKey:     secretKey,
		httpClient:    &http.Client{Timeout: httpTimeout},
		authEnabled:   authEnabled,
		maxAttempts:   defaultMaxAttempts,
		baseDelay:     defaultBaseDelay,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Report asynchronously delivers the event to CubeMaster with configured
// retries. It returns a channel that closes when the report completes (success or
// all retries exhausted). Production code may ignore the channel; tests use it to
// wait for the async goroutine to finish.
func (r *Reporter) Report(event *types.EvictionEvent) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.reportWithRetry(context.Background(), event, r.maxAttempts, r.baseDelay)
	}()
	return done
}

func (r *Reporter) reportWithRetry(ctx context.Context, event *types.EvictionEvent, maxAttempts int, baseDelay time.Duration) {
	body, err := json.Marshal(event)
	if err != nil {
		log.Printf("[eviction-reporter] marshal error eventID=%s: %v", event.EventID, err)
		return
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := r.send(ctx, body); err != nil {
			lastErr = err
			delay := baseDelay * (1 << (attempt - 1))
			log.Printf("[eviction-reporter] attempt %d/%d failed eventID=%s err=%v retrying in %s",
				attempt, maxAttempts, event.EventID, err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		log.Printf("[eviction-reporter] reported eventID=%s attempt=%d", event.EventID, attempt)
		return
	}
	log.Printf("[eviction-reporter] all retries exhausted eventID=%s lastErr=%v", event.EventID, lastErr)
}

func (r *Reporter) send(ctx context.Context, body []byte) error {
	url := r.cubeMasterURL + reportPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	if r.authEnabled {
		if err := auth.Attach(req, r.userID, r.secretKey); err != nil {
			return fmt.Errorf("build auth headers: %w", err)
		}
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
