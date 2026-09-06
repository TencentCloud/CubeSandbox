// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// testExecutor returns an Executor whose build and job-lookup are faked, so the
// concurrency/dedup/shutdown logic is exercised without a DB or real builds.
// The returned started channel receives each jobID as its build begins.
func testExecutor(t *testing.T, maxConcurrent int, buildFn func(ctx context.Context, jobID string) error) (*Executor, chan string) {
	t.Helper()
	e := NewExecutor(maxConcurrent)
	started := make(chan string, 64)
	e.lookupJob = func(context.Context, string) error { return nil }
	e.build = func(ctx context.Context, jobID string, _ *types.CreateTemplateFromImageReq, _, _ string, _ []byte) error {
		started <- jobID
		return buildFn(ctx, jobID)
	}
	t.Cleanup(e.Shutdown)
	return e, started
}

func req() *types.CreateTemplateFromImageReq { return &types.CreateTemplateFromImageReq{} }

func TestExecutorRejectsDuplicateJobID(t *testing.T) {
	release := make(chan struct{})
	e, _ := testExecutor(t, 0, func(ctx context.Context, jobID string) error {
		<-release // first build blocks until released
		return nil
	})

	if err := e.Submit("job-1", req(), "", "", nil); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	// Wait until the first build is actually running so the duplicate check is
	// deterministic.
	e.mu.Lock()
	for len(e.inFlight) == 0 {
		e.mu.Unlock()
		time.Sleep(time.Millisecond)
		e.mu.Lock()
	}
	e.mu.Unlock()

	if err := e.Submit("job-1", req(), "", "", nil); !errors.Is(err, ErrBuildJobDuplicate) {
		t.Fatalf("duplicate submit: got %v, want ErrBuildJobDuplicate", err)
	}
	close(release)
}

func TestExecutorEnforcesConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	e, started := testExecutor(t, 1, func(ctx context.Context, jobID string) error {
		<-release
		return nil
	})

	if err := e.Submit("job-1", req(), "", "", nil); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	<-started // job-1 is now holding the only slot

	if err := e.Submit("job-2", req(), "", "", nil); !errors.Is(err, ErrBuildConcurrencyLimit) {
		t.Fatalf("second submit at capacity: got %v, want ErrBuildConcurrencyLimit", err)
	}
	close(release)
}

func TestExecutorReleasesSlotAfterBuild(t *testing.T) {
	e, started := testExecutor(t, 1, func(ctx context.Context, jobID string) error { return nil })

	if err := e.Submit("job-1", req(), "", "", nil); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	<-started

	// The build returns immediately, but slot/inflight release is async. Poll.
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		empty := len(e.inFlight) == 0
		e.mu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot was not released after build completed")
		}
		time.Sleep(time.Millisecond)
	}

	if err := e.Submit("job-2", req(), "", "", nil); err != nil {
		t.Fatalf("submit after slot freed failed: %v", err)
	}
	<-started
}

func TestExecutorShutdownCancelsInFlightBuilds(t *testing.T) {
	buildStarted := make(chan struct{}, 1)
	var sawCancel int32
	e := NewExecutor(0)
	e.lookupJob = func(context.Context, string) error { return nil }
	e.build = func(ctx context.Context, jobID string, _ *types.CreateTemplateFromImageReq, _, _ string, _ []byte) error {
		buildStarted <- struct{}{}
		<-ctx.Done() // block until Shutdown cancels the root context
		atomic.StoreInt32(&sawCancel, 1)
		return ctx.Err()
	}

	if err := e.Submit("job-1", req(), "", "", nil); err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	<-buildStarted

	done := make(chan struct{})
	go func() {
		e.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return: in-flight build was not cancelled")
	}
	if atomic.LoadInt32(&sawCancel) != 1 {
		t.Fatal("build did not observe context cancellation")
	}
}

func TestExecutorRunsBuildsConcurrentlyUpToLimit(t *testing.T) {
	var running int32
	var maxSeen int32
	release := make(chan struct{})
	e, started := testExecutor(t, 2, func(ctx context.Context, jobID string) error {
		cur := atomic.AddInt32(&running, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if cur <= old || atomic.CompareAndSwapInt32(&maxSeen, old, cur) {
				break
			}
		}
		<-release
		atomic.AddInt32(&running, -1)
		return nil
	})

	for _, id := range []string{"job-1", "job-2"} {
		if err := e.Submit(id, req(), "", "", nil); err != nil {
			t.Fatalf("submit %s failed: %v", id, err)
		}
	}
	<-started
	<-started
	// Give both a moment to register their concurrency.
	time.Sleep(20 * time.Millisecond)
	close(release)

	if got := atomic.LoadInt32(&maxSeen); got != 2 {
		t.Fatalf("max concurrent builds = %d, want 2", got)
	}
}

func TestExecutorSubmitPropagatesLookupError(t *testing.T) {
	e := NewExecutor(0)
	want := errors.New("job lookup failed")
	e.lookupJob = func(context.Context, string) error { return want }
	t.Cleanup(e.Shutdown)

	if err := e.Submit("job-1", req(), "", "", nil); !errors.Is(err, want) {
		t.Fatalf("got %v, want lookup error %v", err, want)
	}
	// A failed lookup must not register the job as in-flight.
	e.mu.Lock()
	n := len(e.inFlight)
	e.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed lookup left %d in-flight entries", n)
	}
}

func TestBuildableJobStatus(t *testing.T) {
	cases := map[string]bool{
		"PENDING": true,
		"RUNNING": true,
		"pending": false, // statuses are matched case-sensitively against the constants
		"BUILT":   false,
		"READY":   false,
		"FAILED":  false,
	}
	for status, want := range cases {
		if got := buildableJobStatus(status); got != want {
			t.Fatalf("buildableJobStatus(%q) = %t, want %t", status, got, want)
		}
	}
}
