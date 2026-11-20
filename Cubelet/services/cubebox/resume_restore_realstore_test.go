// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// realStoreAPI adapts the real cubeboxstore.Store (bolt-backed indexer, the
// production persistence path) to the cubes.CubeboxAPI the service uses,
// mirroring cubes.local's Save (Add + Sync) and Get. The tombstone-restore
// identity check must work against the real store's Save→Sync→Get sequence —
// including Sync's deep-copy on the persistence path — not just against a
// fake that preserves pointer identity by construction.
type realStoreAPI struct {
	store     *cubeboxstore.Store
	saveCalls int
}

func (r *realStoreAPI) Init(context.Context) error { return nil }

func (r *realStoreAPI) Get(_ context.Context, id string) (*cubeboxstore.CubeBox, error) {
	return r.store.Get(id)
}

func (r *realStoreAPI) FindContainerOfCubebox(context.Context, string) (*cubeboxstore.Container, *cubeboxstore.CubeBox, error) {
	return nil, nil, nil
}

func (r *realStoreAPI) List() []*cubeboxstore.CubeBox { return r.store.List() }

func (r *realStoreAPI) IsImageInUse(string) (bool, error) { return false, nil }

func (r *realStoreAPI) Save(_ context.Context, cb *cubeboxstore.CubeBox, _ ...cubes.UpdateCubeboxOpt) error {
	r.saveCalls++
	r.store.Add(cb)
	return r.store.Sync(cb.ID)
}

func (r *realStoreAPI) SyncByID(_ context.Context, id string, _ ...cubes.UpdateCubeboxOpt) error {
	return r.store.Sync(id)
}

func (r *realStoreAPI) Delete(_ context.Context, opt *cubes.DeleteOption) error {
	return r.store.DeleteSync(opt.CubeboxID)
}

// TestRestorePausedReplaceSnapshotAgainstRealStore drives the actual
// store sequence a create-from-pause rollback sees: tombstone Save'd, new row
// Save'd over it (the flow's first Save), then the restore Save's the snapshot
// back and a fresh Get must return the tombstone — with the identity check
// keyed on the CreatedAt value, which survives Sync's deep copy.
func TestRestorePausedReplaceSnapshotAgainstRealStore(t *testing.T) {
	db, err := utils.NewCubeStoreExt(t.TempDir(), "meta_restore.db", 10, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr := &realStoreAPI{store: cubeboxstore.NewStore(db)}
	l := &local{cubeboxManger: mgr}
	ctx := context.Background()

	// The pre-attempt tombstone, as the store holds it before the resume.
	tomb := sandboxWithResourceForTest("sb-realstore", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Add(-time.Minute).UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tomb.SandboxID = "sb-realstore"
	tomb.RootfsWritableLayer = "disk/sb-realstore"
	require.NoError(t, mgr.Save(ctx, tomb))
	snapshot := tomb.DeepCopy() // what the replacement branch captures

	// The flow's first Save overwrites the row with the new attempt's record.
	debris := newCubeboxWithStatusForTest("sb-realstore", cubeboxstore.Status{
		CreatedAt: time.Now().UnixNano(),
	})
	debris.SandboxID = "sb-realstore"
	require.NoError(t, mgr.Save(ctx, debris))

	cur, err := mgr.Get(ctx, "sb-realstore")
	require.NoError(t, err)
	require.NotNil(t, cur)
	assert.NotEqual(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, cur.GetStatus().Get().State(),
		"the flow's Save must have replaced the tombstone")

	// The rollback restores the snapshot, keyed on the debris row's CreatedAt.
	opts := rollbackDestroyContext("sb-realstore")
	opts.PausedReplaceSnapshot = snapshot
	opts.PausedReplaceDebrisCreatedAt = debris.CreatedAt
	require.True(t, l.restorePausedReplaceSnapshot(ctx, opts, "sb-realstore"))

	// A fresh Get (through the real indexer, after Sync persisted it) must see
	// the tombstone again, with the fields the retry's gates and rebuild need.
	restored, err := mgr.Get(ctx, "sb-realstore")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, restored.GetStatus().Get().State())
	assert.Equal(t, snapshot.CreatedAt, restored.CreatedAt,
		"the restore keeps the tombstone's own CreatedAt — the value identity is stable across Save/Sync/Get")
	assert.NotEqual(t, debris.CreatedAt, restored.CreatedAt,
		"the debris row's identity is gone with it")
	assert.NotNil(t, restored.ResourceWithOverHead)
	assert.Equal(t, "disk/sb-realstore", restored.RootfsWritableLayer)
	assert.Nil(t, restored.UserMarkDeletedTime)
}

// TestRestoreIdentitySurvivesRealStoreRowReplacement is the negative twin: a
// newer attempt's Save changes the row's CreatedAt, and the stale rollback's
// restore must then leave the newer row alone.
func TestRestoreIdentitySurvivesRealStoreRowReplacement(t *testing.T) {
	db, err := utils.NewCubeStoreExt(t.TempDir(), "meta_restore_newer.db", 10, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr := &realStoreAPI{store: cubeboxstore.NewStore(db)}
	l := &local{cubeboxManger: mgr}
	ctx := context.Background()

	tomb := sandboxWithResourceForTest("sb-newer", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Add(-time.Minute).UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tomb.SandboxID = "sb-newer"
	require.NoError(t, mgr.Save(ctx, tomb))
	staleSnapshot := tomb.DeepCopy()
	staleCreatedAt := tomb.CreatedAt

	newer := newCubeboxWithStatusForTest("sb-newer", cubeboxstore.Status{
		CreatedAt: time.Now().Add(time.Second).UnixNano(),
	})
	newer.SandboxID = "sb-newer"
	require.NoError(t, mgr.Save(ctx, newer))

	savesBefore := mgr.saveCalls
	opts := rollbackDestroyContext("sb-newer")
	opts.PausedReplaceSnapshot = staleSnapshot
	opts.PausedReplaceDebrisCreatedAt = staleCreatedAt
	assert.True(t, l.restorePausedReplaceSnapshot(ctx, opts, "sb-newer"),
		"the stale rollback must claim the row (so no delete runs) without restoring over it")
	assert.Equal(t, savesBefore, mgr.saveCalls, "the stale snapshot must not be saved")

	cur, err := mgr.Get(ctx, "sb-newer")
	require.NoError(t, err)
	assert.Equal(t, newer.CreatedAt, cur.CreatedAt, "the newer attempt's row is untouched")
}
