// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package reporter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

func TestReporterSuccess(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != reportPath {
			t.Errorf("expected path %s, got %s", reportPath, r.URL.Path)
		}
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(1, time.Millisecond))
	event := &types.EvictionEvent{
		EventID:       "evt-001",
		PodName:       "sandbox-abc",
		Namespace:     "cube-system",
		NodeName:      "node-1",
		InstanceType:  "cubebox",
		InterceptedAt: "2026-07-23T10:00:00Z",
	}

	done := r.Report(event)
	<-done

	if received.Load() != 1 {
		t.Errorf("expected 1 request, got %d", received.Load())
	}
}

func TestReporterRetryThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(3, time.Millisecond))
	event := &types.EvictionEvent{
		EventID:       "evt-retry",
		PodName:       "sandbox-xyz",
		Namespace:     "cube-system",
		NodeName:      "node-2",
		InstanceType:  "cubebox",
		InterceptedAt: "2026-07-23T10:00:00Z",
	}

	done := r.Report(event)
	<-done

	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestReporterExhaustsRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(2, time.Millisecond))
	event := &types.EvictionEvent{
		EventID:       "evt-fail",
		PodName:       "sandbox-dead",
		Namespace:     "cube-system",
		NodeName:      "node-3",
		InstanceType:  "cubebox",
		InterceptedAt: "2026-07-23T10:00:00Z",
	}

	done := r.Report(event)
	<-done

	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestReporterAuthHeadersSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify all 6 auth headers present
		for _, h := range []string{
			"cube_version", "cube_user_id", "cube_timestamp",
			"cube_nonce", "cube_sgn_method", "cube_signature",
		} {
			if r.Header.Get(h) == "" {
				t.Errorf("auth header %s is empty", h)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "test-user", "test-secret", true, WithRetry(1, time.Millisecond))
	event := &types.EvictionEvent{
		EventID:       "evt-auth",
		PodName:       "sandbox-auth",
		Namespace:     "cube-system",
		NodeName:      "node-4",
		InstanceType:  "cubebox",
		InterceptedAt: "2026-07-23T10:00:00Z",
	}

	done := r.Report(event)
	<-done
}

func TestReporterAuthDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// When auth is disabled, no auth signature header should be present
		if sig := r.Header.Get("cube_signature"); sig != "" {
			t.Errorf("unexpected cube_signature when auth disabled: %s", sig)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(1, time.Millisecond))
	event := &types.EvictionEvent{EventID: "evt-noauth", PodName: "s", Namespace: "ns", InterceptedAt: "t"}

	done := r.Report(event)
	<-done
}

func TestReporterRequestBodyMatchesEvent(t *testing.T) {
	sent := &types.EvictionEvent{
		EventID:       "evt-body",
		PodName:       "sandbox-body",
		Namespace:     "cube-system",
		NodeName:      "node-body",
		InstanceType:  "cubebox",
		InterceptedAt: "2026-07-23T10:00:00Z",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var received types.EvictionEvent
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("unmarshal body: %v", err)
			return
		}
		if received.EventID != sent.EventID {
			t.Errorf("EventID: want %s, got %s", sent.EventID, received.EventID)
		}
		if received.PodName != sent.PodName {
			t.Errorf("PodName: want %s, got %s", sent.PodName, received.PodName)
		}
		if received.Namespace != sent.Namespace {
			t.Errorf("Namespace: want %s, got %s", sent.Namespace, received.Namespace)
		}
		if received.NodeName != sent.NodeName {
			t.Errorf("NodeName: want %s, got %s", sent.NodeName, received.NodeName)
		}
		if received.InstanceType != sent.InstanceType {
			t.Errorf("InstanceType: want %s, got %s", sent.InstanceType, received.InstanceType)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(1, time.Millisecond))
	done := r.Report(sent)
	<-done
}

func TestReporterAsyncReturn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // slow server
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false)
	event := &types.EvictionEvent{EventID: "async", PodName: "p", Namespace: "ns", InterceptedAt: "t"}

	start := time.Now()
	done := r.Report(event)
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Errorf("Report should return immediately, took %v", elapsed)
	}
	// Wait for completion so the test doesn't leak goroutines
	<-done
}

func TestReporterConcurrentReports(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(1, time.Millisecond))

	const goroutines = 10
	var doneChs []<-chan struct{}
	for i := 0; i < goroutines; i++ {
		event := &types.EvictionEvent{
			EventID:       fmt.Sprintf("evt-conc-%d", i),
			PodName:       fmt.Sprintf("sandbox-%d", i),
			Namespace:     "cube-system",
			NodeName:      "node-1",
			InstanceType:  "cubebox",
			InterceptedAt: "2026-07-23T10:00:00Z",
		}
		doneChs = append(doneChs, r.Report(event))
	}

	// Wait for all to complete
	for _, ch := range doneChs {
		<-ch
	}

	if int(received.Load()) != goroutines {
		t.Errorf("expected %d requests, got %d", goroutines, received.Load())
	}
}
