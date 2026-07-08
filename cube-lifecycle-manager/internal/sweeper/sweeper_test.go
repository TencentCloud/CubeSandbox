// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sweeper

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
)

// fakeStore implements stateStore. It uses simple maps; lock contention isn't
// the focus of these tests, atomicity of state transitions is.
type fakeStore struct {
	mu     sync.Mutex
	states map[string]string

	failAcquire bool
	acquireBy   func(sid, state string) bool // when set, controls AcquireState success
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: make(map[string]string)}
}

func (f *fakeStore) AcquireState(_ context.Context, sid, state string, _ time.Duration) (bool, error) {
	if f.failAcquire {
		return false, errors.New("redis down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireBy != nil && !f.acquireBy(sid, state) {
		return false, nil
	}
	if _, ok := f.states[sid]; ok {
		return false, nil // already held
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

func (f *fakeStore) GetState(_ context.Context, sid string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.states[sid]
	return v, ok, nil
}

func (f *fakeStore) ClearState(_ context.Context, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.states, sid)
	return nil
}

func (f *fakeStore) state(sid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[sid]
}

// fakeMaster captures every Pause / Kill call and lets tests inject errors.
// The same struct serves both paths so a single sweeper can drive either; the
// pause-* tests assert on `calls`, the kill-* tests on `killCalls`.
type fakeMaster struct {
	mu        sync.Mutex
	calls     []string
	failNext  bool
	failError error

	killCalls    []string
	killReasons  []string
	failNextKill bool
	failKillErr  error
}

func (f *fakeMaster) Pause(_ context.Context, sid, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sid)
	if f.failNext {
		f.failNext = false
		return f.failError
	}
	return nil
}

func (f *fakeMaster) Kill(_ context.Context, sid, _ string, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killCalls = append(f.killCalls, sid)
	f.killReasons = append(f.killReasons, reason)
	if f.failNextKill {
		f.failNextKill = false
		return f.failKillErr
	}
	return nil
}

// fakePush records every state pushed to CubeProxy. It also tracks
// DeleteMeta calls so tests can assert that the not-found eviction path
// fires when expected.
type fakePush struct {
	mu      sync.Mutex
	pushed  map[string][]string // sandbox_id -> ordered list of states
	deleted []string            // sandbox_ids passed to DeleteMeta
}

func newFakePush() *fakePush {
	return &fakePush{pushed: make(map[string][]string)}
}

func (f *fakePush) SetState(_ context.Context, sid, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed[sid] = append(f.pushed[sid], state)
	return nil
}

func (f *fakePush) DeleteMeta(_ context.Context, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, sid)
	return nil
}

func (f *fakePush) states(sid string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.pushed[sid]))
	copy(out, f.pushed[sid])
	return out
}

func (f *fakePush) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleted))
	copy(out, f.deleted)
	return out
}

// build a sweeper wired to fakes. `at` is the wall clock the sweeper sees.
func newTestSweeper(reg *registry.Registry, store *fakeStore, master *fakeMaster, push *fakePush, at time.Time) *Sweeper {
	// BootstrapWarmup=0 means "act on every entry immediately" — tests
	// drive sweepOnce manually and don't need the warmup gate.
	return New(Options{
		Registry:           reg,
		Redis:              store,
		CubeMaster:         master,
		ProxyPush:          push,
		DefaultIdleTimeout: 5 * time.Minute,
		BootstrapWarmup:    0,
		StateLockTTL:       30 * time.Second,
		Interval:           time.Second,
		Now:                func() time.Time { return at },
		Log:                zap.NewNop(),
	})
}

// seedEntry inserts a registry entry. Unlike the previous grace-period
// design, the sweeper now bases its idle decision on max(LastActiveMs,
// CreatedAt), so test cases just need to set those fields appropriately.
func seedEntry(t *testing.T, r *registry.Registry, meta lifecycle.SandboxLifecycleMeta, lastActiveMs int64) {
	t.Helper()
	r.Upsert(meta)
	if lastActiveMs > 0 {
		if !r.MergeLastActive(meta.SandboxID, lastActiveMs) {
			t.Fatalf("seed: MergeLastActive(%s, %d) didn't advance", meta.SandboxID, lastActiveMs)
		}
	}
}

func TestSweeper_PausesIdleSandbox(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{}
	push := newFakePush()

	// last_active was 10 minutes ago, timeout is 5 minutes → past due.
	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-1", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(300),
	}, now.Add(-10*time.Minute).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.calls) != 1 || master.calls[0] != "sbx-1" {
		t.Fatalf("expected single Pause call for sbx-1, got %v", master.calls)
	}
	if got := store.state("sbx-1"); got != "paused" {
		t.Fatalf("expected redis state=paused, got %q", got)
	}
	pushed := push.states("sbx-1")
	if len(pushed) < 2 || pushed[0] != "pausing" || pushed[len(pushed)-1] != "paused" {
		t.Fatalf("expected push pausing→paused, got %v", pushed)
	}
	triggered, failed := s.Stats()
	if triggered != 1 || failed != 0 {
		t.Fatalf("stats: triggered=%d failed=%d", triggered, failed)
	}
}

func TestSweeper_SkipsSandboxWithoutAutoPause(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{}
	push := newFakePush()
	now := time.Now()

	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-2", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-1*time.Hour).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.calls) != 0 {
		t.Fatalf("Pause must NOT fire when AutoPause=false: %v", master.calls)
	}
	if len(master.killCalls) != 1 || master.killCalls[0] != "sbx-2" {
		t.Fatalf("Kill must fire when AutoPause=false: %v", master.killCalls)
	}
	if len(master.killReasons) != 1 || master.killReasons[0] != "timeout" {
		t.Fatalf("sweeper must tag idle kills with reason=timeout: %v", master.killReasons)
	}
	if reg.Get("sbx-2") != nil {
		t.Fatal("registry entry should have been evicted after kill")
	}
	if got := push.deletedIDs(); len(got) != 1 || got[0] != "sbx-2" {
		t.Fatalf("expected DeleteMeta(sbx-2), got %v", got)
	}
	if got := store.state("sbx-2"); got != "killed" {
		t.Fatalf("expected redis state=killed, got %q", got)
	}
	pushed := push.states("sbx-2")
	if len(pushed) == 0 || pushed[0] != "killing" {
		t.Fatalf("expected first push state=killing, got %v", pushed)
	}
	triggered, failed := s.KillStats()
	if triggered != 1 || failed != 0 {
		t.Fatalf("kill stats: triggered=%d failed=%d", triggered, failed)
	}
}

func TestSweeper_KillRollsBackOnFailure(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{failNextKill: true, failKillErr: errors.New("master 500")}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-killfail", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-10*time.Minute).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if got := store.state("sbx-killfail"); got != "" {
		t.Fatalf("redis state should be cleared after rollback, got %q", got)
	}
	pushed := push.states("sbx-killfail")
	if len(pushed) == 0 || pushed[len(pushed)-1] != "running" {
		t.Fatalf("rollback should leave proxy at running, got %v", pushed)
	}
	if reg.Get("sbx-killfail") == nil {
		t.Fatal("registry entry should NOT be evicted on kill failure")
	}
	_, failed := s.KillStats()
	if failed != 1 {
		t.Fatalf("expected kill failed=1, got %d", failed)
	}
}

func TestSweeper_KillNotFoundEvictsRegistry(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{
		failNextKill: true,
		failKillErr: &cubemasterclient.APIError{
			RetCode: cubemasterclient.RetCodeInvalidParamFormat,
			RetMsg:  "key not found",
		},
	}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-killgone", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-10*time.Minute).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if reg.Get("sbx-killgone") != nil {
		t.Fatal("registry entry should be evicted on not-found")
	}
	if got := push.deletedIDs(); len(got) != 1 || got[0] != "sbx-killgone" {
		t.Fatalf("expected DeleteMeta(sbx-killgone), got %v", got)
	}
	triggered, failed := s.KillStats()
	if triggered != 1 || failed != 0 {
		t.Fatalf("not-found is success: triggered=%d failed=%d", triggered, failed)
	}
}

func TestSweeper_KillSkipsAlreadyKillingSandbox(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	store.states["sbx-killmid"] = "killing"

	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-killmid", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-1*time.Hour).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.killCalls) != 0 {
		t.Fatalf("sweeper must NOT call Kill when peer is mid-flight: %v", master.killCalls)
	}
}

func TestSweeper_BootstrapWarmupSkipsBootstrapEntries(t *testing.T) {
	// Verifies the bootstrap-warmup gate: while the sidecar is still in
	// its warmup window, sandboxes whose FirstSeenAt is at-or-before the
	// sweeper's StartedAt (i.e. loaded from HGETALL) are skipped, even if
	// their CreatedAt is hours old. After the warmup elapses, the sweeper
	// must act on them.
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{}
	push := newFakePush()

	startedAt := time.Now()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-bootstrap", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
		CreatedAt: startedAt.Add(-1 * time.Hour).UnixMilli(), // ancient
	})
	// Pin FirstSeenAt to startedAt — that's exactly what main.go's
	// bootstrap() does for HGETALL entries.
	reg.SetFirstSeenAt("sbx-bootstrap", startedAt)

	mkSweeper := func(now time.Time) *Sweeper {
		return New(Options{
			Registry:           reg,
			Redis:              store,
			CubeMaster:         master,
			ProxyPush:          push,
			DefaultIdleTimeout: 5 * time.Minute,
			BootstrapWarmup:    30 * time.Second,
			StateLockTTL:       30 * time.Second,
			Interval:           time.Second,
			StartedAt:          startedAt,
			Now:                func() time.Time { return now },
			Log:                zap.NewNop(),
		})
	}

	// During warmup → skipped.
	mkSweeper(startedAt).sweepOnce(context.Background())
	if len(master.calls) != 0 {
		t.Fatalf("Pause must NOT fire on bootstrap entry during warmup: %v", master.calls)
	}

	// 45s later, well past the 30s warmup → sweeper acts.
	mkSweeper(startedAt.Add(45 * time.Second)).sweepOnce(context.Background())
	if len(master.calls) != 1 {
		t.Fatalf("after warmup, sweeper should pause the idle bootstrap entry: %v", master.calls)
	}
}

func TestSweeper_RollsBackOnPauseFailure(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{failNext: true, failError: errors.New("master 500")}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-4", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-10*time.Minute).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if got := store.state("sbx-4"); got != "" {
		t.Fatalf("redis state should be cleared after rollback, got %q", got)
	}
	pushed := push.states("sbx-4")
	if len(pushed) == 0 || pushed[len(pushed)-1] != "running" {
		t.Fatalf("rollback should leave proxy at running, got %v", pushed)
	}
	_, failed := s.Stats()
	if failed != 1 {
		t.Fatalf("expected failed=1, got %d", failed)
	}
}

func TestSweeper_SkipsWhenLockHeldElsewhere(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	// Pre-seed the state map → AcquireState returns false (someone else owns).
	store.states["sbx-5"] = "pausing"

	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-5", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(1),
	}, now.Add(-1*time.Hour).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.calls) != 0 {
		t.Fatalf("Pause must not fire when lock is held: %v", master.calls)
	}
	triggered, _ := s.Stats()
	if triggered != 0 {
		t.Fatalf("triggered should be 0, got %d", triggered)
	}
}

func TestSweeper_FallsBackToCreatedAtWhenNoActivityRecorded(t *testing.T) {
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()
	// No MergeLastActive call: LastActiveMs stays 0; CreatedAt is recent so
	// the sandbox should NOT be paused.
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-recent", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(300),
		CreatedAt: now.Add(-1 * time.Minute).UnixMilli(),
	})
	// Pretend the entry was added long enough ago to clear the grace window.
	at := now.Add(2 * time.Minute)

	s := newTestSweeper(reg, store, master, push, at)
	s.sweepOnce(context.Background())

	if len(master.calls) != 0 {
		t.Fatalf("recent CreatedAt should keep sandbox alive: %v", master.calls)
	}

	// Now CreatedAt is 10 minutes old → past timeout → expect pause.
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-old", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(300),
		CreatedAt: now.Add(-10 * time.Minute).UnixMilli(),
	})
	at2 := now.Add(2 * time.Minute)
	s2 := newTestSweeper(reg, store, master, push, at2)
	s2.sweepOnce(context.Background())

	if len(master.calls) != 1 || master.calls[0] != "sbx-old" {
		t.Fatalf("old CreatedAt must trigger pause: %v", master.calls)
	}
}

func TestSweeper_NotFoundEvictsRegistryEntry(t *testing.T) {
	// CubeMaster returns "key not found" (RetCodeInvalidParamFormat) when the
	// sandbox has been deleted out from under us. The sweeper must drop the
	// entry from the registry, push a delete to CubeProxy, and NOT record
	// the failure as a retry-able error.
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{
		failNext: true,
		failError: &cubemasterclient.APIError{
			RetCode: cubemasterclient.RetCodeInvalidParamFormat,
			RetMsg:  "key not found",
		},
	}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-gone", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-10*time.Minute).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if reg.Get("sbx-gone") != nil {
		t.Fatal("registry entry should have been evicted")
	}
	if got := push.deletedIDs(); len(got) != 1 || got[0] != "sbx-gone" {
		t.Fatalf("expected DeleteMeta(sbx-gone), got %v", got)
	}
	if got := store.state("sbx-gone"); got != "" {
		t.Fatalf("redis state should be cleared, got %q", got)
	}
	_, failed := s.Stats()
	if failed != 0 {
		t.Fatalf("not-found is not a real failure; failed counter should be 0, got %d", failed)
	}
}

func TestSweeper_SkipsAlreadyPausedSandbox(t *testing.T) {
	// Regression: when a previous sweep successfully paused the sandbox,
	// Redis carries `state:<id>="paused"`. Subsequent sweeps must NOT
	// re-attempt — the sandbox is already at the desired terminal state
	// and there is no resource to reclaim. Without this guard the sweeper
	// hammers SETNX every Interval and issues a pointless RPC against
	// CubeMaster every StateLockTTL seconds.
	reg := registry.New()
	store := newFakeStore()
	store.states["sbx-zzz"] = "paused" // terminal marker from previous sweep

	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-zzz", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-1*time.Hour).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.calls) != 0 {
		t.Fatalf("sweeper must NOT call Pause when state is already paused: %v", master.calls)
	}
	triggered, _ := s.Stats()
	if triggered != 0 {
		t.Fatalf("triggered should be 0, got %d", triggered)
	}
}

func TestSweeper_SkipsPausingSandbox(t *testing.T) {
	// Same idea but for "pausing" — peer is mid-flight, don't pile on.
	reg := registry.New()
	store := newFakeStore()
	store.states["sbx-mid"] = "pausing"

	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-mid", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-1*time.Hour).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.calls) != 0 {
		t.Fatalf("sweeper must NOT call Pause when peer is mid-flight: %v", master.calls)
	}
}

func TestSweeper_NeverTimeoutIsSkipped(t *testing.T) {
	// Never-timeout entries must not be reclaimed. See docs/guide/lifecycle.md.
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-never", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(-1),
	}, now.Add(-24*time.Hour).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.calls) != 0 || len(master.killCalls) != 0 {
		t.Fatalf("never-timeout sandbox must not be paused/killed: pause=%v kill=%v",
			master.calls, master.killCalls)
	}
	if reg.Get("sbx-never") == nil {
		t.Fatal("never-timeout entry must remain in the registry")
	}
}

func TestSweeper_ZeroTimeoutReclaimsImmediately(t *testing.T) {
	// Zero timeout reclaims on the first sweep. See docs/guide/lifecycle.md.
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()
	// Freshly created (CreatedAt = now) and never active — with timeout 0 the
	// idle check trips immediately.
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-immediate", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(0),
		CreatedAt: now.UnixMilli(),
	})

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.killCalls) != 1 || master.killCalls[0] != "sbx-immediate" {
		t.Fatalf("zero-timeout sandbox must be killed immediately: %v", master.killCalls)
	}
}

func TestSweeper_NilTimeoutFallsBackToDefaultIdle(t *testing.T) {
	// Nil timeout falls back to DefaultIdleTimeout. See docs/guide/lifecycle.md.
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{}
	push := newFakePush()

	now := time.Now()

	// Idle for 1 minute (< 5m default) → must survive.
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-legacy-recent", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: nil,
		CreatedAt: now.Add(-1 * time.Minute).UnixMilli(),
	})
	// Idle for 10 minutes (> 5m default) → must be reclaimed.
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-legacy-old", InstanceType: "cubebox",
		AutoPause: false, TimeoutSeconds: nil,
		CreatedAt: now.Add(-10 * time.Minute).UnixMilli(),
	})

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if len(master.killCalls) != 1 || master.killCalls[0] != "sbx-legacy-old" {
		t.Fatalf("nil timeout must use DefaultIdleTimeout: expected only sbx-legacy-old killed, got %v",
			master.killCalls)
	}
}

func TestSweeper_AlreadyPausedReconcilesAsSuccess(t *testing.T) {
	// CubeMaster returns "sandbox is already paused" (RetCodeTaskStateInvalid)
	// when a peer sidecar already paused this sandbox. The sweeper should NOT
	// roll back: it should write `paused` state to Redis + push to CubeProxy
	// just like a fresh successful pause, so all callers converge on the
	// same view.
	reg := registry.New()
	store := newFakeStore()
	master := &fakeMaster{
		failNext: true,
		failError: &cubemasterclient.APIError{
			RetCode: cubemasterclient.RetCodeTaskStateInvalid,
			RetMsg:  "sandbox is already paused",
		},
	}
	push := newFakePush()

	now := time.Now()
	seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-already", InstanceType: "cubebox",
		AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}, now.Add(-10*time.Minute).UnixMilli())

	s := newTestSweeper(reg, store, master, push, now)
	s.sweepOnce(context.Background())

	if got := store.state("sbx-already"); got != "paused" {
		t.Fatalf("expected redis state=paused (reconciliation), got %q", got)
	}
	pushed := push.states("sbx-already")
	if len(pushed) == 0 || pushed[len(pushed)-1] != "paused" {
		t.Fatalf("expected last push state=paused, got %v", pushed)
	}
	triggered, failed := s.Stats()
	if triggered != 1 || failed != 0 {
		t.Fatalf("already-paused should count as triggered, not failed: triggered=%d failed=%d",
			triggered, failed)
	}
}
