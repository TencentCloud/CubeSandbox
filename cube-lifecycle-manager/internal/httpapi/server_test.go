// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/resumer"
)

// ------ resumer test doubles (re-used pattern from resumer_test.go) -------

type fakeStore struct {
	states map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{states: map[string]string{}} }

// AcquireTransition mirrors redisstream.Client.AcquireTransition: an atomic
// CAS that writes the owner-tagged transition marker, succeeding when the
// key is missing or holds one of fromStates.
func (f *fakeStore) AcquireTransition(_ context.Context, sid, transition, owner string, _ time.Duration, fromStates ...string) (bool, error) {
	cur, ok := f.states[sid]
	if !ok {
		f.states[sid] = lifecycle.TransitionValue(transition, owner)
		return true, nil
	}
	for _, from := range fromStates {
		if cur == from {
			f.states[sid] = lifecycle.TransitionValue(transition, owner)
			return true, nil
		}
	}
	return false, nil
}

// CommitTransition mirrors redisstream.Client.CommitTransition: the terminal
// state is written only while the caller still owns the transition lock.
func (f *fakeStore) CommitTransition(_ context.Context, sid, transition, owner, newState string, _ time.Duration) (bool, error) {
	if f.states[sid] != lifecycle.TransitionValue(transition, owner) {
		return false, nil
	}
	f.states[sid] = newState
	return true, nil
}

// ReleaseTransition mirrors redisstream.Client.ReleaseTransition: the key is
// deleted only while the caller still owns the transition lock.
func (f *fakeStore) ReleaseTransition(_ context.Context, sid, transition, owner string) (bool, error) {
	if f.states[sid] != lifecycle.TransitionValue(transition, owner) {
		return false, nil
	}
	delete(f.states, sid)
	return true, nil
}

func (f *fakeStore) GetState(_ context.Context, sid string) (string, bool, error) {
	v, ok := f.states[sid]
	return v, ok, nil
}

// WriteState is the notify-emitting unconditional write used on the
// already-running re-assert path. The httpapi tests only assert on state
// values, not notify payloads.
func (f *fakeStore) WriteState(_ context.Context, sid, state string, _ time.Duration) error {
	f.states[sid] = state
	return nil
}

type fakeMaster struct {
	calls    int32
	failNext bool
}

func (f *fakeMaster) Resume(_ context.Context, _, _ string) error {
	atomic.AddInt32(&f.calls, 1)
	if f.failNext {
		return errors.New("master failed")
	}
	return nil
}

type fakePush struct{}

func (fakePush) SetState(_ context.Context, _, _ string) error { return nil }
func (fakePush) DeleteMeta(_ context.Context, _ string) error  { return nil }

// ------ tests -------------------------------------------------------------

// helper wires up the same handlers Run() registers, so we can use httptest
// without binding a real port.
func newTestHandler(reg *registry.Registry, store *fakeStore, master *fakeMaster) http.Handler {
	r := resumer.New(resumer.Options{
		Registry:     reg,
		Redis:        store,
		CubeMaster:   master,
		ProxyPush:    fakePush{},
		StateLockTTL: time.Minute,
		Log:          zap.NewNop(),
	})
	s := New(":0", r, reg, zap.NewNop())
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/resume", s.handleResume)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	return mux
}

func TestResumeEndpoint_HappyPath(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	master := &fakeMaster{}
	srv := httptest.NewServer(newTestHandler(reg, newFakeStore(), master))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/resume?sandbox_id=sbx", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&master.calls); got != 1 {
		t.Fatalf("expected 1 master.Resume call, got %d", got)
	}
}

func TestResumeEndpoint_RejectsGet(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(registry.New(), newFakeStore(), &fakeMaster{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal/resume?sandbox_id=sbx")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestResumeEndpoint_BadRequestWithoutSandboxID(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(registry.New(), newFakeStore(), &fakeMaster{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/resume", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestResumeEndpoint_503OnResumerError(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
	})
	master := &fakeMaster{failNext: true}
	srv := httptest.NewServer(newTestHandler(reg, newFakeStore(), master))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/resume?sandbox_id=sbx", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503, got %d: %s", resp.StatusCode, body)
	}
}

func TestHealthzAndReadyz(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})

	srv := httptest.NewServer(newTestHandler(reg, newFakeStore(), &fakeMaster{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("/healthz wrong: status=%d body=%q", resp.StatusCode, body)
	}

	resp2, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/readyz wrong status: %d", resp2.StatusCode)
	}
	if !strings.Contains(string(body2), `"registry_len":1`) {
		t.Fatalf("/readyz body should mention registry_len=1: %s", body2)
	}
	// No LeaderGate configured: the process reports itself as a standalone
	// (single-replica) deployment, always ready.
	if !strings.Contains(string(body2), `"ok":true`) || !strings.Contains(string(body2), `"role":"standalone"`) {
		t.Fatalf("/readyz without a leader gate should report ok=true role=standalone: %s", body2)
	}
}

// stubFleetSize is a FleetSizer that returns a caller-provided constant.
type stubFleetSize int

func (s stubFleetSize) Snapshot() int { return int(s) }

// stubGate is a LeaderGate that returns a caller-provided constant.
type stubGate bool

func (g stubGate) IsLeader() bool { return bool(g) }

func TestReadyz_LeaderGate(t *testing.T) {
	build := func(gate LeaderGate) *httptest.Server {
		reg := registry.New()
		reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
		r := resumer.New(resumer.Options{
			Registry:     reg,
			Redis:        newFakeStore(),
			CubeMaster:   &fakeMaster{},
			ProxyPush:    fakePush{},
			StateLockTTL: time.Minute,
			Log:          zap.NewNop(),
		})
		s := New(":0", r, reg, zap.NewNop()).WithLeaderGate(gate)
		mux := http.NewServeMux()
		mux.HandleFunc("/readyz", s.handleReadyz)
		return httptest.NewServer(mux)
	}

	// Standby: still 200 — readiness expresses "process is healthy and can
	// serve" (a standby serves resumes via the meta-hash fallback); the role
	// is surfaced for observability only. Gating readiness on leadership
	// would deadlock rolling upgrades with maxUnavailable=0.
	standby := build(stubGate(false))
	defer standby.Close()
	resp, err := http.Get(standby.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("standby /readyz should be 200, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"role":"standby"`) {
		t.Fatalf("standby /readyz should report role=standby: %s", body)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("standby /readyz should report ok=true: %s", body)
	}

	// Leader: ready.
	leader := build(stubGate(true))
	defer leader.Close()
	resp2, err := http.Get(leader.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("leader /readyz should be 200, got %d: %s", resp2.StatusCode, body2)
	}
	if !strings.Contains(string(body2), `"role":"leader"`) {
		t.Fatalf("leader /readyz should report role=leader: %s", body2)
	}
	if !strings.Contains(string(body2), `"ok":true`) {
		t.Fatalf("leader /readyz should report ok=true: %s", body2)
	}
}

func TestReadyz_ExposesFleetSizeWhenConfigured(t *testing.T) {
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})

	// Build a Server with a fleet sizer attached and mount just /readyz.
	master := &fakeMaster{}
	r := resumer.New(resumer.Options{
		Registry:     reg,
		Redis:        newFakeStore(),
		CubeMaster:   master,
		ProxyPush:    fakePush{},
		StateLockTTL: time.Minute,
		Log:          zap.NewNop(),
	})
	s := New(":0", r, reg, zap.NewNop()).WithFleetSizer(stubFleetSize(3))
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", s.handleReadyz)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz wrong status: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"fleet_size":3`) {
		t.Fatalf("/readyz should surface fleet_size=3: %s", body)
	}
}
