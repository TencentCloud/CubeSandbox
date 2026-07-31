// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

func retries(value int) *int { return &value }

func TestDispatcherRetriesSignsAndKeepsEventID(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var body []byte
	var timestamp, signature string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		body, _ = io.ReadAll(request.Body)
		timestamp = request.Header.Get(timestampHeader)
		signature = request.Header.Get(signatureHeader)
		if attempts < 3 {
			http.Error(writer, "retry", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	dispatcher, err := newDispatcher(config.WebhookConfig{
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
		Endpoints: []config.WebhookEndpointConfig{{
			Name:       "receiver",
			URL:        server.URL,
			Events:     []string{EventSandboxCreated},
			Secret:     "test-secret",
			Timeout:    time.Second,
			MaxRetries: retries(2),
		}},
	})
	if err != nil {
		t.Fatalf("newDispatcher: %v", err)
	}
	event := LifecycleEvent{
		StreamID:  "1-0",
		EventID:   "stable-event-id",
		Op:        "create",
		SandboxID: "sandbox-1",
		Timestamp: 1700000000000,
		Payload:   json.RawMessage(`{"template_id":"template-1"}`),
	}
	if err := dispatcher.Deliver(t.Context(), event); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if signature != sign("test-secret", timestamp, body) {
		t.Fatalf("signature = %q, want valid HMAC", signature)
	}
	var payload Payload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.EventID != event.EventID || payload.Event != EventSandboxCreated {
		t.Fatalf("unexpected body: %+v", payload)
	}
}

func TestDispatcherFiltersSubscriptions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	dispatcher, err := newDispatcher(config.WebhookConfig{
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Endpoints: []config.WebhookEndpointConfig{{
			URL:        server.URL,
			Events:     []string{EventSandboxDeleted},
			Timeout:    time.Second,
			MaxRetries: retries(1),
		}},
	})
	if err != nil {
		t.Fatalf("newDispatcher: %v", err)
	}
	if err := dispatcher.Deliver(t.Context(), LifecycleEvent{
		EventID: "event-1", Op: "create", SandboxID: "sandbox-1", Timestamp: 1,
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want zero for unsubscribed event", requests)
	}
}

func TestDispatcherDoesNotRetryWhenMaxRetriesIsZero(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	dispatcher, err := newDispatcher(config.WebhookConfig{
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Endpoints: []config.WebhookEndpointConfig{{
			URL:        server.URL,
			Events:     []string{EventSandboxCreated},
			Timeout:    time.Second,
			MaxRetries: retries(0),
		}},
	})
	if err != nil {
		t.Fatalf("newDispatcher: %v", err)
	}
	err = dispatcher.Deliver(t.Context(), LifecycleEvent{
		EventID: "event-1", Op: "create", SandboxID: "sandbox-1", Timestamp: 1,
	})
	if err == nil {
		t.Fatal("Deliver succeeded, want receiver failure")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 when max_retries is zero", requests)
	}
}

func TestDispatcherRejectsNegativeMaxRetries(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.WebhookConfig
	}{
		{
			name: "default",
			cfg: config.WebhookConfig{
				DefaultMaxRetries: retries(-1),
			},
		},
		{
			name: "endpoint",
			cfg: config.WebhookConfig{
				Endpoints: []config.WebhookEndpointConfig{{
					URL:        "https://example.com/webhook",
					Events:     []string{EventSandboxCreated},
					MaxRetries: retries(-1),
				}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newDispatcher(test.cfg)
			if err == nil {
				t.Fatal("newDispatcher accepted negative max_retries")
			}
			if !strings.Contains(err.Error(), "max_retries") {
				t.Fatalf("newDispatcher error = %q, want max_retries validation error", err)
			}
		})
	}
}
