// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package statesync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
)

type fakeRedis struct {
	mu       sync.Mutex
	states   map[string]string
	getErr   error
	setErr   error
	setCalls int
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{states: map[string]string{}}
}

func (f *fakeRedis) GetState(_ context.Context, sid string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", false, f.getErr
	}
	v, ok := f.states[sid]
	return v, ok, nil
}

func (f *fakeRedis) SetState(_ context.Context, sid, state string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	f.states[sid] = state
	return nil
}

type fakeProxy struct {
	mu    sync.Mutex
	calls []string // "sid=state"
	err   error
}

func (p *fakeProxy) SetState(_ context.Context, sid, state string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, sid+"="+state)
	return p.err
}

func (p *fakeProxy) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	copy(out, p.calls)
	return out
}

func stateEvent(sid, state, actor string) redisstream.Event {
	return redisstream.Event{
		StreamID:  "1-0",
		Op:        lifecycle.OpState,
		SandboxID: sid,
		State: &lifecycle.StatePayload{
			State: state,
			Actor: actor,
		},
		Timestamp: 1700000000000,
	}
}

func buildDeps(t *testing.T) (Deps, *fakeRedis, *fakeProxy, *registry.Registry) {
	t.Helper()
	r := newFakeRedis()
	p := &fakeProxy{}
	reg := registry.New()
	// Register a sandbox so Registry.Get returns non-nil.
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx-1"})
	d := Deps{
		Registry:  reg,
		Redis:     r,
		ProxyPush: p,
		TTL:       60 * time.Second,
		Log:       zap.NewNop(),
		Now:       func() time.Time { return time.UnixMilli(1_700_000_500_000) },
	}
	return d, r, p, reg
}

func TestHandle_PausedToRunning(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = "paused"

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "running" {
		t.Fatalf("redis state = %q, want running", got)
	}
	if got := p.recorded(); len(got) != 1 || got[0] != "sbx-1=running" {
		t.Fatalf("proxy push wrong: %+v", got)
	}
	entry := reg.Get("sbx-1")
	if entry == nil || entry.LastActiveMs != 1_700_000_500_000 {
		t.Fatalf("LastActiveMs not bumped: %+v", entry)
	}
}

func TestHandle_RunningToPaused(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = "running"

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StatePaused, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "paused" {
		t.Fatalf("redis state = %q, want paused", got)
	}
	if got := p.recorded(); len(got) != 1 || got[0] != "sbx-1=paused" {
		t.Fatalf("proxy push wrong: %+v", got)
	}
	entry := reg.Get("sbx-1")
	if entry.LastActiveMs != 0 {
		t.Fatalf("LastActiveMs must not change on paused event: %d", entry.LastActiveMs)
	}
}

func TestHandle_IdempotentSameState(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.states["sbx-1"] = "paused"

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StatePaused, lifecycle.ActorCubeMaster))

	if r.setCalls != 0 {
		t.Fatalf("SetState must not be called on no-op: %d", r.setCalls)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not be called on no-op: %+v", got)
	}
}

func TestHandle_SkipDuringPausingTransition(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.states["sbx-1"] = "pausing"

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "pausing" {
		t.Fatalf("state must remain pausing, got %q", got)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not fire during transition: %+v", got)
	}
}

func TestHandle_SkipDuringResumingTransition(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.states["sbx-1"] = "resuming"

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StatePaused, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "resuming" {
		t.Fatalf("state must remain resuming, got %q", got)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not fire during transition: %+v", got)
	}
}

func TestHandle_UnknownSandboxSkipped(t *testing.T) {
	d, r, p, _ := buildDeps(t)

	Handle(context.Background(), d, stateEvent("sbx-does-not-exist", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if r.setCalls != 0 {
		t.Fatalf("SetState called for unknown sandbox: %d", r.setCalls)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push called for unknown sandbox: %+v", got)
	}
}

func TestHandle_NilStatePayload(t *testing.T) {
	d, r, p, _ := buildDeps(t)

	ev := redisstream.Event{
		Op:        lifecycle.OpState,
		SandboxID: "sbx-1",
		// State intentionally nil
	}
	Handle(context.Background(), d, ev)

	if r.setCalls != 0 || len(p.recorded()) != 0 {
		t.Fatal("nil state payload must be a no-op")
	}
}

func TestHandle_InvalidState(t *testing.T) {
	cases := []string{"pausing", "resuming", "", "UNKNOWN"}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			d, r, p, _ := buildDeps(t)
			Handle(context.Background(), d, stateEvent("sbx-1", bad, lifecycle.ActorCubeMaster))
			if r.setCalls != 0 || len(p.recorded()) != 0 {
				t.Fatalf("invalid state %q must be no-op", bad)
			}
		})
	}
}

func TestHandle_SetStateErrorStillPushesProxy(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.states["sbx-1"] = "paused"
	r.setErr = errors.New("redis boom")

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := p.recorded(); len(got) != 1 {
		t.Fatalf("proxy push must still fire when Redis SetState fails: %+v", got)
	}
}

func TestHandle_ProxyPushErrorSwallowed(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.states["sbx-1"] = "paused"
	p.err = errors.New("proxy boom")

	// Must not panic; must still bump LastActiveMs since Redis SetState succeeded.
	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "running" {
		t.Fatalf("redis state should have flipped, got %q", got)
	}
}

func TestHandle_GetStateErrorEarlyReturn(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.getErr = errors.New("redis get boom")

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if r.setCalls != 0 {
		t.Fatal("must not attempt SetState when GetState fails")
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("must not push proxy when GetState fails: %+v", got)
	}
}

func TestHandle_EmptySandboxID(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	Handle(context.Background(), d, stateEvent("", lifecycle.StateRunning, lifecycle.ActorCubeMaster))
	if r.setCalls != 0 || len(p.recorded()) != 0 {
		t.Fatal("empty sandbox id must be no-op")
	}
}
