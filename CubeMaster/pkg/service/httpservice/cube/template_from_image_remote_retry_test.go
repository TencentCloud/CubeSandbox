// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/tcclient"
)

// shrinkRetryDelays swaps forwardBuildJobRetryDelays for near-zero waits so
// retry tests run fast, restoring the original on cleanup.
func shrinkRetryDelays(t *testing.T) {
	t.Helper()
	orig := forwardBuildJobRetryDelays
	forwardBuildJobRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { forwardBuildJobRetryDelays = orig })
}

func stubSubmitBuildJob(t *testing.T, fn func(attempt int) error) *int {
	t.Helper()
	orig := submitBuildJobFn
	attempts := 0
	submitBuildJobFn = func(ctx context.Context, endpoint, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL, envdSHA string, envdData []byte) error {
		attempts++
		return fn(attempts)
	}
	t.Cleanup(func() { submitBuildJobFn = orig })
	return &attempts
}

func TestSubmitBuildJobWithRetrySuccessFirstTry(t *testing.T) {
	attempts := stubSubmitBuildJob(t, func(int) error { return nil })
	if err := submitBuildJobWithRetry(context.Background(), "http://tc", "job-1", nil, "", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *attempts != 1 {
		t.Fatalf("attempts = %d, want 1", *attempts)
	}
}

// This is the P1 regression test: 429/503 used to be treated as a permanent
// failure (job marked FAILED on the first hiccup). It must now be retried
// and succeed once TC recovers.
func TestSubmitBuildJobWithRetryTransientThenSucceeds(t *testing.T) {
	shrinkRetryDelays(t)
	attempts := stubSubmitBuildJob(t, func(n int) error {
		if n < 3 {
			return &tcclient.StatusError{StatusCode: http.StatusTooManyRequests, Body: "busy"}
		}
		return nil
	})
	if err := submitBuildJobWithRetry(context.Background(), "http://tc", "job-2", nil, "", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *attempts != 3 {
		t.Fatalf("attempts = %d, want 3", *attempts)
	}
}

// A permanent 4xx (e.g. bad request) must NOT be retried.
func TestSubmitBuildJobWithRetryPermanentErrorNoRetry(t *testing.T) {
	shrinkRetryDelays(t)
	attempts := stubSubmitBuildJob(t, func(int) error {
		return &tcclient.StatusError{StatusCode: http.StatusBadRequest, Body: "bad spec"}
	})
	err := submitBuildJobWithRetry(context.Background(), "http://tc", "job-3", nil, "", "", nil)
	if err == nil {
		t.Fatal("expected error for permanent 400")
	}
	if *attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry for permanent errors)", *attempts)
	}
}

// 409 (TC already has this job_id in flight, e.g. a duplicate forward) must
// be treated as success, not failure -- the P0 double-forward safety net.
func TestSubmitBuildJobWithRetry409TreatedAsSuccess(t *testing.T) {
	attempts := stubSubmitBuildJob(t, func(int) error {
		return &tcclient.StatusError{StatusCode: http.StatusConflict, Body: "already submitted"}
	})
	if err := submitBuildJobWithRetry(context.Background(), "http://tc", "job-4", nil, "", "", nil); err != nil {
		t.Fatalf("expected 409 to be treated as success, got err=%v", err)
	}
	if *attempts != 1 {
		t.Fatalf("attempts = %d, want 1", *attempts)
	}
}

// Retries must eventually give up and surface the last error.
func TestSubmitBuildJobWithRetryExhausted(t *testing.T) {
	shrinkRetryDelays(t)
	attempts := stubSubmitBuildJob(t, func(int) error {
		return &tcclient.StatusError{StatusCode: http.StatusServiceUnavailable, Body: "still busy"}
	})
	err := submitBuildJobWithRetry(context.Background(), "http://tc", "job-5", nil, "", "", nil)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	wantAttempts := len(forwardBuildJobRetryDelays) + 1
	if *attempts != wantAttempts {
		t.Fatalf("attempts = %d, want %d", *attempts, wantAttempts)
	}
}

// Non-StatusError errors (network failure, DNS, etc.) are also retried:
// they are just as transient as a 503 from TC's perspective.
func TestSubmitBuildJobWithRetryNetworkErrorIsRetried(t *testing.T) {
	shrinkRetryDelays(t)
	attempts := stubSubmitBuildJob(t, func(n int) error {
		if n < 2 {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	})
	if err := submitBuildJobWithRetry(context.Background(), "http://tc", "job-6", nil, "", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *attempts != 2 {
		t.Fatalf("attempts = %d, want 2", *attempts)
	}
}
