// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

func TestReporterSuccess(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, reportPath, r.URL.Path)
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

	assert.Equal(t, int32(1), received.Load())
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

	assert.Equal(t, int32(3), attempts.Load())
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

	assert.Equal(t, int32(2), attempts.Load())
}

func TestReporterAuthHeadersSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify all 6 auth headers present
		for _, h := range []string{
			"cube_version", "cube_user_id", "cube_timestamp",
			"cube_nonce", "cube_sgn_method", "cube_signature",
		} {
			assert.NotEmpty(t, r.Header.Get(h), "auth header %s should not be empty", h)
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
		assert.Empty(t, r.Header.Get("cube_signature"), "unexpected cube_signature when auth disabled")
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
		require.NoError(t, json.Unmarshal(body, &received), "unmarshal body")
		assert.Equal(t, sent.EventID, received.EventID)
		assert.Equal(t, sent.PodName, received.PodName)
		assert.Equal(t, sent.Namespace, received.Namespace)
		assert.Equal(t, sent.NodeName, received.NodeName)
		assert.Equal(t, sent.InstanceType, received.InstanceType)
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

	assert.LessOrEqual(t, elapsed, 50*time.Millisecond, "Report should return immediately, took %v", elapsed)
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

	assert.Equal(t, goroutines, int(received.Load()))
}

func TestReporterContextCancelledDuringSend(t *testing.T) {
	// Use a server that blocks long enough for the context to cancel,
	// but responds eventually so the server can close cleanly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(1, time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	event := &types.EvictionEvent{EventID: "ctx-cancel", PodName: "p", Namespace: "ns", InterceptedAt: "t"}
	body, _ := json.Marshal(event)
	err := r.send(ctx, body)
	assert.Error(t, err, "send should return error when context is cancelled")
}

func TestReporterContextCancelledBetweenRetries(t *testing.T) {
	// Server always returns 500 to force retries, then ctx is cancelled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false, WithRetry(5, 50*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	event := &types.EvictionEvent{EventID: "ctx-retry", PodName: "p", Namespace: "ns", InterceptedAt: "t"}
	body, _ := json.Marshal(event)
	// reportWithRetry should return early when ctx is cancelled between retries.
	r.reportWithRetry(ctx, event, 5, 50*time.Millisecond)
	_ = body // used above for send test
}

func TestReporterSendNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	r := New(srv.URL, "", "", false)
	event := &types.EvictionEvent{EventID: "503", PodName: "p", Namespace: "ns", InterceptedAt: "t"}
	body, _ := json.Marshal(event)
	err := r.send(context.Background(), body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}
