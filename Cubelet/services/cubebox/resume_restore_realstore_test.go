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
// mirroring cubes.local's Save (Add + Sync) and Get. The tombstone restore
// must work against the real store's Save→Sync→Get sequence — including
// Sync's deep-copy on the persistence path — not just against a fake that
// preserves pointer identity by construction.
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

// TestRestorePausedTombstoneAgainstRealStore drives the actual store sequence
// a failed create-from-pause rollback sees: tombstone Save'd, the flow's first
// Save overwrites it with the new attempt's debris, then the synchronous
// restore Save's the gate's snapshot back — and a fresh Get through the real
// indexer must return the tombstone, PAUSED, with the rollback's delete marks
// scrubbed, so the retry's gate and quota admission see exactly the record the
// first attempt saw.
func TestRestorePausedTombstoneAgainstRealStore(t *testing.T) {
	db, err := utils.NewCubeStoreExt(t.TempDir(), "meta_restore.db", 10, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr := &realStoreAPI{store: cubeboxstore.NewStore(db)}
	s := &service{cubeboxMgr: &local{cubeboxManger: mgr}}
	ctx := context.Background()

	// The pre-attempt tombstone, as the store holds it before the resume.
	tomb := sandboxWithResourceForTest("sb-realstore", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Add(-time.Minute).UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tomb.SandboxID = "sb-realstore"
	tomb.RootfsWritableLayer = "disk/sb-realstore"
	require.NoError(t, mgr.Save(ctx, tomb))
	snapshot := tomb.DeepCopy() // what the gate captures before the flow runs

	// The flow's first Save overwrites the row with the new attempt's record
	// (runContainer/probe/envd then fail — the common post-Save failure shape).
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

	// The rollback destroy stamps the delete marks (they let the CDP delete
	// hooks pass); the restore must scrub them or the restored tombstone would
	// read as user-deleted to gcservice.allowCleanup.
	marked := time.Now()
	snapshot.UserMarkDeletedTime = &marked
	snapshot.DeleteRequestID = "rollback-destroy"
	s.restorePausedTombstone(ctx, snapshot)

	// A fresh Get (through the real indexer, after Sync persisted it) must see
	// the tombstone again, with the fields the retry's gates and rebuild need.
	restored, err := mgr.Get(ctx, "sb-realstore")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, restored.GetStatus().Get().State())
	assert.Equal(t, snapshot.CreatedAt, restored.CreatedAt,
		"the restore keeps the tombstone's own CreatedAt — stable across Save/Sync/Get")
	assert.NotEqual(t, debris.CreatedAt, restored.CreatedAt,
		"the debris row's identity is gone with it")
	assert.NotNil(t, restored.ResourceWithOverHead)
	assert.Equal(t, "disk/sb-realstore", restored.RootfsWritableLayer)
	assert.Nil(t, restored.UserMarkDeletedTime,
		"the restored tombstone must not read as user-deleted")
	assert.Empty(t, restored.DeleteRequestID)
}

// TestRestorePausedTombstoneNilSnapshotIsNoop pins the cross-node resume
// shape: no local tombstone existed, so the rollback destroy is the whole
// story and the restore hook must not write anything.
func TestRestorePausedTombstoneNilSnapshotIsNoop(t *testing.T) {
	db, err := utils.NewCubeStoreExt(t.TempDir(), "meta_restore_nil.db", 10, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr := &realStoreAPI{store: cubeboxstore.NewStore(db)}
	s := &service{cubeboxMgr: &local{cubeboxManger: mgr}}

	s.restorePausedTombstone(context.Background(), nil)

	assert.Zero(t, mgr.saveCalls, "a nil snapshot must not touch the store")
}
