// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

func TestExternalHTTPScoreDefaultFailurePolicyIsFailOpen(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL)).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() error = nil, want plain fail-open error for observability")
	}
	if IsFailClosed(err) {
		t.Fatalf("default failure_policy error type = %T (%v), want plain error (fail_open)", err, err)
	}
}

func TestExternalHTTPScoreFailOpenOnHTTP5xx(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := testPluginConfig(server.URL)
	cfg.FailurePolicy = "fail_open"
	got, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx())
	if IsFailClosed(err) {
		t.Fatalf("fail_open Select() wrapped as FailClosedError: %v", err)
	}
	if err == nil {
		t.Fatal("fail_open Select() error = nil, want plain error (runScoreFilter still skips)")
	}
	if got != nil {
		t.Fatalf("fail_open Select() = %+v, want nil scores", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("endpoint hits = %d, want 1", hits.Load())
	}
}

func TestExternalHTTPScoreFailClosedOnHTTP5xx(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := testPluginConfig(server.URL)
	cfg.FailurePolicy = "fail_closed"
	_, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("fail_closed Select() error = nil, want HTTP status error")
	}
	if !IsFailClosed(err) {
		t.Fatalf("fail_closed error type = %T (%v), want FailClosedError", err, err)
	}
}

func TestExternalHTTPScoreFailOpenOnTimeout(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10), "node-b": float64Ptr(90)},
		})
	}))
	defer server.Close()

	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      server.URL,
		Timeout:       10 * time.Millisecond,
		Mode:          "prefer-node-b",
		FailurePolicy: "fail_open",
	}
	got, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx())
	if IsFailClosed(err) {
		t.Fatalf("fail_open timeout wrapped as FailClosedError: %v", err)
	}
	if err == nil {
		t.Fatal("fail_open timeout Select() error = nil, want plain error")
	}
	if got != nil {
		t.Fatalf("fail_open timeout Select() = %+v, want nil scores", got)
	}
}

func TestExternalHTTPScoreFailOpenOnMalformedResponse(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer server.Close()

	cfg := testPluginConfig(server.URL)
	cfg.FailurePolicy = "fail_open"
	got, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx())
	if IsFailClosed(err) {
		t.Fatalf("fail_open malformed wrapped as FailClosedError: %v", err)
	}
	if err == nil {
		t.Fatal("fail_open malformed Select() error = nil, want plain error")
	}
	if got != nil {
		t.Fatalf("fail_open malformed Select() = %+v, want nil scores", got)
	}
}

func TestExternalHTTPScoreFailOpenOnPartialNodes(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10)},
		})
	}))
	defer server.Close()

	cfg := testPluginConfig(server.URL)
	cfg.FailurePolicy = "fail_open"
	got, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx())
	if IsFailClosed(err) {
		t.Fatalf("fail_open partial-node wrapped as FailClosedError: %v", err)
	}
	if err == nil {
		t.Fatal("fail_open partial-node Select() error = nil, want plain error")
	}
	if got != nil {
		t.Fatalf("fail_open partial-node Select() = %+v, want nil scores", got)
	}
}

func TestExternalHTTPScoreFailClosedOnPartialNodes(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10)},
		})
	}))
	defer server.Close()

	cfg := testPluginConfig(server.URL)
	cfg.FailurePolicy = "fail_closed"
	_, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("fail_closed partial-node Select() error = nil, want missing node error")
	}
	if !IsFailClosed(err) {
		t.Fatalf("fail_closed partial-node error type = %T (%v), want FailClosedError", err, err)
	}
}

func TestExternalHTTPScoreCircuitOpensAndSkipsFullTimeout(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(80 * time.Millisecond)
	}))
	defer server.Close()

	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      server.URL,
		Timeout:       20 * time.Millisecond,
		FailurePolicy: "fail_closed",
		CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
			FailureThreshold:  2,
			OpenDuration:      5 * time.Second,
			HalfOpenMaxProbes: 1,
		},
	}
	scorer := newExternalHTTPScoreWithConfig(cfg)
	for i := 0; i < 2; i++ {
		if _, err := scorer.Select(externalHTTPScoreTestCtx()); err == nil {
			t.Fatalf("attempt %d: Select() error = nil, want timeout error", i+1)
		}
	}

	start := time.Now()
	_, err := scorer.Select(externalHTTPScoreTestCtx())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("open-circuit Select() error = nil, want circuit-open error")
	}
	if !IsFailClosed(err) {
		t.Fatalf("open-circuit error type = %T (%v), want FailClosedError", err, err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("open-circuit Select() took %s, want fast reject without waiting for HTTP timeout (%s)", elapsed, cfg.Timeout)
	}
	if hits.Load() != 2 {
		t.Fatalf("endpoint hits = %d, want 2 (no request while circuit is open)", hits.Load())
	}
	if got := externalHTTPScoreCircuitStateValue(); got != 2 {
		t.Fatalf("circuit state = %v, want 2 (open)", got)
	}
	if reason := externalHTTPScoreMetricReason(sanitizeExternalHTTPScoreFailure(err)); reason != externalHTTPScoreReasonCircuitOpen {
		t.Fatalf("open-circuit metric reason = %q, want %q", reason, externalHTTPScoreReasonCircuitOpen)
	}
}

func TestExternalHTTPScoreCircuitHalfOpenRecovers(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10), "node-b": float64Ptr(90)},
		})
	}))
	defer server.Close()

	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      server.URL,
		Timeout:       time.Second,
		FailurePolicy: "fail_closed",
		CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
			FailureThreshold:  2,
			OpenDuration:      40 * time.Millisecond,
			HalfOpenMaxProbes: 1,
		},
	}
	scorer := newExternalHTTPScoreWithConfig(cfg)
	for i := 0; i < 2; i++ {
		if _, err := scorer.Select(externalHTTPScoreTestCtx()); err == nil {
			t.Fatalf("attempt %d: Select() error = nil, want 5xx error", i+1)
		}
	}

	time.Sleep(50 * time.Millisecond)
	healthy.Store(true)

	got, err := scorer.Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("half-open probe Select() error = %v, want recovery success", err)
	}
	if got.Len() != 2 {
		t.Fatalf("recovered scores len = %d, want 2", got.Len())
	}
	if state := externalHTTPScoreCircuitStateValue(); state != 0 {
		t.Fatalf("circuit state after recovery = %v, want 0 (closed)", state)
	}

	got, err = scorer.Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("post-recovery Select() error = %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("post-recovery scores len = %d, want 2", got.Len())
	}
}

func TestExternalHTTPScoreCircuitDisabledAlwaysAllows(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := testPluginConfig(server.URL)
	cfg.FailurePolicy = "fail_closed"
	cfg.CircuitBreaker = &config.ExternalHTTPScoreCircuitBreaker{
		Disable:          true,
		FailureThreshold: 1,
	}
	scorer := newExternalHTTPScoreWithConfig(cfg)
	for i := 0; i < 3; i++ {
		if _, err := scorer.Select(externalHTTPScoreTestCtx()); !IsFailClosed(err) {
			t.Fatalf("attempt %d: error = %v, want FailClosedError", i+1, err)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("endpoint hits = %d, want 3 (breaker disabled)", hits.Load())
	}
}

func TestCircuitTargetLabelStripsCredentialsAndQuery(t *testing.T) {
	tests := []struct {
		endpoint string
		want     string
	}{
		{endpoint: "https://user:secret@sidecar.internal:8443/score?token=abc", want: "sidecar.internal:8443"},
		{endpoint: "http://127.0.0.1:9090/v1?api_key=1", want: "127.0.0.1:9090"},
		{endpoint: "not a url", want: "invalid"},
		{endpoint: "", want: "invalid"},
	}
	for _, tt := range tests {
		if got := circuitTargetLabel(tt.endpoint); got != tt.want {
			t.Errorf("circuitTargetLabel(%q) = %q, want %q", tt.endpoint, got, tt.want)
		}
	}
}

func TestExternalHTTPScoreCircuitStateUsesHostLabelNotFullURL(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	unsafeEndpoint := server.URL + "/score?token=super-secret"
	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      unsafeEndpoint,
		Timeout:       time.Second,
		FailurePolicy: "fail_closed",
		CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
			FailureThreshold:  1,
			OpenDuration:      5 * time.Second,
			HalfOpenMaxProbes: 1,
		},
	}
	if _, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx()); err == nil {
		t.Fatal("Select() error = nil, want fail_closed error")
	}

	wantHost := circuitTargetLabel(unsafeEndpoint)
	if wantHost == "invalid" || strings.Contains(wantHost, "token=") || strings.Contains(wantHost, "/") {
		t.Fatalf("sanitized target %q still looks like a full URL", wantHost)
	}

	labels := gatherCircuitStateTargets(t)
	if _, ok := labels[wantHost]; !ok {
		t.Fatalf("circuit_state labels = %v, want host %q", labels, wantHost)
	}
	for label := range labels {
		if strings.Contains(label, "token=") || strings.Contains(label, "://") || strings.Contains(label, "@") {
			t.Fatalf("circuit_state label %q contains credentials or full URL", label)
		}
	}
}

func TestExternalHTTPScoreEndpointHostChangeClearsPreviousCircuit(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	openCircuit := func(t *testing.T) string {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		cfg := &config.ExternalHTTPScore{
			Weight:        float64Ptr(1),
			Endpoint:      server.URL,
			Timeout:       time.Second,
			FailurePolicy: "fail_closed",
			CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
				FailureThreshold: 1,
				OpenDuration:     5 * time.Second,
			},
		}
		if _, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx()); err == nil {
			t.Fatal("Select() error = nil, want circuit-trip error")
		}
		return circuitTargetLabel(server.URL)
	}

	hostA := openCircuit(t)
	labels := gatherCircuitStateTargets(t)
	if labels[hostA] != float64(circuitStateOpen) {
		t.Fatalf("host A state = %v, want open; labels=%v", labels[hostA], labels)
	}

	hostB := openCircuit(t)
	if hostA == hostB {
		t.Fatalf("test servers shared host label %q", hostA)
	}

	labels = gatherCircuitStateTargets(t)
	if _, ok := labels[hostA]; ok {
		t.Fatalf("abandoned host A still exported after endpoint change; labels=%v", labels)
	}
	if labels[hostB] != float64(circuitStateOpen) {
		t.Fatalf("host B state = %v, want open; labels=%v", labels[hostB], labels)
	}
}

func TestExternalHTTPScoreRetiredBreakerDoesNotResurrectGauge(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	const target = "abandoned.example:8080"
	b := newExternalHTTPScoreBreaker(target, &config.ExternalHTTPScoreCircuitBreaker{
		FailureThreshold: 1,
		OpenDuration:     time.Hour,
	})
	externalHTTPScoreBreakersMu.Lock()
	externalHTTPScoreBreakers[target] = b
	externalHTTPScoreActiveTarget = target
	externalHTTPScoreBreakersMu.Unlock()

	b.recordFailure()
	labels := gatherCircuitStateTargets(t)
	if labels[target] != float64(circuitStateOpen) {
		t.Fatalf("before clear: state = %v, want open; labels=%v", labels[target], labels)
	}

	clearExternalHTTPScoreCircuitTarget(target)
	labels = gatherCircuitStateTargets(t)
	if _, ok := labels[target]; ok {
		t.Fatalf("after clear: abandoned target still exported; labels=%v", labels)
	}

	// In-flight caller still holds the old pointer after endpoint host change.
	b.recordFailure()
	b.mu.Lock()
	b.state = circuitStateOpen
	b.openedAt = time.Now().Add(-2 * time.Hour)
	b.mu.Unlock()
	_ = b.allow() // would publish half-open if not retired
	b.recordSuccess()
	labels = gatherCircuitStateTargets(t)
	if _, ok := labels[target]; ok {
		t.Fatalf("retired breaker resurrected circuit_state; labels=%v", labels)
	}
}

func TestExternalHTTPScoreStaleInFlightSuccessDoesNotCloseOpenCircuit(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	b := newExternalHTTPScoreBreaker("sidecar.example:8080", &config.ExternalHTTPScoreCircuitBreaker{
		FailureThreshold:  3,
		OpenDuration:      time.Hour,
		HalfOpenMaxProbes: 1,
	})
	// Admit while closed (no half-open reservation), then trip open, then a
	// late success from the pre-open request must not cancel the open window.
	if err := b.allow(); err != nil {
		t.Fatalf("allow() while closed: %v", err)
	}
	for i := 0; i < 3; i++ {
		b.recordFailure()
	}
	b.mu.Lock()
	if b.state != circuitStateOpen {
		b.mu.Unlock()
		t.Fatalf("state = %d, want open", b.state)
	}
	openedAt := b.openedAt
	failures := b.consecutiveFailures
	b.mu.Unlock()

	b.recordSuccess() // stale in-flight success

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != circuitStateOpen {
		t.Fatalf("stale success closed circuit: state = %d", b.state)
	}
	if b.consecutiveFailures != failures {
		t.Fatalf("consecutiveFailures = %d, want %d", b.consecutiveFailures, failures)
	}
	if !b.openedAt.Equal(openedAt) {
		t.Fatalf("openedAt mutated by stale success")
	}
}

func TestExternalHTTPScoreHalfOpenAllowsSingleProbe(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10), "node-b": float64Ptr(90)},
		})
	}))
	defer server.Close()

	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      server.URL,
		Timeout:       2 * time.Second,
		FailurePolicy: "fail_closed",
		CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
			FailureThreshold:  1,
			OpenDuration:      30 * time.Millisecond,
			HalfOpenMaxProbes: 1,
		},
	}
	scorer := newExternalHTTPScoreWithConfig(cfg)
	if _, err := scorer.Select(externalHTTPScoreTestCtx()); err == nil {
		t.Fatal("first Select() error = nil, want 5xx to open circuit")
	}
	time.Sleep(40 * time.Millisecond)

	probeErr := make(chan error, 1)
	go func() {
		_, err := scorer.Select(externalHTTPScoreTestCtx())
		probeErr <- err
	}()
	<-started
	// Second Select must be rejected while probe 1 is still blocked in HTTP.
	secondErr := make(chan error, 1)
	go func() {
		_, err := scorer.Select(externalHTTPScoreTestCtx())
		secondErr <- err
	}()
	second := <-secondErr
	if !IsFailClosed(second) || !errors.Is(second, errExternalHTTPScoreCircuitOpen) {
		close(release)
		t.Fatalf("concurrent half-open Select() = %v, want circuit_open FailClosedError", second)
	}
	close(release)
	first := <-probeErr
	if first != nil {
		t.Fatalf("half-open probe Select() error = %v, want success", first)
	}
	if hits.Load() != 2 {
		t.Fatalf("endpoint hits = %d, want 2 (trip + one half-open probe)", hits.Load())
	}
}

func TestExternalHTTPScoreApplyConfigConcurrentWithAllow(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	b := newExternalHTTPScoreBreaker("sidecar.example:8080", &config.ExternalHTTPScoreCircuitBreaker{
		FailureThreshold:  8,
		OpenDuration:      time.Second,
		HalfOpenMaxProbes: 1,
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			b.applyConfig(&config.ExternalHTTPScoreCircuitBreaker{
				FailureThreshold:  4 + i%3,
				OpenDuration:      50 * time.Millisecond,
				HalfOpenMaxProbes: 1,
			})
		}(i)
		go func() {
			defer wg.Done()
			_ = b.allow()
			b.recordSuccess()
		}()
	}
	wg.Wait()
}

func TestExternalHTTPScoreApplyConfigResetsToDefaults(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	b := newExternalHTTPScoreBreaker("sidecar.example:8080", &config.ExternalHTTPScoreCircuitBreaker{
		FailureThreshold:  20,
		OpenDuration:      30 * time.Second,
		HalfOpenMaxProbes: 4,
	})
	b.mu.Lock()
	if b.failureThreshold != 20 || b.openDuration != 30*time.Second || b.halfOpenMaxProbes != 4 {
		b.mu.Unlock()
		t.Fatalf("initial tunables = threshold=%d duration=%s probes=%d", b.failureThreshold, b.openDuration, b.halfOpenMaxProbes)
	}
	b.mu.Unlock()

	b.applyConfig(nil)
	b.mu.Lock()
	if b.failureThreshold != defaultCircuitFailureThreshold ||
		b.openDuration != defaultCircuitOpenDuration ||
		b.halfOpenMaxProbes != defaultCircuitHalfOpenMaxProbes {
		b.mu.Unlock()
		t.Fatalf("after nil apply: threshold=%d duration=%s probes=%d, want defaults", b.failureThreshold, b.openDuration, b.halfOpenMaxProbes)
	}
	b.mu.Unlock()

	b.applyConfig(&config.ExternalHTTPScoreCircuitBreaker{
		FailureThreshold:  20,
		OpenDuration:      30 * time.Second,
		HalfOpenMaxProbes: 4,
	})
	b.applyConfig(&config.ExternalHTTPScoreCircuitBreaker{}) // all zeros → defaults
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failureThreshold != defaultCircuitFailureThreshold ||
		b.openDuration != defaultCircuitOpenDuration ||
		b.halfOpenMaxProbes != defaultCircuitHalfOpenMaxProbes {
		t.Fatalf("after zero-field apply: threshold=%d duration=%s probes=%d, want defaults", b.failureThreshold, b.openDuration, b.halfOpenMaxProbes)
	}
}

func TestExternalHTTPScoreHalfOpenReArmsAfterLeakedProbe(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)
	circuitHalfOpenProbeLeakTimeoutForTest = 30 * time.Millisecond
	t.Cleanup(func() { circuitHalfOpenProbeLeakTimeoutForTest = 0 })

	b := newExternalHTTPScoreBreaker("sidecar.example:8080", &config.ExternalHTTPScoreCircuitBreaker{
		FailureThreshold:  1,
		OpenDuration:      30 * time.Millisecond,
		HalfOpenMaxProbes: 1,
	})
	b.recordFailure() // open
	if err := b.allow(); !errors.Is(err, errExternalHTTPScoreCircuitOpen) {
		t.Fatalf("immediate allow after open = %v, want circuit open", err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := b.allow(); err != nil {
		t.Fatalf("first half-open probe allow() = %v, want nil", err)
	}
	// Leak the probe slot (no recordSuccess/recordFailure) — previously wedged forever.
	if err := b.allow(); !errors.Is(err, errExternalHTTPScoreCircuitOpen) {
		t.Fatalf("second half-open allow while slot held = %v, want circuit open", err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := b.allow(); err != nil {
		t.Fatalf("half-open re-arm after leak timeout = %v, want nil", err)
	}
}

func TestExternalHTTPScoreDisableClearsCircuitStateGauge(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      server.URL,
		Timeout:       time.Second,
		FailurePolicy: "fail_closed",
		CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
			FailureThreshold: 1,
			OpenDuration:     5 * time.Second,
		},
	}
	if _, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx()); err == nil {
		t.Fatal("Select() error = nil, want failure to open circuit")
	}
	target := circuitTargetLabel(server.URL)
	labels := gatherCircuitStateTargets(t)
	if labels[target] != float64(circuitStateOpen) {
		t.Fatalf("before disable: state = %v, want open", labels[target])
	}

	cfg.CircuitBreaker.Disable = true
	if _, err := newExternalHTTPScoreWithConfig(cfg).Select(externalHTTPScoreTestCtx()); err == nil {
		t.Fatal("disabled breaker still failed Select(); want HTTP error or success path without circuit_open")
	}
	labels = gatherCircuitStateTargets(t)
	if _, ok := labels[target]; ok {
		t.Fatalf("after disable: target %q still present in circuit_state labels=%v", target, labels)
	}
}

func TestExternalHTTPScorePanicAfterAllowReleasesHalfOpenSlot(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10), "node-b": float64Ptr(90)},
		})
	}))
	defer server.Close()

	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      server.URL,
		Timeout:       time.Second,
		FailurePolicy: "fail_closed",
		CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
			FailureThreshold:  1,
			OpenDuration:      20 * time.Millisecond,
			HalfOpenMaxProbes: 1,
		},
	}
	scorer := newExternalHTTPScoreWithConfig(cfg)
	if _, err := scorer.Select(externalHTTPScoreTestCtx()); err == nil {
		t.Fatal("Select() error = nil, want failure to open circuit")
	}

	time.Sleep(30 * time.Millisecond)
	healthy.Store(true)

	externalHTTPScoreAfterAllowForTest = func() { panic("forced after allow") }
	defer func() { externalHTTPScoreAfterAllowForTest = nil }()

	_, err := scorer.Select(externalHTTPScoreTestCtx())
	if err == nil {
		t.Fatal("Select() after forced panic = nil, want recovered error")
	}
	externalHTTPScoreAfterAllowForTest = nil

	// Without recordFailure on panic recovery the half-open slot would wedge.
	// After recovery the circuit should be Open again (failed probe), then
	// after openDuration a new half-open probe must be allowed.
	time.Sleep(30 * time.Millisecond)
	got, err := scorer.Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() after panic-released half-open = %v, want success", err)
	}
	if got.Len() != 2 {
		t.Fatalf("scores len = %d, want 2", got.Len())
	}
}

func TestFailClosedErrorGRPCStatusIsSelectNodesFailed(t *testing.T) {
	closed := &FailClosedError{Err: errExternalHTTPScoreCircuitOpen}
	status, ok := ret.FromError(closed)
	if !ok {
		t.Fatal("FromError(FailClosedError) ok = false, want true via GRPCStatus")
	}
	if status.Code() != errorcode.ErrorCode_SelectNodesFailed {
		t.Fatalf("RetCode = %v, want SelectNodesFailed", status.Code())
	}
	if status.Message() == "" || strings.Contains(status.Message(), "http://") {
		t.Fatalf("RetMsg = %q, want sanitized non-empty category", status.Message())
	}
	// Wrapping breaks ret.FromError's direct type assertion while IsFailClosed
	// still matches — document the create-path contract.
	wrapped := fmt.Errorf("schedule: %w", closed)
	if !IsFailClosed(wrapped) {
		t.Fatal("IsFailClosed(wrapped) = false, want true")
	}
	if _, ok := ret.FromError(wrapped); ok {
		t.Fatal("FromError(wrapped FailClosedError) ok = true; wrapping must not pretend to carry GRPCStatus")
	}
}

func TestExternalHTTPScoreFailClosedHonorsConfigValidationErrors(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	emptyEndpoint := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      "   ",
		Timeout:       time.Second,
		FailurePolicy: "fail_closed",
	}
	_, err := newExternalHTTPScoreWithConfig(emptyEndpoint).Select(externalHTTPScoreTestCtx())
	if !IsFailClosed(err) {
		t.Fatalf("empty endpoint under fail_closed: type=%T (%v), want FailClosedError", err, err)
	}

	// Construction rejects a permanently bad endpoint; hot-reload can still
	// mutate the live block after a valid constructor, which Select must honor.
	badEndpoint := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      "http://127.0.0.1:9/score",
		Timeout:       time.Second,
		FailurePolicy: "fail_closed",
	}
	scorer := newExternalHTTPScoreWithConfig(badEndpoint)
	badEndpoint.Endpoint = "127.0.0.1:18080/score" // missing scheme
	_, err = scorer.Select(externalHTTPScoreTestCtx())
	if !IsFailClosed(err) {
		t.Fatalf("invalid endpoint under fail_closed: type=%T (%v), want FailClosedError", err, err)
	}
}

func TestExternalHTTPScoreCallerCancelDoesNotTripBreaker(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10), "node-b": float64Ptr(90)},
		})
	}))
	defer server.Close()

	cfg := &config.ExternalHTTPScore{
		Weight:        float64Ptr(1),
		Endpoint:      server.URL,
		Timeout:       time.Second,
		FailurePolicy: "fail_closed",
		CircuitBreaker: &config.ExternalHTTPScoreCircuitBreaker{
			FailureThreshold:  2,
			OpenDuration:      time.Second,
			HalfOpenMaxProbes: 1,
		},
	}
	scorer := newExternalHTTPScoreWithConfig(cfg)

	for i := 0; i < 3; i++ {
		parent, cancel := context.WithCancel(context.Background())
		selCtx := externalHTTPScoreTestCtx()
		selCtx.Ctx = parent
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		_, err := scorer.Select(selCtx)
		if err == nil {
			t.Fatalf("attempt %d: Select() error = nil, want cancel error", i+1)
		}
	}

	// Cancels must not open the circuit; a healthy request should still succeed.
	got, err := scorer.Select(externalHTTPScoreTestCtx())
	if err != nil {
		t.Fatalf("Select() after cancels = %v, want success (breaker must stay closed)", err)
	}
	if got.Len() != 2 {
		t.Fatalf("scores len = %d, want 2", got.Len())
	}
	if state := externalHTTPScoreCircuitStateValue(); state != 0 {
		t.Fatalf("circuit state = %v, want 0 (closed) after caller cancels", state)
	}
}

func TestExternalHTTPScoreReleaseProbeAllowsNextHalfOpen(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	b := newExternalHTTPScoreBreaker("sidecar.example:8080", &config.ExternalHTTPScoreCircuitBreaker{
		FailureThreshold:  1,
		OpenDuration:      20 * time.Millisecond,
		HalfOpenMaxProbes: 1,
	})
	b.recordFailure()
	time.Sleep(25 * time.Millisecond)
	if err := b.allow(); err != nil {
		t.Fatalf("half-open allow() = %v", err)
	}
	b.releaseProbe()
	if err := b.allow(); err != nil {
		t.Fatalf("allow() after releaseProbe = %v, want nil", err)
	}
}

func TestExternalHTTPScoreCircuitMetricsAreRegistered(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)
	setExternalHTTPScoreCircuitState("invalid", circuitStateClosed)

	want := map[string]bool{
		"cube_scheduler_external_http_score_circuit_state": false,
		"cube_scheduler_external_http_score_outcomes_total": false,
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, fam := range families {
		if _, ok := want[fam.GetName()]; ok {
			want[fam.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %s is not registered", name)
		}
	}
}

func TestExternalHTTPScoreConcurrentSelect(t *testing.T) {
	resetExternalHTTPScoreRuntimeForTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(externalHTTPScoreResponse{
			Scores: map[string]*float64{"node-a": float64Ptr(10), "node-b": float64Ptr(90)},
		})
	}))
	defer server.Close()

	scorer := newExternalHTTPScoreWithConfig(testPluginConfig(server.URL))
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := scorer.Select(externalHTTPScoreTestCtx())
			if err != nil {
				errCh <- err
				return
			}
			if got.Len() != 2 {
				errCh <- fmt.Errorf("len(scores) = %d, want 2", got.Len())
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Select() error = %v", err)
		}
	}
}

func gatherCircuitStateTargets(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	out := map[string]float64{}
	for _, fam := range families {
		if fam.GetName() != "cube_scheduler_external_http_score_circuit_state" {
			continue
		}
		for _, m := range fam.Metric {
			label := ""
			for _, lp := range m.Label {
				if lp.GetName() == "target" {
					label = lp.GetValue()
				}
			}
			if m.Gauge != nil {
				out[label] = m.Gauge.GetValue()
			}
		}
	}
	return out
}

func resetExternalHTTPScoreRuntimeForTest(t *testing.T) {
	t.Helper()
	resetExternalHTTPScoreRuntime()
	t.Cleanup(resetExternalHTTPScoreRuntime)
}
