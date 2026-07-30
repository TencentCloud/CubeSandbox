// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type blockingTransport struct {
	started   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

func (transport *blockingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	close(transport.started)
	select {
	case <-transport.release:
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	case <-request.Context().Done():
		close(transport.cancelled)
		return nil, request.Context().Err()
	}
}

func endpointForServer(t *testing.T, server *httptest.Server) *Endpoint {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Endpoint{ID: 0, Name: "test", URL: parsed, BatchSize: 1, Secret: "test-secret"}
}

func fastDeliveryConfig() DeliveryConfig {
	cfg := defaultDeliveryConfig()
	cfg.MaxOutstandingDeliveries = 10
	cfg.MaxConcurrentRequests = 10
	cfg.RequestTimeoutSecs = 1
	cfg.MaxAttempts = 2
	cfg.InitialBackoffMS = 1
	cfg.MaxBackoffSecs = 1
	return cfg
}

func TestSender_SignsExactRequestBody(t *testing.T) {
	var body []byte
	var signature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		signature = r.Header.Get("X-Cube-Signature-256")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := newSender(fastDeliveryConfig(), server.Client())
	if !sender.Submit(context.Background(), endpointForServer(t, server), validBatch().Events) {
		t.Fatal("Submit returned false")
	}
	if err := sender.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if signature != want {
		t.Fatalf("signature = %q, want %q", signature, want)
	}
	var payload ExternalBatch
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(payload.BatchID); err != nil {
		t.Fatalf("batch_id = %q: %v", payload.BatchID, err)
	}
}

func TestSender_RetryReusesExternalBatchIDAndBody(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := newSender(fastDeliveryConfig(), server.Client())
	sender.Submit(context.Background(), endpointForServer(t, server), validBatch().Events)
	if err := sender.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("retry bodies = %q, want two identical bodies", bodies)
	}
	stats := sender.Stats().Snapshot()
	if stats.RetryAttempts != 1 || stats.DeliverySuccesses != 1 {
		t.Fatalf("sender stats = %#v", stats)
	}
}

func TestSender_DoesNotRetryPermanentStatus(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	sender := newSender(fastDeliveryConfig(), server.Client())
	sender.Submit(context.Background(), endpointForServer(t, server), validBatch().Events)
	if err := sender.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if got := sender.Stats().Snapshot().DeliveryFailures; got != 1 {
		t.Fatalf("delivery failures = %d, want 1", got)
	}
}

func TestSender_SubmittedDeliverySurvivesCallerCancellation(t *testing.T) {
	transport := &blockingTransport{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
	sender := newSender(fastDeliveryConfig(), &http.Client{Transport: transport})
	endpointURL, err := url.Parse("http://example.test/webhook")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &Endpoint{ID: 0, Name: "test", URL: endpointURL, BatchSize: 1}
	ctx, cancel := context.WithCancel(context.Background())
	if !sender.Submit(ctx, endpoint, validBatch().Events) {
		t.Fatal("Submit returned false")
	}
	<-transport.started
	cancel()
	select {
	case <-transport.cancelled:
		close(transport.release)
		t.Fatal("accepted delivery was cancelled with its submitter")
	case <-time.After(20 * time.Millisecond):
	}
	close(transport.release)
	if err := sender.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSender_BoundsConcurrentHTTPAttempts(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		<-release
		active.Add(-1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := fastDeliveryConfig()
	cfg.MaxConcurrentRequests = 2
	sender := newSender(cfg, server.Client())
	endpoint := endpointForServer(t, server)
	for range 4 {
		if !sender.Submit(context.Background(), endpoint, validBatch().Events) {
			t.Fatal("Submit returned false")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent attempts = %d, want 2", got)
	}
	close(release)
	if err := sender.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
