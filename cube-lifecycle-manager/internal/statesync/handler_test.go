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
	mu         sync.Mutex
	states     map[string]string
	getErr     error
	setErr     error
	setCalls   int
	casUpdated *bool
	// beforeCAS, when set, runs under the lock just before the CAS compare.
	// Tests use it to model a peer writing between Handle's read and CAS.
	beforeCAS func(states map[string]string)
	// getErrAfterCAS makes every GetState after the first CAS fail.
	getErrAfterCAS error
	casSeen        bool
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
	if f.casSeen && f.getErrAfterCAS != nil {
		return "", false, f.getErrAfterCAS
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

// WriteState is the notify-emitting equivalent of SetState. statesync
// tests don't inspect the StateNotify payload; forward to SetState so
// setCalls / setErr semantics stay unchanged.
func (f *fakeRedis) WriteState(ctx context.Context, sid, state string, ttl time.Duration) error {
	return f.SetState(ctx, sid, state, ttl)
}

func (f *fakeRedis) WriteStateCAS(
	_ context.Context, sid, expected, state string, _ time.Duration,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.casSeen = true
	if f.setErr != nil {
		return false, f.setErr
	}
	if f.beforeCAS != nil {
		f.beforeCAS(f.states)
	}
	if f.casUpdated != nil {
		return *f.casUpdated, nil
	}
	if f.states[sid] != expected {
		return false, nil
	}
	f.states[sid] = state
	return true, nil
}

type fakeProxy struct {
	mu    sync.Mutex
	calls []string // "sid=state"
	err   error
}

type fakeLeader struct{ leader bool }

func (f fakeLeader) IsLeader() bool { return f.leader }
func (f fakeLeader) Enabled() bool  { return true }

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

func TestHandle_SameStateStillConvergesProxy(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.states["sbx-1"] = "paused"

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StatePaused, lifecycle.ActorCubeMaster))

	if r.setCalls != 1 {
		t.Fatalf("same-state event must still CAS: %d", r.setCalls)
	}
	if got := p.recorded(); len(got) != 1 {
		t.Fatalf("same-state event should converge proxy once: %+v", got)
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

// A peer replica owns "resuming" and will write Redis running itself; the
// leader must still record running so a stale paused RuntimeState does not
// make the sweeper skip the sandbox forever after the key expires.
func TestHandle_ResumingTransitionRecordsMatchingOutcome(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = "resuming"
	reg.SetRuntimeState("sbx-1", lifecycle.StatePaused)

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "resuming" {
		t.Fatalf("state must remain resuming, got %q", got)
	}
	if r.setCalls != 0 {
		t.Fatalf("must not write Redis during transition: %d", r.setCalls)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not fire during transition: %+v", got)
	}
	entry := reg.Get("sbx-1")
	if entry.RuntimeState != lifecycle.StateRunning {
		t.Fatalf("RuntimeState = %q, want running", entry.RuntimeState)
	}
	if entry.LastActiveMs != 1_700_000_500_000 {
		t.Fatalf("LastActiveMs not bumped: %d", entry.LastActiveMs)
	}
}

func TestHandle_PausingTransitionRecordsMatchingOutcome(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = "pausing"
	reg.SetRuntimeState("sbx-1", lifecycle.StateRunning)

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StatePaused, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "pausing" {
		t.Fatalf("state must remain pausing, got %q", got)
	}
	if r.setCalls != 0 {
		t.Fatalf("must not write Redis during transition: %d", r.setCalls)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not fire during transition: %+v", got)
	}
	if got := reg.Get("sbx-1").RuntimeState; got != lifecycle.StatePaused {
		t.Fatalf("RuntimeState = %q, want paused", got)
	}
}

// Redis read failures leave no verdict to contradict the event. Dropping it
// would strand the leader's RuntimeState exactly like a skipped event.
func TestHandle_GetStateErrorRecordsLocally(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	reg.SetRuntimeState("sbx-1", lifecycle.StatePaused)
	r.getErr = errors.New("redis get boom")

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if r.setCalls != 0 || len(p.recorded()) != 0 {
		t.Fatalf("no external writes expected: set=%d push=%+v", r.setCalls, p.recorded())
	}
	entry := reg.Get("sbx-1")
	if entry.RuntimeState != lifecycle.StateRunning || entry.LastActiveMs != 1_700_000_500_000 {
		t.Fatalf("event not recorded locally: %+v", entry)
	}
}

func TestHandle_CASErrorRecordsLocally(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = lifecycle.StateRunning
	reg.SetRuntimeState("sbx-1", lifecycle.StateRunning)
	r.setErr = errors.New("redis set boom")

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StatePaused, lifecycle.ActorCubeMaster))

	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not fire when CAS fails: %+v", got)
	}
	if got := reg.Get("sbx-1").RuntimeState; got != lifecycle.StatePaused {
		t.Fatalf("RuntimeState = %q, want paused", got)
	}
}

// A peer took "resuming" between Handle's read and CAS; the event is that
// resume's outcome, so the leader must record it like the marker path does.
func TestHandle_CASLostToMatchingMarkerRecordsLocally(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = lifecycle.StatePaused
	reg.SetRuntimeState("sbx-1", lifecycle.StatePaused)
	r.beforeCAS = func(states map[string]string) { states["sbx-1"] = "resuming" }

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != "resuming" {
		t.Fatalf("peer marker overwritten: %q", got)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not fire on lost CAS: %+v", got)
	}
	if got := reg.Get("sbx-1").RuntimeState; got != lifecycle.StateRunning {
		t.Fatalf("RuntimeState = %q, want running", got)
	}
}

func TestHandle_CASLostToDisagreeingStateKeepsRegistry(t *testing.T) {
	// initial is what Handle reads first; beforeCAS then changes it so the
	// CAS is lost and Handle has to judge the newer value.
	cases := []struct {
		name    string
		initial string
		latest  *string // nil = key removed
		getErr  bool
	}{
		{name: "mismatched marker", initial: lifecycle.StatePaused, latest: strPtr("pausing")},
		{name: "other terminal", initial: "", latest: strPtr(lifecycle.StatePaused)},
		{name: "key removed", initial: lifecycle.StatePaused},
		{name: "re-read fails", initial: lifecycle.StatePaused, latest: strPtr("resuming"), getErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, r, _, reg := buildDeps(t)
			if tc.initial != "" {
				r.states["sbx-1"] = tc.initial
			}
			reg.SetRuntimeState("sbx-1", lifecycle.StatePaused)
			r.beforeCAS = func(states map[string]string) {
				if tc.latest == nil {
					delete(states, "sbx-1")
					return
				}
				states["sbx-1"] = *tc.latest
			}
			if tc.getErr {
				r.getErrAfterCAS = errors.New("redis get boom")
			}

			Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

			entry := reg.Get("sbx-1")
			if entry.RuntimeState != lifecycle.StatePaused || entry.LastActiveMs != 0 {
				t.Fatalf("disagreeing newer state changed registry: %+v", entry)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func TestHandle_TransitionIgnoresMismatchedOutcome(t *testing.T) {
	cases := []struct{ marker, event string }{
		{"pausing", lifecycle.StateRunning},
		{"resuming", lifecycle.StatePaused},
	}
	for _, tc := range cases {
		t.Run(tc.marker, func(t *testing.T) {
			d, r, _, reg := buildDeps(t)
			r.states["sbx-1"] = tc.marker
			reg.SetRuntimeState("sbx-1", "prior")

			Handle(context.Background(), d, stateEvent("sbx-1", tc.event, lifecycle.ActorCubeMaster))

			entry := reg.Get("sbx-1")
			if entry.RuntimeState != "prior" || entry.LastActiveMs != 0 {
				t.Fatalf("mismatched event changed registry: %+v", entry)
			}
		})
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

func TestHandle_RestartingWritesRedisNotProxy(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = lifecycle.StateRunning
	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRestarting, lifecycle.ActorCubeMaster))
	if got := r.states["sbx-1"]; got != lifecycle.StateRestarting {
		t.Fatalf("redis state %q", got)
	}
	if len(p.recorded()) != 0 {
		t.Fatalf("proxy must not learn restarting: %+v", p.recorded())
	}
	if got := reg.Get("sbx-1").RuntimeState; got != lifecycle.StateRestarting {
		t.Fatalf("runtime %q", got)
	}

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateGaveUp, lifecycle.ActorCubeMaster))
	if got := r.states["sbx-1"]; got != lifecycle.StateRunning {
		t.Fatalf("gave up redis state %q", got)
	}
	if got := reg.Get("sbx-1").RuntimeState; got != lifecycle.StateRunning {
		t.Fatalf("gave up runtime %q", got)
	}
}

func TestHandle_CubeletRunningDoesNotUndoPause(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = lifecycle.StatePaused
	ev := stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster)
	ev.State.Source = "cubelet"
	Handle(context.Background(), d, ev)
	if got := r.states["sbx-1"]; got != lifecycle.StatePaused {
		t.Fatalf("redis state %q", got)
	}
	if len(p.recorded()) != 0 {
		t.Fatalf("proxy push %+v", p.recorded())
	}
	if got := reg.Get("sbx-1").RuntimeState; got != "" {
		t.Fatalf("runtime %q", got)
	}
	if got := reg.Get("sbx-1").LastActiveMs; got != 0 {
		t.Fatalf("last active %d", got)
	}

	r.states["sbx-1"] = lifecycle.StateBackOff
	Handle(context.Background(), d, ev)
	if got := r.states["sbx-1"]; got != lifecycle.StateRunning {
		t.Fatalf("redis after backoff %q", got)
	}
	if len(p.recorded()) != 0 {
		t.Fatalf("proxy learned running %+v", p.recorded())
	}
	if got := reg.Get("sbx-1").LastActiveMs; got != 0 {
		t.Fatalf("idle timer refreshed %d", got)
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

func TestHandle_SetStateErrorDoesNotPushProxy(t *testing.T) {
	d, r, p, _ := buildDeps(t)
	r.states["sbx-1"] = "paused"
	r.setErr = errors.New("redis boom")

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("proxy push must not fire when Redis CAS fails: %+v", got)
	}
}

func TestHandle_StandbyRetainsWarmStateWithoutExternalWrites(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	d.Leader = fakeLeader{leader: false}
	r.states["sbx-1"] = lifecycle.StatePaused

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StateRunning, lifecycle.ActorCubeMaster))

	if got := r.states["sbx-1"]; got != lifecycle.StatePaused {
		t.Fatalf("standby changed Redis state to %q", got)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("standby pushed proxy state: %+v", got)
	}
	if got := reg.Get("sbx-1").RuntimeState; got != lifecycle.StateRunning {
		t.Fatalf("warm RuntimeState = %q, want running", got)
	}
}

func TestHandle_RejectedCASDoesNotRollBackWarmState(t *testing.T) {
	d, r, p, reg := buildDeps(t)
	r.states["sbx-1"] = lifecycle.StateRunning
	reg.SetRuntimeState("sbx-1", lifecycle.StateRunning)
	rejected := false
	r.casUpdated = &rejected

	Handle(context.Background(), d, stateEvent("sbx-1", lifecycle.StatePaused, lifecycle.ActorCubeMaster))

	if got := reg.Get("sbx-1").RuntimeState; got != lifecycle.StateRunning {
		t.Fatalf("rejected old event rolled RuntimeState back to %q", got)
	}
	if got := p.recorded(); len(got) != 0 {
		t.Fatalf("rejected old event pushed proxy state: %+v", got)
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
