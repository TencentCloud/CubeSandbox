// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const webhookUserAgent = "CubeSandbox-Webhook/1.0"

// Sender performs bounded, finite-retry delivery to external endpoints.
type Sender struct {
	delivery    DeliveryConfig
	client      *http.Client
	outstanding chan struct{}
	attempts    chan struct{}
	wg          sync.WaitGroup
	deliveryCtx context.Context
	cancel      context.CancelFunc
	stopped     atomic.Bool
	stats       *Stats
}

// NewSender creates a sender with a shared HTTP client.
func NewSender(delivery DeliveryConfig) *Sender {
	return newSenderWithStats(delivery, &http.Client{
		Timeout: time.Duration(delivery.RequestTimeoutSecs) * time.Second,
	}, NewStats())
}

func newSender(delivery DeliveryConfig, client *http.Client) *Sender {
	return newSenderWithStats(delivery, client, NewStats())
}

func newSenderWithStats(delivery DeliveryConfig, client *http.Client, stats *Stats) *Sender {
	deliveryCtx, cancel := context.WithCancel(context.Background())
	return &Sender{
		delivery:    delivery,
		client:      client,
		outstanding: make(chan struct{}, delivery.MaxOutstandingDeliveries),
		attempts:    make(chan struct{}, delivery.MaxConcurrentRequests),
		deliveryCtx: deliveryCtx,
		cancel:      cancel,
		stats:       stats,
	}
}

// Stats returns the sender statistics registry.
func (s *Sender) Stats() *Stats {
	return s.stats
}

// Submit waits for delivery-task capacity, then owns a copy of the event slice.
func (s *Sender) Submit(ctx context.Context, endpoint *Endpoint, events []Event) bool {
	if len(events) == 0 {
		return true
	}
	if s.stopped.Load() {
		return false
	}
	select {
	case s.outstanding <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	if s.stopped.Load() {
		<-s.outstanding
		return false
	}

	ownedEvents := append([]Event(nil), events...)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.outstanding }()
		s.stats.activeDeliveries.Add(1)
		defer s.stats.activeDeliveries.Add(-1)
		s.deliver(s.deliveryCtx, endpoint, ownedEvents)
	}()
	return true
}

// Stop cancels active HTTP attempts and retry backoff, and rejects new work.
func (s *Sender) Stop() {
	if s.stopped.CompareAndSwap(false, true) {
		s.cancel()
	}
}

func (s *Sender) deliver(ctx context.Context, endpoint *Endpoint, events []Event) {
	started := time.Now()
	batchID := uuid.NewString()
	body, err := json.Marshal(ExternalBatch{BatchID: batchID, Events: events})
	if err != nil {
		s.stats.deliveryFailures.Add(1)
		slog.Error("serialize webhook batch", "endpoint", endpoint.Name, "batch_id", batchID, "event_count", len(events), "error", err)
		return
	}

	for attempt := 1; attempt <= s.delivery.MaxAttempts; attempt++ {
		status, err := s.sendAttempt(ctx, endpoint, body)
		if err == nil && status >= http.StatusOK && status < http.StatusMultipleChoices {
			s.stats.deliverySuccesses.Add(1)
			slog.Debug("webhook delivery succeeded", "endpoint", endpoint.Name, "batch_id", batchID, "event_count", len(events), "latency_ms", time.Since(started).Milliseconds())
			return
		}
		if err == nil && !isRetryableStatus(status) {
			s.stats.deliveryFailures.Add(1)
			slog.Error("webhook delivery failed", "endpoint", endpoint.Name, "batch_id", batchID, "event_count", len(events), "attempts", attempt, "status", status)
			return
		}
		if attempt == s.delivery.MaxAttempts {
			s.stats.deliveryFailures.Add(1)
			slog.Error("webhook delivery failed", "endpoint", endpoint.Name, "batch_id", batchID, "event_count", len(events), "attempts", attempt, "status", status, "error", err)
			return
		}

		s.stats.retryAttempts.Add(1)
		slog.Warn("webhook delivery retrying", "endpoint", endpoint.Name, "batch_id", batchID, "event_count", len(events), "attempt", attempt, "status", status, "error", err)
		delay := backoffDuration(s.delivery, attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.stats.deliveryFailures.Add(1)
			slog.Warn("webhook delivery cancelled", "endpoint", endpoint.Name, "batch_id", batchID, "event_count", len(events), "latency_ms", time.Since(started).Milliseconds())
			return
		case <-timer.C:
		}
	}
}

func (s *Sender) sendAttempt(ctx context.Context, endpoint *Endpoint, body []byte) (int, error) {
	select {
	case s.attempts <- struct{}{}:
		defer func() { <-s.attempts }()
		s.stats.activeHTTPAttempts.Add(1)
		defer s.stats.activeHTTPAttempts.Add(-1)
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL.String(), bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", webhookUserAgent)
	if endpoint.Secret != "" {
		request.Header.Set("X-Cube-Signature-256", signatureHeader(endpoint.Secret, body))
	}

	response, err := s.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}

// Wait waits for all deliveries submitted before this call to finish.
func (s *Sender) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func signatureHeader(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
