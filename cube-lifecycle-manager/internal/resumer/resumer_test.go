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

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/eventbus"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
)

// fakeStore is the resumer-side test double for the redisstream client.
type fakeStore struct {
	mu     sync.Mutex
	states map[string]string
	// allowAcquire controls whether AcquireState succeeds. When the second
	// element is non-empty, AcquireState seeds that state value into the
	// map (simulating a peer holding the lock) and returns false.
	preLocked map[string]string
	// bus, when non-nil, receives every WriteState / ClearStateNotify
	// event. This mirrors redisstream.Client.SetLocalBus in production.
	bus      *eventbus.Bus
	getCount int
	afterGet func(int)
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

func (f *fakeStore) AcquireResume(_ context.Context, sid string, _ time.Duration) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.preLocked[sid]; ok {
		f.states[sid] = v
		return v, false, nil
	}
	current, ok := f.states[sid]
	if !ok || current == lifecycle.StatePaused {
		f.states[sid] = "resuming"
		return "resuming", true, nil
	}
	return current, false, nil
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
	v, ok := f.states[sid]
	f.getCount++
	count := f.getCount
	afterGet := f.afterGet
	f.mu.Unlock()
	if afterGet != nil {
		afterGet(count)
	}
	return v, ok, nil
}

// WriteState mirrors redisstream.Client.WriteState: it performs the state
// mutation and, when a local bus is attached (see attachBus), fans out a
// StateNotify to it.
func (f *fakeStore) WriteState(_ context.Context, sid, state string, _ time.Duration) error {
	f.mu.Lock()
	f.states[sid] = state
	bus := f.bus
	f.mu.Unlock()
	if bus != nil {
		bus.Publish(sid)
	}
	return nil
}

func (f *fakeStore) ClearStateNotify(_ context.Context, sid string) error {
	f.mu.Lock()
	delete(f.states, sid)
	bus := f.bus
	f.mu.Unlock()
	if bus != nil {
		bus.Publish(sid)
	}
	return nil
}

// attachBus wires the fakeStore to a local eventbus.Bus so that state
// writes fan out to any waiter registered on that bus. Mirrors what
// redisstream.Client.SetLocalBus does in production.
func (f *fakeStore) attachBus(b *eventbus.Bus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bus = b
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
	mu       sync.Mutex
	pushed   []string
	deleted  []string
	failSet  int
	setCalls int
}

func (f *fakePush) SetState(_ context.Context, _, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.failSet > 0 {
		f.failSet--
		return errors.New("push failed")
	}
	f.pushed = append(f.pushed, state)
	return nil
}

func (f *fakePush) DeleteMeta(_ context.Context, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, sid)
	return nil
}

// pollUntil polls cond every 10ms until it returns true or timeout elapses,
// reporting whether it succeeded. Used to wait on the asynchronous proxy push
// (see doResume's `go r.pushRunningState`) without racing the goroutine.
func pollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	// AutoResume=false + state=paused: resumer would need to drive
	// CubeMaster.Resume to bring the sandbox back, but the sandbox opted
	// out. Must return an error AND release the "resuming" lock it took
	// during acquireResumeOwnership, otherwise the state key would sit at
	// "resuming" for its full TTL and make the sweeper skip decisions.
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: false,
	})
	store := newFakeStore()
	store.states["sbx"] = "paused"
	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	err := r.Resume(context.Background(), "sbx")
	if err == nil || err.Error() != "auto_resume not enabled for sandbox" {
		t.Fatalf("expected auto_resume disabled error, got %v", err)
	}
	if got := store.state("sbx"); got != "" {
		t.Fatalf("resuming lock must be released on AutoResume rejection, got %q", got)
	}
	if got := atomic.LoadInt32(&master.calls); got != 0 {
		t.Fatalf("master.Resume must not be called when AutoResume=false, got %d", got)
	}
}

// TestResumer_AutoResumeFalseAllowedWhenAlreadyRunning is the regression
// this scenario targets: the SDK called Sandbox.connect() to manually resume
// a paused sandbox that was configured with auto_resume=false. CubeMaster
// forwarded the resume to Cubelet and the state event synchroniser
// (statesync.Handle) flipped Redis state to "running". The proxy's local
// dict is momentarily still "paused", so the next dataplane request lands
// on /internal/resume. The resumer must NOT reject the request just because
// auto_resume is false — the sandbox is objectively running and we're only
// being asked to re-assert state, not to drive the resume RPC ourselves.
func TestResumer_AutoResumeFalseAllowedWhenAlreadyRunning(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: false,
	})
	store := newFakeStore()
	store.states["sbx"] = "running" // state synced from CubeMaster→CLM
	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	if err := r.Resume(context.Background(), "sbx"); err != nil {
		t.Fatalf("resume must succeed when sandbox already running, got %v", err)
	}
	if got := atomic.LoadInt32(&master.calls); got != 0 {
		t.Fatalf("master.Resume must NOT be called, got %d", got)
	}
	// Success bookkeeping still re-asserts running into the proxy dict so
	// the local cache converges even if the initial push missed the mark.
	// The push is performed asynchronously (see pushRunningState), so poll
	// until the background goroutine records it.
	if !pollUntil(2*time.Second, func() bool {
		push.mu.Lock()
		defer push.mu.Unlock()
		return len(push.pushed) == 1 && push.pushed[0] == "running"
	}) {
		push.mu.Lock()
		got := append([]string(nil), push.pushed...)
		push.mu.Unlock()
		t.Fatalf("proxy push should re-assert running, got %+v", got)
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

func TestResumer_DedupesAcrossReplicas(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx"] = lifecycle.StatePaused
	master := &fakeMaster{latency: 100 * time.Millisecond}
	first := newTestResumer(reg, store, master, &fakePush{})
	second := newTestResumer(reg, store, master, &fakePush{})

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, r := range []*Resumer{first, second} {
		go func(r *Resumer) {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errs <- r.Resume(ctx, "sbx")
		}(r)
	}
	close(start)

	if err := <-errs; err != nil {
		t.Fatalf("first replica failed: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("second replica failed: %v", err)
	}
	if got := atomic.LoadInt32(&master.calls); got != 1 {
		t.Fatalf("cross-replica resumes must coalesce into 1 RPC, got %d", got)
	}
}

func TestResumer_RollsBackOnRPCFailure(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	master := &fakeMaster{failNext: true, failError: &cubemasterclient.APIError{
		RetCode: 999,
		RetMsg:  "definitive failure",
	}}
	push := &fakePush{}
	r := newTestResumer(reg, store, master, push)

	if err := r.Resume(context.Background(), "sbx"); err == nil {
		t.Fatal("expected error from RPC failure")
	}
	if got := store.state("sbx"); got != "" {
		t.Fatalf("state must be cleared on rollback, got %q", got)
	}
}

func TestResumer_PreservesOwnershipOnUnknownRPCResult(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	master := &fakeMaster{failNext: true, failError: errors.New("connection reset")}
	r := newTestResumer(reg, store, master, &fakePush{})

	if err := r.Resume(context.Background(), "sbx"); err == nil {
		t.Fatal("expected error from transport failure")
	}
	if got := store.state("sbx"); got != "resuming" {
		t.Fatalf("unknown result must retain ownership until TTL, got %q", got)
	}
}

func TestResumer_WaitsWhenPeerHoldsLock(t *testing.T) {
	// Pre-seed the state key with "resuming" — simulates a peer CLM replica
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
	// sandbox, leaving cube:v1:shared:sandbox:lifecycle:state:<id>="paused".
	// A subsequent
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

func TestResumer_RunningStatePushRetriesAsynchronously(t *testing.T) {
	tests := []struct {
		name       string
		failSet    int
		wantCalls  int
		wantPushes int
	}{
		{name: "succeeds immediately", failSet: 0, wantCalls: 1, wantPushes: 1},
		{name: "succeeds after first retry", failSet: 1, wantCalls: 2, wantPushes: 1},
		{name: "succeeds after second retry", failSet: 2, wantCalls: 3, wantPushes: 1},
		{name: "fails after retry limit", failSet: 3, wantCalls: 3, wantPushes: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := registry.New()
			reg.Upsert(lifecycle.SandboxLifecycleMeta{
				SandboxID: "sbx-retry", InstanceType: "cubebox", AutoResume: true,
			})
			store := newFakeStore()
			push := &fakePush{failSet: tt.failSet}
			r := newTestResumer(reg, store, &fakeMaster{}, push)

			if err := r.Resume(context.Background(), "sbx-retry"); err != nil {
				t.Fatalf("resume failed: %v", err)
			}

			if !pollUntil(2*time.Second, func() bool {
				push.mu.Lock()
				defer push.mu.Unlock()
				return push.setCalls == tt.wantCalls
			}) {
				t.Fatalf("timed out waiting for %d calls", tt.wantCalls)
			}
			push.mu.Lock()
			got := len(push.pushed)
			push.mu.Unlock()
			if got != tt.wantPushes {
				t.Fatalf("got %d successful pushes, want %d", got, tt.wantPushes)
			}
		})
	}
}

// newTestResumerWithBus is the eventbus-mode counterpart of newTestResumer.
// It also attaches the bus to the fakeStore so WriteState / ClearStateNotify
// calls fan out just like they do in production.
func newTestResumerWithBus(reg *registry.Registry, store *fakeStore, master *fakeMaster, push *fakePush, bus *eventbus.Bus) *Resumer {
	store.attachBus(bus)
	return New(Options{
		Registry:     reg,
		Redis:        store,
		CubeMaster:   master,
		ProxyPush:    push,
		StateLockTTL: 30 * time.Second,
		Log:          zap.NewNop(),
		EventBus:     bus,
	})
}

func observeStoreGets(store *fakeStore) <-chan int {
	ch := make(chan int, 16)
	store.mu.Lock()
	store.afterGet = func(count int) { ch <- count }
	store.mu.Unlock()
	return ch
}

func waitForGetCount(t *testing.T, ch <-chan int, want int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case count := <-ch:
			if count >= want {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for GET count %d", want)
		}
	}
}

func TestResumer_EventBusWakesWaiterFast(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx"] = "resuming" // peer's in-flight lock

	bus := eventbus.New()
	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumerWithBus(reg, store, master, push, bus)
	gets := observeStoreGets(store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.Resume(ctx, "sbx") }()

	// GET #1 observes the peer lock; GET #2 is waitForRunning's initial
	// read after listener registration. Publish only after both occurred.
	waitForGetCount(t, gets, 2)
	_ = store.WriteState(context.Background(), "sbx", "running", time.Minute)

	if err := <-result; err != nil {
		t.Fatalf("waiter should have observed running via eventbus, got %v", err)
	}
	if got := atomic.LoadInt32(&master.calls); got != 0 {
		t.Fatalf("waiter must not call master.Resume, got %d", got)
	}
}

func TestResumer_EventBeforeWaitResolvedByGet(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx"] = "resuming"

	bus := eventbus.New()
	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumerWithBus(reg, store, master, push, bus)

	firstGetRead := make(chan struct{})
	releaseFirstGet := make(chan struct{})
	store.afterGet = func(count int) {
		if count == 1 {
			close(firstGetRead)
			<-releaseFirstGet
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.Resume(ctx, "sbx") }()

	<-firstGetRead
	_ = store.SetState(context.Background(), "sbx", "running", time.Minute)
	bus.Publish("sbx") // Wait has not registered yet; this hint is discarded.
	close(releaseFirstGet)

	if err := <-result; err != nil {
		t.Fatalf("initial GET after Wait should have observed running, got %v", err)
	}
}

func TestResumer_EventBusIgnoresStaleHint(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx"] = "resuming"

	bus := eventbus.New()
	// This event predates Wait and must not be replayed.
	bus.Publish("sbx")

	master := &fakeMaster{}
	push := &fakePush{}
	r := newTestResumerWithBus(reg, store, master, push, bus)
	gets := observeStoreGets(store)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.Resume(ctx, "sbx") }()

	waitForGetCount(t, gets, 2)
	bus.Publish("sbx") // stale/duplicate hint while Redis is still resuming
	waitForGetCount(t, gets, 3)
	select {
	case err := <-result:
		t.Fatalf("stale hint completed wait unexpectedly: %v", err)
	default:
	}

	_ = store.WriteState(context.Background(), "sbx", "running", time.Minute)
	if err := <-result; err != nil {
		t.Fatalf("stale hint must not fail the current resume: %v", err)
	}
}

// TestResumer_EventBusFallbackPolls confirms the Scenario 3 safety net:
// when the notifier drops a message, the 100ms poll still converges.
func TestResumer_EventBusFallbackPolls(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	store := newFakeStore()
	store.states["sbx"] = "resuming"

	// Bus is provided but the peer flips state via SetState (bypassing the
	// bus's Publish) — simulates a dropped notify.
	bus := eventbus.New()
	// Deliberately do NOT attach the bus to the store, so WriteState fanout
	// wouldn't fire even if used.
	master := &fakeMaster{}
	push := &fakePush{}
	r := New(Options{
		Registry:     reg,
		Redis:        store,
		CubeMaster:   master,
		ProxyPush:    push,
		StateLockTTL: 30 * time.Second,
		Log:          zap.NewNop(),
		EventBus:     bus,
	})
	gets := observeStoreGets(store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.Resume(ctx, "sbx") }()

	waitForGetCount(t, gets, 2)
	_ = store.SetState(context.Background(), "sbx", "running", time.Minute)
	waitForGetCount(t, gets, 3) // no hint: this read must come from fallback

	if err := <-result; err != nil {
		t.Fatalf("fallback poll should have observed running, got %v", err)
	}
}

func TestClassifyState(t *testing.T) {
	tests := []struct {
		name    string
		state   string
		ok      bool
		wantErr bool
		done    bool
	}{
		{name: "running", state: "running", ok: true, done: true},
		{name: "paused", state: "paused", ok: true, wantErr: true, done: true},
		{name: "killed", state: lifecycle.StateKilled, ok: true, wantErr: true, done: true},
		{name: "missing", ok: false, wantErr: true, done: true},
		{name: "resuming", state: "resuming", ok: true},
		{name: "pausing", state: "pausing", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err, done := classifyState(tt.state, tt.ok)
			if done != tt.done || (err != nil) != tt.wantErr {
				t.Fatalf("classifyState(%q, %v) = (%v, %v)", tt.state, tt.ok, err, done)
			}
		})
	}
}
