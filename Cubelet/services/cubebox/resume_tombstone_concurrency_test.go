// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func newTombstoneConcurrencyStore() *tombstoneStore {
	return &tombstoneStore{
		storeViewCubeboxAPI: &storeViewCubeboxAPI{
			byID: make(map[string]*cubeboxstore.CubeBox),
		},
	}
}

// A failed create leaves a partially built row behind. It is not PAUSED.
func tombstoneDebrisForTest(id string) *cubeboxstore.CubeBox {
	sb := sandboxWithResourceForTest(id,
		cubeboxstore.Status{CreatedAt: time.Now().UnixNano()},
		"1000m", "2Gi", 1, 0, 0)
	sb.SandboxID = id
	return sb
}

func newCreateAttemptOptsForTest(mgr *local, orig *cubeboxstore.CubeBox) *workflow.CreateContext {
	opts := &workflow.CreateContext{}
	opts.SandboxID = orig.ID
	mgr.snapshotPausedTombstoneForReplace(orig.ID, orig, opts)
	return opts
}

func rollbackOptsForTest(id string, snap *cubeboxstore.CubeBox, guard *workflow.PausedTombstoneGuard) *workflow.DestroyContext {
	opts := &workflow.DestroyContext{
		DestroyInfo:            &cubeboxpb.DestroyCubeSandboxRequest{SandboxID: id, RequestID: "req-rollback"},
		RestorePausedTombstone: snap,
		PausedTombstoneGuard:   guard,
	}
	opts.IsRollBack = true
	opts.SandboxID = id
	return opts
}

// Review: if a retry stores a RUNNING row while a failed attempt's rollback is
// still in flight, neither the synchronous restore nor the late failover may
// turn that RUNNING row back into a PAUSED tombstone.
func TestLateRollbackDoesNotOverwriteSuccessfulRetry(t *testing.T) {
	snap := pausedTombstoneForTest()
	store := newTombstoneConcurrencyStore()
	mgr := &local{cubeboxManger: store}
	ctx := context.Background()

	attemptA := newCreateAttemptOptsForTest(mgr, snap)
	debris := tombstoneDebrisForTest(snap.ID)
	require.NoError(t, mgr.saveCreateRow(ctx, attemptA, debris))

	attemptB := newCreateAttemptOptsForTest(mgr, snap)
	running := runningForCreateTest(snap.ID)
	require.NoError(t, mgr.saveCreateRow(ctx, attemptB, running))

	// Both restore paths of the failed attempt A arrive late.
	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))
	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))

	row, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	assert.Same(t, running, row, "a late rollback must not overwrite the retry's RUNNING row")
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_RUNNING, row.GetStatus().Get().State())
	assert.Len(t, store.savedRows, 2, "no tombstone may be written once a newer attempt owns the row")
	assert.Empty(t, store.deletedIDs)
}

// Even if a newer RUNNING row was saved directly into the store without going
// through snapshotPausedTombstoneForReplace, pointer-identity comparison must
// still leave it untouched.
func TestLateRollbackDoesNotOverwriteUnregisteredRunningRow(t *testing.T) {
	snap := pausedTombstoneForTest()
	store := newTombstoneConcurrencyStore()
	mgr := &local{cubeboxManger: store}
	ctx := context.Background()

	attemptA := newCreateAttemptOptsForTest(mgr, snap)
	require.NoError(t, mgr.saveCreateRow(ctx, attemptA, tombstoneDebrisForTest(snap.ID)))

	running := runningForCreateTest(snap.ID)
	require.NoError(t, store.Save(ctx, running, cubes.WithNoEvent))

	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))

	row, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	assert.Same(t, running, row)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_RUNNING, row.GetStatus().Get().State())
}

// If the retry succeeded and the user subsequently paused the sandbox again
// (producing a newer PAUSED tombstone), a stale rollback from before the retry
// must not overwrite that newer PAUSED snapshot with the older one.
func TestLateRollbackDoesNotOverwriteNewerPausedTombstone(t *testing.T) {
	snapOld := pausedTombstoneForTest()
	snapOld.Annotations = map[string]string{"snap": "old"}
	store := newTombstoneConcurrencyStore()
	mgr := &local{cubeboxManger: store}
	ctx := context.Background()

	attemptA := newCreateAttemptOptsForTest(mgr, snapOld)

	newerPaused := pausedTombstoneForTest()
	newerPaused.Annotations = map[string]string{"snap": "new"}
	require.NoError(t, store.Save(ctx, newerPaused, cubes.WithNoEvent))

	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))

	row, err := store.Get(ctx, snapOld.ID)
	require.NoError(t, err)
	assert.Same(t, newerPaused, row, "must not clobber a newer PAUSED row with a stale snapshot")
	assert.Equal(t, "new", row.Annotations["snap"])
}

type spyDestroyStepFlow struct {
	id           string
	destroyCalls int
}

func (s *spyDestroyStepFlow) ID() string                                     { return s.id }
func (s *spyDestroyStepFlow) Init(context.Context, *workflow.InitInfo) error { return nil }
func (s *spyDestroyStepFlow) Create(context.Context, *workflow.CreateContext) error {
	return nil
}
func (s *spyDestroyStepFlow) Destroy(context.Context, *workflow.DestroyContext) error {
	s.destroyCalls++
	return nil
}
func (s *spyDestroyStepFlow) CleanUp(context.Context, *workflow.CleanContext) error { return nil }

// A late rollback must skip both the cubebox Destroy body AND subsequent
// workflow steps (storage, network, cgroup, cube-sandbox-store), otherwise
// returning nil from step 1 would let step 2 tear down the retry's live
// network and disk.
func TestLateRollbackDestroyAbortsEntireWorkflowWhenRetryOwnsRow(t *testing.T) {
	snap := pausedTombstoneForTest()
	store := newTombstoneConcurrencyStore()
	mgr := &local{cubeboxManger: store}
	ctx := namespaces.WithNamespace(
		CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: "req-rollback"}),
		"default",
	)

	attemptA := newCreateAttemptOptsForTest(mgr, snap)
	require.NoError(t, mgr.saveCreateRow(ctx, attemptA, tombstoneDebrisForTest(snap.ID)))

	attemptB := newCreateAttemptOptsForTest(mgr, snap)
	running := runningForCreateTest(snap.ID)
	require.NoError(t, mgr.saveCreateRow(ctx, attemptB, running))

	spyStep2 := &spyDestroyStepFlow{id: "storage-network-spy"}
	engine := &workflow.Engine{}
	engine.AddFlow("destroy", &workflow.Workflow{
		Name:    "destroy",
		Limiter: semaphore.NewLimiter(4),
		Steps: []*workflow.Step{
			{Name: "cubebox", Actions: []workflow.Flow{mgr}},
			{Name: "resources", Actions: []workflow.Flow{spyStep2}},
		},
	})

	opts := rollbackOptsForTest(snap.ID, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard)
	require.NoError(t, engine.Destroy(ctx, opts))
	assert.True(t, opts.StalePausedTombstoneRollback)
	assert.Zero(t, spyStep2.destroyCalls,
		"remaining destroy steps must not tear down the live retry's storage/network")

	row, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	assert.Same(t, running, row, "the stale rollback must not delete or mutate the retry's row")
	assert.Empty(t, store.deletedIDs)
	assert.Len(t, store.savedRows, 2)
}

// Even in the window where the retry has armed and deleted the PAUSED row but
// has not yet called saveCreateRow, a late rollback from the previous attempt
// must see the advanced epoch and refuse to interfere.
func TestLateRollbackSkipsWhileRetryIsMidCreateBeforeFirstSave(t *testing.T) {
	snap := pausedTombstoneForTest()
	store := newTombstoneConcurrencyStore()
	store.addRow(snap)
	mgr := &local{cubeboxManger: store}
	ctx := context.Background()

	attemptA := newCreateAttemptOptsForTest(mgr, snap)
	require.NoError(t, store.Delete(ctx, &cubes.DeleteOption{CubeboxID: snap.ID}))
	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))

	restored, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	// Retry B begins and deletes the restored PAUSED row, before saving its new row.
	_ = newCreateAttemptOptsForTest(mgr, restored)
	require.NoError(t, store.Delete(ctx, &cubes.DeleteOption{CubeboxID: snap.ID}))

	// Late rollback from attempt A arrives while the store slot is legitimately empty for B.
	opts := rollbackOptsForTest(snap.ID, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard)
	require.NoError(t, mgr.Destroy(ctx, opts))
	assert.True(t, opts.StalePausedTombstoneRollback)

	got, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "attempt A must not write a PAUSED tombstone into retry B's empty slot")
}

// If the user explicitly deletes the paused sandbox after a failed resume, a
// late rollback from that failed resume must not resurrect it.
func TestLateRollbackDoesNotResurrectUserDeletedSandbox(t *testing.T) {
	snap := pausedTombstoneForTest()
	store := newTombstoneConcurrencyStore()
	mgr := &local{cubeboxManger: store}
	ctx := context.Background()

	attemptA := newCreateAttemptOptsForTest(mgr, snap)
	mgr.advancePausedTombstoneEpochIfTracked(snap.ID)

	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))
	got, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "a stale rollback must not resurrect a user-deleted sandbox")
	assert.Empty(t, store.savedRows)
}

// Guard the original behaviour: with no newer attempt, the failed create's own
// debris must still be replaced by a clean PAUSED row so a retry is admitted as
// a same-node resume.
func TestRollbackStillRestoresItsOwnDebris(t *testing.T) {
	snap := pausedTombstoneForTest()
	store := newTombstoneConcurrencyStore()
	mgr := &local{cubeboxManger: store}
	ctx := context.Background()

	attemptA := newCreateAttemptOptsForTest(mgr, snap)
	debris := tombstoneDebrisForTest(snap.ID)
	require.NoError(t, mgr.saveCreateRow(ctx, attemptA, debris))
	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))

	row, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	require.NotSame(t, debris, row)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, row.GetStatus().Get().State())
	assert.Nil(t, row.UserMarkDeletedTime)
	assert.False(t, row.GetStatus().Get().Removing)
}

// A missing store row when no newer attempt has started is restored cleanly.
func TestRollbackRestoresWhenStoreRowIsGone(t *testing.T) {
	snap := pausedTombstoneForTest()
	store := newTombstoneConcurrencyStore()
	mgr := &local{cubeboxManger: store}
	ctx := context.Background()

	attemptA := newCreateAttemptOptsForTest(mgr, snap)
	require.NoError(t, mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard))
	row, err := store.Get(ctx, snap.ID)
	require.NoError(t, err)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, row.GetStatus().Get().State())
}

// Exercise the real interleaving under -race: a late restore and the next
// attempt's arm + first store write contend for the same sandbox ID. Once the
// retry saves its RUNNING row, the final stored state must always be RUNNING.
func TestTombstoneRestoreAndRetrySaveAreRaceFree(t *testing.T) {
	for round := 0; round < 50; round++ {
		snap := pausedTombstoneForTest()
		store := newTombstoneConcurrencyStore()
		mgr := &local{cubeboxManger: store}
		ctx := context.Background()

		attemptA := newCreateAttemptOptsForTest(mgr, snap)
		require.NoError(t, mgr.saveCreateRow(ctx, attemptA, tombstoneDebrisForTest(snap.ID)))
		running := runningForCreateTest(snap.ID)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = mgr.restorePausedTombstone(ctx, attemptA.PausedTombstone, attemptA.PausedTombstoneGuard)
		}()
		go func() {
			defer wg.Done()
			attemptB := newCreateAttemptOptsForTest(mgr, snap)
			_ = mgr.saveCreateRow(ctx, attemptB, running)
		}()
		wg.Wait()

		row, err := store.Get(ctx, snap.ID)
		require.NoError(t, err)
		require.NotNil(t, row)
		// Whichever goroutine acquired pausedTombstoneMu first:
		// - If A ran first, A wrote PAUSED and then B overwrote it with RUNNING.
		// - If B ran first, B bumped the epoch and wrote RUNNING, so A skipped.
		// In BOTH orders the converged row MUST be B's RUNNING row!
		assert.Same(t, running, row, "retry B's RUNNING row must win in every interleaving")
		assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_RUNNING, row.GetStatus().Get().State())
	}
}
