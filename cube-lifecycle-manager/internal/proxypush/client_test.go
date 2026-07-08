// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package proxypush

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/discovery"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// mutableFleet is a Fleet whose endpoints can be swapped at runtime, used to
// verify that Client re-reads Snapshot() on every call rather than caching it.
type mutableFleet struct {
	mu  sync.Mutex
	eps []discovery.Endpoint
}

func (f *mutableFleet) Snapshot() []discovery.Endpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]discovery.Endpoint, len(f.eps))
	copy(out, f.eps)
	return out
}

func (f *mutableFleet) set(eps ...discovery.Endpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eps = append(f.eps[:0], eps...)
}

// fakeAdmin is a tiny stand-in for CubeProxy's admin server. It records every
// request it sees and serves /admin/last_active from an in-memory map.
type fakeAdmin struct {
	mu          sync.Mutex
	lastActive  map[string]int64
	now         int64
	tokenWanted string

	upserts  []map[string]any
	deletes  []map[string]any
	states   []map[string]any
	missingT int // count of requests missing the expected token
}

func (f *fakeAdmin) handler() http.Handler {
	mux := http.NewServeMux()

	check := func(w http.ResponseWriter, r *http.Request) bool {
		if f.tokenWanted == "" {
			return true
		}
		if r.Header.Get("X-Cube-Admin-Token") != f.tokenWanted {
			f.mu.Lock()
			f.missingT++
			f.mu.Unlock()
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		return true
	}

	mux.HandleFunc("/admin/meta/upsert", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		f.mu.Lock()
		f.upserts = append(f.upserts, obj)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/admin/meta/delete", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		f.mu.Lock()
		f.deletes = append(f.deletes, obj)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/admin/state", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		f.mu.Lock()
		f.states = append(f.states, obj)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/admin/last_active", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		f.mu.Lock()
		entries := make(map[string]int64, len(f.lastActive))
		for k, v := range f.lastActive {
			entries[k] = v
		}
		now := f.now
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"now":     now,
			"since":   0,
			"count":   len(entries),
			"entries": entries,
		})
	})
	return mux
}

func TestUpsertMeta_RoundTrip(t *testing.T) {
	fa := &fakeAdmin{lastActive: map[string]int64{}}
	srv := httptest.NewServer(fa.handler())
	defer srv.Close()

	c := New([]string{srv.URL}, "", time.Second, zap.NewNop())
	meta := lifecycle.SandboxLifecycleMeta{
		SandboxID: "sbx-1", AutoPause: true, TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
	}
	if err := c.UpsertMeta(context.Background(), meta); err != nil {
		t.Fatalf("UpsertMeta failed: %v", err)
	}
	if len(fa.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(fa.upserts))
	}
	if got, _ := fa.upserts[0]["sandbox_id"].(string); got != "sbx-1" {
		t.Fatalf("upsert sandbox_id wrong: %v", fa.upserts[0])
	}
}

func TestSetState_AndDeleteMeta(t *testing.T) {
	fa := &fakeAdmin{lastActive: map[string]int64{}}
	srv := httptest.NewServer(fa.handler())
	defer srv.Close()
	c := New([]string{srv.URL}, "", time.Second, zap.NewNop())

	if err := c.SetState(context.Background(), "sbx-1", "paused"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteMeta(context.Background(), "sbx-1"); err != nil {
		t.Fatal(err)
	}
	if len(fa.states) != 1 || fa.states[0]["state"] != "paused" {
		t.Fatalf("states wrong: %+v", fa.states)
	}
	if len(fa.deletes) != 1 || fa.deletes[0]["sandbox_id"] != "sbx-1" {
		t.Fatalf("deletes wrong: %+v", fa.deletes)
	}
}

func TestPullLastActive_MergesAcrossEndpoints(t *testing.T) {
	a := &fakeAdmin{lastActive: map[string]int64{"sbx-1": 100, "sbx-2": 50}, now: 1000}
	b := &fakeAdmin{lastActive: map[string]int64{"sbx-1": 200, "sbx-3": 75}, now: 1100}
	sa := httptest.NewServer(a.handler())
	defer sa.Close()
	sb := httptest.NewServer(b.handler())
	defer sb.Close()

	c := New([]string{sa.URL, sb.URL}, "", time.Second, zap.NewNop())
	merged, minNow, err := c.PullLastActive(context.Background(), 0)
	if err != nil {
		t.Fatalf("pull failed: %v", err)
	}
	if merged["sbx-1"] != 200 {
		t.Fatalf("expected merged sbx-1=200 (max across endpoints), got %d", merged["sbx-1"])
	}
	if merged["sbx-2"] != 50 || merged["sbx-3"] != 75 {
		t.Fatalf("merged map wrong: %+v", merged)
	}
	if minNow != 1000 {
		t.Fatalf("minNow should be 1000 (the smaller of the two clocks), got %d", minNow)
	}
}

func TestPullLastActive_TolerantToOneEndpointDown(t *testing.T) {
	a := &fakeAdmin{lastActive: map[string]int64{"sbx-1": 100}, now: 500}
	sa := httptest.NewServer(a.handler())
	defer sa.Close()

	// "sb" deliberately points at an unused port to force a connection error.
	c := New([]string{sa.URL, "http://127.0.0.1:1"}, "", 200*time.Millisecond, zap.NewNop())
	merged, _, err := c.PullLastActive(context.Background(), 0)
	if err != nil {
		t.Fatalf("partial-success pull should not error: %v", err)
	}
	if merged["sbx-1"] != 100 {
		t.Fatalf("expected merged sbx-1=100, got %+v", merged)
	}
}

func TestUpsertMeta_TokenHeader(t *testing.T) {
	fa := &fakeAdmin{lastActive: map[string]int64{}, tokenWanted: "secret"}
	srv := httptest.NewServer(fa.handler())
	defer srv.Close()

	withTok := New([]string{srv.URL}, "secret", time.Second, zap.NewNop())
	if err := withTok.UpsertMeta(context.Background(),
		lifecycle.SandboxLifecycleMeta{SandboxID: "ok"}); err != nil {
		t.Fatalf("token-bearing call should succeed: %v", err)
	}

	noTok := New([]string{srv.URL}, "", time.Second, zap.NewNop())
	if err := noTok.UpsertMeta(context.Background(),
		lifecycle.SandboxLifecycleMeta{SandboxID: "fail"}); err == nil {
		t.Fatal("token-less call should error")
	}
	if fa.missingT != 1 {
		t.Fatalf("expected exactly 1 missing-token rejection, got %d", fa.missingT)
	}
}

func TestClient_DynamicFleet(t *testing.T) {
	a := &fakeAdmin{lastActive: map[string]int64{}}
	b := &fakeAdmin{lastActive: map[string]int64{}}
	sa := httptest.NewServer(a.handler())
	defer sa.Close()
	sb := httptest.NewServer(b.handler())
	defer sb.Close()

	// Start with only a; after the first push add b and confirm the next
	// push reaches both. This is the crux of the discovery-driven design:
	// the Client must re-consult Fleet.Snapshot() on every call, not cache.
	fleet := &mutableFleet{}
	fleet.set(discovery.Endpoint{ProxyID: "a", AdminURL: sa.URL})

	c := NewWithFleet(fleet, "", time.Second, zap.NewNop())
	if err := c.UpsertMeta(context.Background(),
		lifecycle.SandboxLifecycleMeta{SandboxID: "sbx-1"}); err != nil {
		t.Fatalf("first push failed: %v", err)
	}
	if len(a.upserts) != 1 || len(b.upserts) != 0 {
		t.Fatalf("first push should hit only a; got a=%d b=%d",
			len(a.upserts), len(b.upserts))
	}

	fleet.set(
		discovery.Endpoint{ProxyID: "a", AdminURL: sa.URL},
		discovery.Endpoint{ProxyID: "b", AdminURL: sb.URL},
	)
	if err := c.UpsertMeta(context.Background(),
		lifecycle.SandboxLifecycleMeta{SandboxID: "sbx-2"}); err != nil {
		t.Fatalf("second push failed: %v", err)
	}
	if len(a.upserts) != 2 || len(b.upserts) != 1 {
		t.Fatalf("second push should fan out; got a=%d b=%d",
			len(a.upserts), len(b.upserts))
	}
}

func TestBroadcast_EmptyFleetIsNoOp(t *testing.T) {
	c := NewWithFleet(&mutableFleet{}, "", time.Second, zap.NewNop())
	// UpsertMeta / SetState / DeleteMeta / PullLastActive must all succeed
	// against an empty fleet — reconciliation happens on the next event.
	if err := c.UpsertMeta(context.Background(),
		lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"}); err != nil {
		t.Fatalf("UpsertMeta on empty fleet errored: %v", err)
	}
	if err := c.SetState(context.Background(), "sbx", "paused"); err != nil {
		t.Fatalf("SetState on empty fleet errored: %v", err)
	}
	if err := c.DeleteMeta(context.Background(), "sbx"); err != nil {
		t.Fatalf("DeleteMeta on empty fleet errored: %v", err)
	}
	got, minNow, err := c.PullLastActive(context.Background(), 0)
	if err != nil {
		t.Fatalf("PullLastActive on empty fleet errored: %v", err)
	}
	if len(got) != 0 || minNow != 0 {
		t.Fatalf("expected empty pull, got got=%v minNow=%d", got, minNow)
	}
}

func TestUpsertMetaTo_TargetsSingleEndpoint(t *testing.T) {
	a := &fakeAdmin{lastActive: map[string]int64{}}
	b := &fakeAdmin{lastActive: map[string]int64{}}
	sa := httptest.NewServer(a.handler())
	defer sa.Close()
	sb := httptest.NewServer(b.handler())
	defer sb.Close()

	fleet := &mutableFleet{}
	fleet.set(
		discovery.Endpoint{ProxyID: "a", AdminURL: sa.URL},
		discovery.Endpoint{ProxyID: "b", AdminURL: sb.URL},
	)
	c := NewWithFleet(fleet, "", time.Second, zap.NewNop())

	// A targeted push (used by discovery onJoin replay) must NOT hit the
	// unrelated endpoint.
	if err := c.UpsertMetaTo(context.Background(), sb.URL,
		lifecycle.SandboxLifecycleMeta{SandboxID: "replay-1"}); err != nil {
		t.Fatalf("UpsertMetaTo failed: %v", err)
	}
	if len(a.upserts) != 0 {
		t.Fatalf("targeted push leaked to unrelated endpoint a")
	}
	if len(b.upserts) != 1 {
		t.Fatalf("expected exactly 1 targeted push to b, got %d", len(b.upserts))
	}
}
