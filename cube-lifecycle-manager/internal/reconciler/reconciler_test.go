// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
)

type fakeMetaStore struct {
	metas   map[string]lifecycle.SandboxLifecycleMeta
	err     error
	cleared []string
}

func (f *fakeMetaStore) Bootstrap(_ context.Context) (map[string]lifecycle.SandboxLifecycleMeta, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.metas, nil
}

func (f *fakeMetaStore) ClearState(_ context.Context, sid string) error {
	f.cleared = append(f.cleared, sid)
	return nil
}

type fakeMetaPush struct {
	upserted []lifecycle.SandboxLifecycleMeta
	deleted  []string
}

func (f *fakeMetaPush) UpsertMeta(_ context.Context, meta lifecycle.SandboxLifecycleMeta) error {
	f.upserted = append(f.upserted, meta)
	return nil
}

func (f *fakeMetaPush) DeleteMeta(_ context.Context, sid string) error {
	f.deleted = append(f.deleted, sid)
	return nil
}

func meta(sid string, timeoutSec int) lifecycle.SandboxLifecycleMeta {
	return lifecycle.SandboxLifecycleMeta{
		SandboxID:      sid,
		InstanceType:   "cubebox",
		TimeoutSeconds: lifecycle.TimeoutSecondsPtr(timeoutSec),
		AutoPause:      true,
		CreatedAt:      1700000000000,
	}
}

func newTestReconciler(reg *registry.Registry, store *fakeMetaStore, push *fakeMetaPush, interval time.Duration) *Reconciler {
	return New(Options{
		Registry:  reg,
		Redis:     store,
		ProxyPush: push,
		Interval:  interval,
		Log:       zap.NewNop(),
	})
}

func TestReconcileAdoptsMissingEntry(t *testing.T) {
	reg := registry.New()
	store := &fakeMetaStore{metas: map[string]lifecycle.SandboxLifecycleMeta{
		"sbx-new": meta("sbx-new", 300),
	}}
	push := &fakeMetaPush{}

	newTestReconciler(reg, store, push, time.Minute).reconcileOnce(context.Background())

	e := reg.Get("sbx-new")
	require.NotNil(t, e, "sandbox present in the meta hash must be adopted into the registry")
	assert.Equal(t, 300, *e.Meta.TimeoutSeconds)
	require.Len(t, push.upserted, 1)
	assert.Equal(t, "sbx-new", push.upserted[0].SandboxID)
	assert.Empty(t, push.deleted)
}

func TestReconcileRefreshesStaleMeta(t *testing.T) {
	reg := registry.New()
	reg.Upsert(meta("sbx-1", 300))
	store := &fakeMetaStore{metas: map[string]lifecycle.SandboxLifecycleMeta{
		"sbx-1": meta("sbx-1", 600), // timeout changed on CubeMaster
	}}
	push := &fakeMetaPush{}

	newTestReconciler(reg, store, push, time.Minute).reconcileOnce(context.Background())

	e := reg.Get("sbx-1")
	require.NotNil(t, e)
	assert.Equal(t, 600, *e.Meta.TimeoutSeconds, "registry must converge to the hash value")
	require.Len(t, push.upserted, 1, "the updated meta must be re-pushed to proxies")
}

func TestReconcileNoDriftIsQuiet(t *testing.T) {
	reg := registry.New()
	reg.Upsert(meta("sbx-1", 300))
	store := &fakeMetaStore{metas: map[string]lifecycle.SandboxLifecycleMeta{
		"sbx-1": meta("sbx-1", 300),
	}}
	push := &fakeMetaPush{}

	newTestReconciler(reg, store, push, time.Minute).reconcileOnce(context.Background())

	assert.Empty(t, push.upserted)
	assert.Empty(t, push.deleted)
	assert.Empty(t, store.cleared)
	assert.Equal(t, 1, reg.Len())
}

func TestReconcileEvictsEntryGoneFromHash(t *testing.T) {
	reg := registry.New()
	reg.Upsert(meta("sbx-gone", 300))
	// Backdate past the grace window so the entry is eligible for eviction.
	reg.SetFirstSeenAt("sbx-gone", time.Now().Add(-2*time.Minute))
	store := &fakeMetaStore{metas: map[string]lifecycle.SandboxLifecycleMeta{}}
	push := &fakeMetaPush{}

	newTestReconciler(reg, store, push, time.Minute).reconcileOnce(context.Background())

	assert.Nil(t, reg.Get("sbx-gone"), "entry absent from the hash must be evicted")
	assert.Equal(t, []string{"sbx-gone"}, push.deleted)
	assert.Equal(t, []string{"sbx-gone"}, store.cleared,
		"a lingering transition lock must be cleared alongside the eviction")
}

func TestReconcileGraceWindowProtectsFreshEntries(t *testing.T) {
	reg := registry.New()
	reg.Upsert(meta("sbx-fresh", 300)) // FirstSeenAt = now, within one Interval
	store := &fakeMetaStore{metas: map[string]lifecycle.SandboxLifecycleMeta{}}
	push := &fakeMetaPush{}

	newTestReconciler(reg, store, push, time.Minute).reconcileOnce(context.Background())

	assert.NotNil(t, reg.Get("sbx-fresh"),
		"an entry learned about less than one interval ago must survive the pass")
	assert.Empty(t, push.deleted)
}

func TestReconcileBootstrapErrorSkipsPass(t *testing.T) {
	reg := registry.New()
	reg.Upsert(meta("sbx-1", 300))
	reg.SetFirstSeenAt("sbx-1", time.Now().Add(-2*time.Minute))
	store := &fakeMetaStore{err: errors.New("redis hgetall failed")}
	push := &fakeMetaPush{}

	newTestReconciler(reg, store, push, time.Minute).reconcileOnce(context.Background())

	// A failed read must not be mistaken for "the hash is empty".
	assert.NotNil(t, reg.Get("sbx-1"))
	assert.Empty(t, push.deleted)
}
