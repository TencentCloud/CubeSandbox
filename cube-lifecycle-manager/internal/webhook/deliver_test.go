// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type capturedRequest struct {
	header http.Header
	body   []byte
}

// receiver collects requests and answers with the queued status codes
// (defaulting to 204 once the queue runs dry).
type receiver struct {
	mu       sync.Mutex
	statuses []int
	requests []capturedRequest
	// block, when non-nil, makes every handler call wait on it — used to
	// force client-side timeouts.
	block <-chan struct{}
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, capturedRequest{header: req.Header.Clone(), body: body})
	status := http.StatusNoContent
	if len(r.statuses) > 0 {
		status = r.statuses[0]
		r.statuses = r.statuses[1:]
	}
	block := r.block
	r.mu.Unlock()
	if block != nil {
		<-block
	}
	if status/100 == 3 {
		w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
	}
	w.WriteHeader(status)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *receiver) got(i int) capturedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[i]
}

func testConfig() DeliveryConfig {
	return DeliveryConfig{
		MaxRetries:  3,
		Timeout:     5 * time.Second,
		RetryBase:   0, // keep tests fast; backoff math is covered separately
		MaxDuration: 30 * time.Second,
	}
}

func newTestDeliverer(cfg DeliveryConfig) *Deliverer {
	return NewDeliverer(cfg, zap.NewNop())
}

func TestDeliver_SuccessWithExactHeadersAndSignature(t *testing.T) {
	r := &receiver{}
	srv := httptest.NewServer(r)
	defer srv.Close()

	ep := Endpoint{URL: srv.URL, Secret: "0123456789abcdef"}
	body := []byte(`{"version":"1","event":"sandbox.created"}`)
	d := newTestDeliverer(testConfig())
	if err := d.Deliver(context.Background(), ep, body, EventCreated, "2026-07-29T08:00:00Z"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("requests = %d, want 1", r.count())
	}
	req := r.got(0)
	if got := req.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got := req.header.Get("X-Cube-Event"); got != EventCreated {
		t.Fatalf("x-cube-event = %q", got)
	}
	if got := req.header.Get("X-Cube-Timestamp"); got != "2026-07-29T08:00:00Z" {
		t.Fatalf("x-cube-timestamp = %q", got)
	}
	// The signature must cover the exact bytes received, not a re-serialization.
	if got, want := req.header.Get("X-Cube-Signature-256"), sign(ep.Secret, req.body); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestDeliver_OmitsSignatureWithoutSecret(t *testing.T) {
	r := &receiver{}
	srv := httptest.NewServer(r)
	defer srv.Close()

	d := newTestDeliverer(testConfig())
	if err := d.Deliver(context.Background(), Endpoint{URL: srv.URL}, []byte(`{}`), EventDeleted, "ts"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := r.got(0).header.Get("X-Cube-Signature-256"); got != "" {
		t.Fatalf("unexpected signature header %q", got)
	}
}

func TestDeliver_RetriesTransientStatusesUntilSuccess(t *testing.T) {
	r := &receiver{statuses: []int{
		http.StatusInternalServerError,
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusNoContent,
	}}
	srv := httptest.NewServer(r)
	defer srv.Close()

	d := newTestDeliverer(testConfig())
	if err := d.Deliver(context.Background(), Endpoint{URL: srv.URL}, []byte(`{}`), EventCreated, "ts"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if r.count() != 4 {
		t.Fatalf("requests = %d, want 4", r.count())
	}
}

func TestDeliver_DoesNotRetryPermanentClientError(t *testing.T) {
	r := &receiver{statuses: []int{http.StatusBadRequest}}
	srv := httptest.NewServer(r)
	defer srv.Close()

	d := newTestDeliverer(testConfig())
	err := d.Deliver(context.Background(), Endpoint{URL: srv.URL}, []byte(`{}`), EventCreated, "ts")
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if r.count() != 1 {
		t.Fatalf("requests = %d, want 1 (no retry on 4xx)", r.count())
	}
}

func TestDeliver_StopsAfterConfiguredRetryLimit(t *testing.T) {
	r := &receiver{statuses: []int{500, 500, 500, 500}}
	srv := httptest.NewServer(r)
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRetries = 2
	d := newTestDeliverer(cfg)
	if err := d.Deliver(context.Background(), Endpoint{URL: srv.URL}, []byte(`{}`), EventCreated, "ts"); err == nil {
		t.Fatal("expected exhausted-retries error")
	}
	if r.count() != 3 {
		t.Fatalf("requests = %d, want 3 (1 + 2 retries)", r.count())
	}
}

func TestDeliver_RetriesTimedOutRequests(t *testing.T) {
	gate := make(chan struct{})
	r := &receiver{block: gate}
	srv := httptest.NewServer(r)
	defer srv.Close()
	defer close(gate)

	cfg := testConfig()
	cfg.MaxRetries = 1
	cfg.Timeout = 100 * time.Millisecond
	d := newTestDeliverer(cfg)
	if err := d.Deliver(context.Background(), Endpoint{URL: srv.URL}, []byte(`{}`), EventCreated, "ts"); err == nil {
		t.Fatal("expected timeout error")
	}
	if r.count() != 2 {
		t.Fatalf("requests = %d, want 2", r.count())
	}
}

func TestDeliver_CapsTotalDeliveryDuration(t *testing.T) {
	gate := make(chan struct{})
	r := &receiver{block: gate}
	srv := httptest.NewServer(r)
	defer srv.Close()
	defer close(gate)

	cfg := testConfig()
	cfg.MaxRetries = 1000                   // effectively unbounded retries...
	cfg.MaxDuration = 50 * time.Millisecond // ...but the total cap wins
	d := newTestDeliverer(cfg)
	start := time.Now()
	if err := d.Deliver(context.Background(), Endpoint{URL: srv.URL}, []byte(`{}`), EventCreated, "ts"); err == nil {
		t.Fatal("expected deadline error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("delivery ran %v, want it capped near 50ms", elapsed)
	}
	if r.count() != 1 {
		t.Fatalf("requests = %d, want 1", r.count())
	}
}

func TestDeliver_DoesNotFollowRedirects(t *testing.T) {
	r := &receiver{statuses: []int{http.StatusFound}}
	srv := httptest.NewServer(r)
	defer srv.Close()

	d := newTestDeliverer(testConfig())
	err := d.Deliver(context.Background(), Endpoint{URL: srv.URL}, []byte(`{}`), EventCreated, "ts")
	if err == nil {
		t.Fatal("expected terminal rejection on 302")
	}
	if r.count() != 1 {
		t.Fatalf("requests = %d, want 1 (redirect neither followed nor retried)", r.count())
	}
}

func TestShouldRetryStatus(t *testing.T) {
	cases := map[int]bool{
		500: true, 502: true, 503: true,
		http.StatusRequestTimeout:  true,
		http.StatusTooManyRequests: true,
		200:                        false, 302: false, 400: false, 401: false, 404: false,
	}
	for status, want := range cases {
		if got := shouldRetryStatus(status); got != want {
			t.Fatalf("shouldRetryStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	if got := retryDelay(0, 4); got != 0 {
		t.Fatalf("zero base must stay zero, got %v", got)
	}
	base := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		got := retryDelay(base, 2)
		if got < 400*time.Millisecond || got > 600*time.Millisecond {
			t.Fatalf("delay %v outside [400ms, 600ms]", got)
		}
	}
}
