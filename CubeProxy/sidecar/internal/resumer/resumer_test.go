// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package resumer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/registry"
)

// fakeStore is the resumer-side test double for the redisstream client.
type fakeStore struct {
	mu     sync.Mutex
	states map[string]string
	// allowAcquire controls whether AcquireState succeeds. When the second
	// element is non-empty, AcquireState seeds that state value into the
	// map (simulating a peer holding the lock) and returns false.
	preLocked map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: make(map[string]string), preLocked: make(map[string]string)}
}

func (f *fakeStore) AcquireState(_ context.Context, sid, state string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.preLocked[sid]; ok {
		f.states[sid] = v
		return false, nil
	}
	if _, ok := f.states[sid]; ok {
		return false, nil
	}
	f.states[sid] = state
	return true, nil
}

func (f *fakeStore) SetState(_ context.Context, sid, state string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[sid] = state
	return nil
}

func (f *fakeStore) ClearState(_ context.Context, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.states, sid)
	return nil
}

func (f *fakeStore) GetState(_ context.Context, sid string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.states[sid]
	return v, ok, nil
}

func (f *fakeStore) state(sid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[sid]
}

// fakeMaster captures Resume calls; supports artificial latency to exercise
// the in-flight de-duplication path.
type fakeMaster struct {
	mu        sync.Mutex
	calls     int32
	latency   time.Duration
	failNext  bool
	failError error
}

func (f *fakeMaster) Resume(ctx context.Context, _, _ string) error {
	atomic.AddInt32(&f.calls, 1)
	if f.latency > 0 {
		select {
		case <-time.After(f.latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return f.failError
	}
	return nil
}

type fakePush struct {
	mu      sync.Mutex
	pushed  []string
	deleted []string
}

func (f *fakePush) SetState(_ context.Context, _, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, state)
	return nil
}

func (f *fakePush) DeleteMeta(_ context.Context, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, sid)
	return nil
}

func newTestResumer(reg *registry.Registry, store *fakeStore, master *fakeMaster, push *fakePush) *Resumer {
	return New(Options{
		Registry:     reg,
		Redis:        store,
		CubeMaster:   master,
		ProxyPush:    push,
		StateLockTTL: 30 * time.Second,
		Log:          zap.NewNop(),
	})
}

func TestResumer_HappyPath(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
		CreatedAt: time.Now().Add(-1 * time.Hour).UnixMilli(), // ancient
	})
	store := newFakeStore()
	master := &fakeMaster{}
	push := &fakePush{}

	beforeResume := time.Now().UnixMilli()
	r := newTestResumer(reg, store, master, push)
	if err := r.Resume(context.Background(), "sbx"); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if got := atomic.LoadInt32(&master.calls); got != 1 {
		t.Fatalf("expected 1 master.Resume call, got %d", got)
	}
	if got := store.state("sbx"); got != "running" {
		t.Fatalf("expected redis state=running, got %q", got)
	}
	// Regression: a successful resume must reset the registry's
	// LastActiveMs to "now" — otherwise the sweeper, on its next 5s
	// tick, computes idle = now - CreatedAt (hours ago) and noisily
	// logs "idle threshold exceeded" for a sandbox we just woke up.
	entry := reg.Get("sbx")
	if entry == nil {
		t.Fatal("registry entry missing after resume")
	}
	if entry.LastActiveMs < beforeResume {
		t.Fatalf("LastActiveMs not refreshed: got %d, expected >= %d",
			entry.LastActiveMs, beforeResume)
	}
}

func TestResumer_RejectsAutoResumeDisabled(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: false,
	})
	r := newTestResumer(reg, newFakeStore(), &fakeMaster{}, &fakePush{})

	err := r.Resume(context.Background(), "sbx")
	if err == nil || err.Error() != "auto_resume not enabled for sandbox" {
		t.Fatalf("expected auto_resume disabled error, got %v", err)
	}
}

func TestResumer_RejectsUnknownSandbox(t *testing.T) {
	r := newTestResumer(registry.New(), newFakeStore(), &fakeMaster{}, &fakePush{})
	if err := r.Resume(context.Background(), "ghost"); err == nil {
		t.Fatal("resume of unknown sandbox should error")
	}
}

func TestResumer_DedupesConcurrentResumes(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	master := &fakeMaster{latency: 100 * time.Millisecond}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	const N = 20
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			errs[i] = r.Resume(context.Background(), "sbx")
		}()
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d failed: %v", i, e)
		}
	}
	if got := atomic.LoadInt32(&master.calls); got != 1 {
		t.Fatalf("concurrent resumes must coalesce into 1 RPC, got %d", got)
	}
}

func TestResumer_RollsBackOnRPCFailure(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	master := &fakeMaster{failNext: true, failError: errors.New("master 500")}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	if err := r.Resume(context.Background(), "sbx"); err == nil {
		t.Fatal("expected error from RPC failure")
	}
	if got := store.state("sbx"); got != "" {
		t.Fatalf("state must be cleared on rollback, got %q", got)
	}
}

func TestResumer_WaitsWhenPeerHoldsLock(t *testing.T) {
	// Pre-seed the state key with "resuming" — simulates a peer sidecar
	// that's already mid-flight on this sandbox. acquireResumeOwnership
	// should observe it via GetState and route to waitForRunning instead
	// of issuing a duplicate CubeMaster RPC.
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx"] = "resuming" // peer's in-flight lock, GetState-visible
	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	// Have the peer flip state to running after a moment, simulating their
	// RPC completing.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = store.SetState(context.Background(), "sbx", "running", time.Minute)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Resume(ctx, "sbx"); err != nil {
		t.Fatalf("waiter should have observed running, got %v", err)
	}
	if got := atomic.LoadInt32(&master.calls); got != 0 {
		t.Fatalf("waiter must not call master.Resume, got %d", got)
	}
}

func TestResumer_OwnsResumeWhenStateIsTerminalPaused(t *testing.T) {
	// The race we hit in production: sweeper successfully paused the
	// sandbox, leaving cube:sandbox:state:<id>="paused". A subsequent
	// request hits the gate, which calls /_sidecar_resume. The resumer
	// must NOT mistake the terminal "paused" for a peer's in-flight lock
	// — it must claim ownership and drive the resume RPC.
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-paused", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx-paused"] = "paused" // terminal marker from sweeper
	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	if err := r.Resume(context.Background(), "sbx-paused"); err != nil {
		t.Fatalf("resume should succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&master.calls); got != 1 {
		t.Fatalf("resumer must call master.Resume exactly once, got %d", got)
	}
	if got := store.state("sbx-paused"); got != "running" {
		t.Fatalf("state should be running after successful resume, got %q", got)
	}
}

func TestResumer_NoOpWhenStateIsAlreadyRunning(t *testing.T) {
	// If a peer already finished resume but the proxy's local dict still
	// says paused (e.g. push delay), we must not re-call CubeMaster.Resume
	// — just re-assert the running state into the proxy.
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-run", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx-run"] = "running"
	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	if err := r.Resume(context.Background(), "sbx-run"); err != nil {
		t.Fatalf("resume should be a no-op success, got %v", err)
	}
	if got := atomic.LoadInt32(&master.calls); got != 0 {
		t.Fatalf("master.Resume must NOT be called when state=running, got %d", got)
	}
}
