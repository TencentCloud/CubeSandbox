// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/discovery"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/leader"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/proxypush"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/statesync"
)

type standbyStatus struct{}

func (standbyStatus) IsLeader() bool { return false }
func (standbyStatus) Enabled() bool  { return true }

func TestCatchUpStreamToDrainsPromotionHighWater(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	addEvent := func(id, sandboxID string) {
		t.Helper()
		payload, err := json.Marshal(lifecycle.SandboxLifecycleMeta{SandboxID: sandboxID})
		if err != nil {
			t.Fatal(err)
		}
		if err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: lifecycle.EventStreamKey,
			ID:     id,
			Values: map[string]interface{}{
				lifecycle.FieldOp:        lifecycle.OpCreate,
				lifecycle.FieldSandboxID: sandboxID,
				lifecycle.FieldPayload:   string(payload),
				lifecycle.FieldTimestamp: time.Now().UnixMilli(),
			},
		}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	addEvent("1-0", "already-applied")
	addEvent("2-0", "must-catch-up")

	stream := redisstream.New(rdb, zap.NewNop())
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "already-applied"})
	progress := newStreamProgress("1-0")
	deps := statesync.Deps{Registry: reg, Leader: standbyStatus{}, Log: zap.NewNop()}
	push := proxypush.New(nil, "", time.Second, zap.NewNop())

	if err := catchUpStreamTo(
		ctx, "2-0", stream, push, reg, deps, progress, zap.NewNop(),
	); err != nil {
		t.Fatal(err)
	}
	if got := progress.Cursor(); got != "2-0" {
		t.Fatalf("cursor = %q, want 2-0", got)
	}
	if reg.Get("must-catch-up") == nil {
		t.Fatal("promotion catch-up did not apply high-water event")
	}
}

func TestCatchUpStateEventKeepsWarmStateWithoutWritingRedis(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	stream := redisstream.New(rdb, zap.NewNop())

	if err := stream.SetState(ctx, "sbx", lifecycle.StatePaused, time.Minute); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(lifecycle.StatePayload{
		State: lifecycle.StateRunning,
		Actor: lifecycle.ActorCubeMaster,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: lifecycle.EventStreamKey,
		ID:     "10-0",
		Values: map[string]interface{}{
			lifecycle.FieldOp:        lifecycle.OpState,
			lifecycle.FieldSandboxID: "sbx",
			lifecycle.FieldPayload:   string(payload),
			lifecycle.FieldTimestamp: time.Now().UnixMilli(),
		},
	}).Err(); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	reg.SetRuntimeState("sbx", lifecycle.StatePaused)
	progress := newStreamProgress("0-0")
	deps := statesync.Deps{
		Registry: reg,
		Redis:    stream,
		TTL:      time.Minute,
		Leader:   standbyStatus{},
		Log:      zap.NewNop(),
	}
	push := proxypush.New(nil, "", time.Second, zap.NewNop())

	if err := catchUpStreamTo(ctx, "10-0", stream, push, reg, deps, progress, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if got := reg.Get("sbx").RuntimeState; got != lifecycle.StateRunning {
		t.Fatalf("RuntimeState = %q, want running", got)
	}
	if got, _, err := stream.GetState(ctx, "sbx"); err != nil || got != lifecycle.StatePaused {
		t.Fatalf("standby catch-up wrote Redis state %q, want paused", got)
	}
}

// deleteRecorder stubs a CubeProxy admin endpoint and reports the sandbox IDs
// pushed to /admin/meta/delete.
func deleteRecorder(t *testing.T) (adminURL string, deleted func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/meta/delete" {
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			seen = append(seen, payload["sandbox_id"])
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)
	return ts.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// A delete event always drops the sandbox from the local registry and every
// replica pushes DeleteMeta: the op is terminal and sandbox IDs are not reused.
func TestCatchUpDeleteEventPushesProxyFromAllReplicas(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	stream := redisstream.New(rdb, zap.NewNop())
	adminURL, deleted := deleteRecorder(t)

	if err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: lifecycle.EventStreamKey,
		ID:     "10-0",
		Values: map[string]interface{}{
			lifecycle.FieldOp:        lifecycle.OpDelete,
			lifecycle.FieldSandboxID: "sbx-del",
			lifecycle.FieldTimestamp: time.Now().UnixMilli(),
		},
	}).Err(); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx-del"})
	deps := statesync.Deps{
		Registry: reg,
		Redis:    stream,
		Leader:   standbyStatus{},
		Log:      zap.NewNop(),
	}
	push := proxypush.New([]string{adminURL}, "", time.Second, zap.NewNop())

	if err := catchUpStreamTo(
		ctx, "10-0", stream, push, reg, deps, newStreamProgress("0-0"), zap.NewNop(),
	); err != nil {
		t.Fatal(err)
	}
	if reg.Get("sbx-del") != nil {
		t.Fatal("registry entry should have been deleted")
	}
	got := deleted()
	if len(got) != 1 || got[0] != "sbx-del" {
		t.Fatalf("standby must push DeleteMeta, got %v", got)
	}
}

func TestStreamProgressRejectsPrefetchedOlderBatch(t *testing.T) {
	progress := newStreamProgress("100-0")
	progress.Advance("200-0") // promotion catch-up
	if progress.ShouldApply("150-0") {
		t.Fatal("prefetched event older than promotion high-water was accepted")
	}
	if !progress.ShouldApply("201-0") {
		t.Fatal("new event after promotion high-water was rejected")
	}
}

func TestResolvePromotionState(t *testing.T) {
	cases := []struct {
		name   string
		local  string
		shared string
		want   string
	}{
		{"both empty", "", "", ""},
		{"local only", lifecycle.StatePaused, "", lifecycle.StatePaused},
		{"shared only", "", lifecycle.StateRunning, lifecycle.StateRunning},
		{"agree running", lifecycle.StateRunning, lifecycle.StateRunning, lifecycle.StateRunning},
		{"agree paused", lifecycle.StatePaused, lifecycle.StatePaused, lifecycle.StatePaused},
		{"disagree prefers paused", lifecycle.StatePaused, lifecycle.StateRunning, lifecycle.StatePaused},
		{"disagree running local", lifecycle.StateRunning, lifecycle.StatePaused, lifecycle.StatePaused},
		{"skip pausing", lifecycle.StatePaused, "pausing", ""},
		{"skip resuming", lifecycle.StateRunning, "resuming", ""},
		{"skip killing", lifecycle.StatePaused, "killing", ""},
		{"skip killed", lifecycle.StateRunning, lifecycle.StateKilled, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePromotionState(tc.local, tc.shared); got != tc.want {
				t.Fatalf("resolvePromotionState(%q, %q) = %q, want %q",
					tc.local, tc.shared, got, tc.want)
			}
		})
	}
}

func TestReconcileSharedStateWritesStaleRunningToPaused(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	stream := redisstream.New(rdb, zap.NewNop())
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	reg.SetRuntimeState("sbx", lifecycle.StatePaused)
	if err := stream.SetState(ctx, "sbx", lifecycle.StateRunning, time.Minute); err != nil {
		t.Fatal(err)
	}

	got, err := reconcileSharedState(ctx, stream, reg, time.Minute, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if got["sbx"] != lifecycle.StatePaused {
		t.Fatalf("resolved = %q, want paused", got["sbx"])
	}
	if state, _, err := stream.GetState(ctx, "sbx"); err != nil || state != lifecycle.StatePaused {
		t.Fatalf("redis state = %q, want paused", state)
	}
}

func TestReconcileSharedStateSkipsTransitionMarker(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	stream := redisstream.New(rdb, zap.NewNop())
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	reg.SetRuntimeState("sbx", lifecycle.StateRunning)
	if err := stream.SetState(ctx, "sbx", "resuming", time.Minute); err != nil {
		t.Fatal(err)
	}

	got, err := reconcileSharedState(ctx, stream, reg, time.Minute, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["sbx"]; ok {
		t.Fatalf("transition marker must be skipped, got %v", got)
	}
	if state, _, err := stream.GetState(ctx, "sbx"); err != nil || state != "resuming" {
		t.Fatalf("redis state = %q, want resuming", state)
	}
}

// mgetFlipHook rewrites a Redis key after MGET so WriteStateCAS sees a
// concurrent resume and must not overwrite it.
type mgetFlipHook struct {
	mini *miniredis.Miniredis
	key  string
	val  string
	once sync.Once
}

func (h *mgetFlipHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *mgetFlipHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() == "mget" {
			h.once.Do(func() { _ = h.mini.Set(h.key, h.val) })
		}
		return err
	}
}

func (h *mgetFlipHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestReconcileSharedStateCASMismatchLeavesRedis(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	rdb.AddHook(&mgetFlipHook{mini: server, key: lifecycle.StateKey("sbx"), val: "resuming"})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	stream := redisstream.New(rdb, zap.NewNop())
	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	reg.SetRuntimeState("sbx", lifecycle.StatePaused)
	if err := stream.SetState(ctx, "sbx", lifecycle.StateRunning, time.Minute); err != nil {
		t.Fatal(err)
	}

	got, err := reconcileSharedState(ctx, stream, reg, time.Minute, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["sbx"]; ok {
		t.Fatalf("CAS mismatch must drop sid, got %v", got)
	}
	if state, _, err := stream.GetState(ctx, "sbx"); err != nil || state != "resuming" {
		t.Fatalf("redis state = %q, want resuming", state)
	}
}

func TestReconciledLeaderDisabledElectionIgnoresGeneration(t *testing.T) {
	lease := leader.New(leader.Options{Enabled: false, Identity: "single", Log: zap.NewNop()})
	active := &reconciledLeader{lease: lease}
	if !active.IsLeader() {
		t.Fatal("disabled election must not wait for stream catch-up")
	}
	active.invalidate()
	if !active.IsLeader() {
		t.Fatal("invalidate must not demote a disabled-election replica")
	}
}

func TestRebuildRegistryAfterTrimPreservesActivityAndReadsCursorFirst(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	stream := redisstream.New(rdb, zap.NewNop())

	meta := lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"}
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.HSet(ctx, lifecycle.MetaKey, "sbx", payload).Err(); err != nil {
		t.Fatal(err)
	}
	addCreate := func(id string) {
		t.Helper()
		if err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: lifecycle.EventStreamKey,
			ID:     id,
			Values: map[string]interface{}{
				lifecycle.FieldOp:        lifecycle.OpCreate,
				lifecycle.FieldSandboxID: "sbx",
				lifecycle.FieldPayload:   string(payload),
				lifecycle.FieldTimestamp: time.Now().UnixMilli(),
			},
		}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	addCreate("1-0")
	addCreate("2-0")

	startupTs := time.Unix(1_700_000_000, 0)
	reg := registry.New()
	reg.Upsert(meta)
	reg.MergeLastActive("sbx", 42)
	reg.SetRuntimeState("sbx", lifecycle.StatePaused)
	progress := newStreamProgress("0-0")

	if err := rebuildRegistryAfterTrim(ctx, stream, reg, progress, startupTs, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	got := reg.Get("sbx")
	if got == nil {
		t.Fatal("rebuild dropped sandbox still present in the Hash")
	}
	if got.LastActiveMs != 42 {
		t.Fatalf("LastActiveMs = %d, want 42", got.LastActiveMs)
	}
	if got.RuntimeState != lifecycle.StatePaused {
		t.Fatalf("RuntimeState = %q, want paused", got.RuntimeState)
	}
	if !got.FirstSeenAt.Equal(startupTs) {
		t.Fatalf("FirstSeenAt = %v, want startupTs %v", got.FirstSeenAt, startupTs)
	}
	if progress.Cursor() != "2-0" {
		t.Fatalf("cursor = %q, want 2-0 from LatestID before HGETALL", progress.Cursor())
	}
}

func TestReplayRegistryToPushesPausedState(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var states []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/admin/state" {
			var payload map[string]string
			_ = json.Unmarshal(body, &payload)
			states = append(states, payload["state"])
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)

	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	reg.SetRuntimeState("sbx", lifecycle.StatePaused)
	push := proxypush.New([]string{ts.URL}, "", time.Second, zap.NewNop())
	ep := discovery.Endpoint{ProxyID: "p", AdminURL: ts.URL}

	resolved := map[string]string{"sbx": lifecycle.StatePaused}
	if !replayRegistryTo(context.Background(), push, resolved, reg, ep, zap.NewNop()) {
		t.Fatal("replayRegistryTo failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/admin/meta/upsert" || paths[1] != "/admin/state" {
		t.Fatalf("paths = %v, want [upsert, state]", paths)
	}
	if len(states) != 1 || states[0] != lifecycle.StatePaused {
		t.Fatalf("states = %v, want [paused]", states)
	}
}

func TestReplayRegistryToReresolvesLivePausedOverStaleRunning(t *testing.T) {
	var mu sync.Mutex
	var states []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/admin/state" {
			var payload map[string]string
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			states = append(states, payload["state"])
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)

	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	reg.SetRuntimeState("sbx", lifecycle.StatePaused)
	push := proxypush.New([]string{ts.URL}, "", time.Second, zap.NewNop())
	ep := discovery.Endpoint{ProxyID: "p", AdminURL: ts.URL}

	stale := map[string]string{"sbx": lifecycle.StateRunning}
	if !replayRegistryTo(context.Background(), push, stale, reg, ep, zap.NewNop()) {
		t.Fatal("replayRegistryTo failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 1 || states[0] != lifecycle.StatePaused {
		t.Fatalf("states = %v, want [paused]", states)
	}
}

func TestReconcileOnLeadershipDoesNotWaitForFleetHTTP(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	lease := leader.New(leader.Options{
		Redis:         rdb,
		Key:           "lease",
		Identity:      "promoted",
		Enabled:       true,
		TTL:           2 * time.Second,
		RenewInterval: 200 * time.Millisecond,
		RetryInterval: 20 * time.Millisecond,
		Log:           zap.NewNop(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = lease.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for !lease.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("lease was not acquired")
		}
		time.Sleep(10 * time.Millisecond)
	}

	reg := registry.New()
	reg.Upsert(lifecycle.SandboxLifecycleMeta{SandboxID: "sbx"})
	active := &reconciledLeader{lease: lease}
	if active.IsLeader() {
		t.Fatal("executable leadership must wait for stream catch-up")
	}

	stream := redisstream.New(rdb, zap.NewNop())
	progress := newStreamProgress("0-0")
	fleet := discovery.NewStatic([]string{ts.URL})
	push := proxypush.NewWithFleet(fleet, "", time.Second, zap.NewNop())
	var mu sync.Mutex
	deps := statesync.Deps{Registry: reg, Leader: active, Log: zap.NewNop()}
	go func() {
		_ = reconcileOnLeadership(
			ctx, lease, active, stream, push, reg, fleet, deps, progress, &mu,
			10*time.Millisecond, 0, time.Minute, zap.NewNop(),
		)
	}()

	deadline = time.Now().Add(2 * time.Second)
	for !active.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("leadership stayed blocked on CubeProxy hydrate failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startTestLease(t *testing.T, rdb *redis.Client) (*leader.Lease, context.Context) {
	t.Helper()
	lease := leader.New(leader.Options{
		Redis:         rdb,
		Key:           "lease",
		Identity:      "promoted",
		Enabled:       true,
		TTL:           2 * time.Second,
		RenewInterval: 200 * time.Millisecond,
		RetryInterval: 20 * time.Millisecond,
		Log:           zap.NewNop(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = lease.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !lease.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("lease was not acquired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return lease, ctx
}

func TestInvalidateKeepsGenerationAndSkipsHTTPDrain(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	lease, ctx := startTestLease(t, rdb)

	active := &reconciledLeader{lease: lease}
	active.markReconciled(lease.Generation())
	active.invalidate()
	if active.IsLeader() {
		t.Fatal("invalidate must pause executable leadership")
	}
	if got := active.generation.Load(); got != lease.Generation() {
		t.Fatalf("invalidate cleared generation to %d", got)
	}

	stream := redisstream.New(rdb, zap.NewNop())
	reg := registry.New()
	progress := newStreamProgress("0-0")
	fleet := discovery.NewStatic(nil)
	push := proxypush.New(nil, "", time.Second, zap.NewNop())
	var mu sync.Mutex
	deps := statesync.Deps{Registry: reg, Leader: active, Log: zap.NewNop()}
	go func() {
		_ = reconcileOnLeadership(
			ctx, lease, active, stream, push, reg, fleet, deps, progress, &mu,
			10*time.Millisecond, time.Hour, time.Minute, zap.NewNop(),
		)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !active.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("same-generation restore waited for HTTPTimeout drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFirstGenerationStillDrainsBeforeBecomingExecutable(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	lease, ctx := startTestLease(t, rdb)

	active := &reconciledLeader{lease: lease}
	stream := redisstream.New(rdb, zap.NewNop())
	reg := registry.New()
	progress := newStreamProgress("0-0")
	fleet := discovery.NewStatic(nil)
	push := proxypush.New(nil, "", time.Second, zap.NewNop())
	var mu sync.Mutex
	deps := statesync.Deps{Registry: reg, Leader: active, Log: zap.NewNop()}
	go func() {
		_ = reconcileOnLeadership(
			ctx, lease, active, stream, push, reg, fleet, deps, progress, &mu,
			10*time.Millisecond, time.Hour, time.Minute, zap.NewNop(),
		)
	}()

	time.Sleep(150 * time.Millisecond)
	if active.IsLeader() {
		t.Fatal("first-generation promotion skipped HTTPTimeout drain")
	}
}

func TestRestoreIfSameGenerationAfterTrim(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	lease, _ := startTestLease(t, rdb)

	active := &reconciledLeader{lease: lease}
	active.markReconciled(lease.Generation())
	active.invalidate()
	if active.IsLeader() {
		t.Fatal("invalidate must pause executable leadership")
	}
	active.restoreIfSameGeneration()
	if !active.IsLeader() {
		t.Fatal("trim rebuild should restore the same lease generation without draining")
	}
}
