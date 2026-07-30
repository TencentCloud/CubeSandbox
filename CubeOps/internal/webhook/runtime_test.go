// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestRuntime_ShutdownFlushesEndpointBuffers(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	parsedURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	endpoint := &Endpoint{ID: 0, Name: "audit", URL: parsedURL, BatchSize: 10}
	cfg := &Config{
		Delivery:  fastDeliveryConfig(),
		Endpoints: []*Endpoint{endpoint},
		Routes:    map[string][]*Endpoint{"sandbox.created": {endpoint}},
	}
	cfg.Delivery.EventQueueCapacity = 10
	cfg.Delivery.FlushIntervalSecs = 60
	runtime := NewRuntime(cfg)
	runtime.Start()
	if !runtime.Ingress().TryEnqueue(InternalBatch{Events: []Event{testEvent("sandbox.created")}}) {
		t.Fatal("enqueue failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-received:
	default:
		t.Fatal("shutdown did not deliver the partial endpoint batch")
	}
}

func TestRuntime_ShutdownDeadlineCancelsActiveDelivery(t *testing.T) {
	transport := &blockingTransport{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
	parsedURL, err := url.Parse("http://example.test/webhook")
	if err != nil {
		t.Fatal(err)
	}

	endpoint := &Endpoint{ID: 0, Name: "audit", URL: parsedURL, BatchSize: 1}
	cfg := &Config{
		Delivery:  fastDeliveryConfig(),
		Endpoints: []*Endpoint{endpoint},
		Routes:    map[string][]*Endpoint{"sandbox.created": {endpoint}},
	}
	cfg.Delivery.EventQueueCapacity = 10
	runtime := newRuntime(cfg, newSender(fastDeliveryConfig(), &http.Client{Transport: transport}))
	runtime.Start()
	if !runtime.Ingress().TryEnqueue(InternalBatch{Events: []Event{testEvent("sandbox.created")}}) {
		t.Fatal("enqueue failed")
	}
	<-transport.started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runtime.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown = nil, want deadline error")
	}
	select {
	case <-transport.cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown deadline did not cancel the active delivery")
	}
}
