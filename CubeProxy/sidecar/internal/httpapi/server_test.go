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

	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/registry"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/resumer"
)

// ------ resumer test doubles (re-used pattern from resumer_test.go) -------

type fakeStore struct {
	states map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{states: map[string]string{}} }
func (f *fakeStore) AcquireState(_ context.Context, sid, state string, _ time.Duration) (bool, error) {
	if _, ok := f.states[sid]; ok {
		return false, nil
	}
	f.states[sid] = state
	return true, nil
}
func (f *fakeStore) SetState(_ context.Context, sid, state string, _ time.Duration) error {
	f.states[sid] = state
	return nil
}
func (f *fakeStore) ClearState(_ context.Context, sid string) error {
	delete(f.states, sid)
	return nil
}
func (f *fakeStore) GetState(_ context.Context, sid string) (string, bool, error) {
	v, ok := f.states[sid]
	return v, ok, nil
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
}
