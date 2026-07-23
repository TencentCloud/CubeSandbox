package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

type guestMetricsEpochSyncerFake struct {
	err     error
	syncIDs []string
}

func (f *guestMetricsEpochSyncerFake) SyncByID(_ context.Context, id string, _ ...cubes.UpdateCubeboxOpt) error {
	f.syncIDs = append(f.syncIDs, id)
	return f.err
}

func TestBeginAndPersistGuestMetricsEpochPersistsFreshAndRollback(t *testing.T) {
	syncer := &guestMetricsEpochSyncerFake{}
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox"}}
	freshAt := time.Date(2026, time.July, 18, 1, 0, 0, 0, time.UTC)
	require.NoError(t, beginAndPersistGuestMetricsEpoch(
		context.Background(), syncer, cb, cubeboxstore.GuestMetricsEpochFreshCreate, freshAt,
	))
	require.Equal(t, []string{"sandbox"}, syncer.syncIDs)
	require.Equal(t, uint64(1), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, cubeboxstore.GuestMetricsEpochPending, cb.GuestMetricsEpoch.State)

	rollbackAt := freshAt.Add(time.Minute)
	require.NoError(t, beginAndPersistGuestMetricsEpoch(
		context.Background(), syncer, cb, cubeboxstore.GuestMetricsEpochRollback, rollbackAt,
	))
	require.Equal(t, uint64(2), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, cubeboxstore.GuestMetricsEpochRollback, cb.GuestMetricsEpoch.Reason)
	require.Equal(t, rollbackAt, cb.GuestMetricsEpoch.StartedAt)
}

func TestBeginAndPersistGuestMetricsEpochRestoresPreviousEpochOnPersistenceFailure(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 1, 0, 0, 0, time.UTC)
	previous := &cubeboxstore.GuestMetricsEpoch{
		Generation: 1,
		StartedAt:  startedAt,
		Reason:     cubeboxstore.GuestMetricsEpochFreshCreate,
		State:      cubeboxstore.GuestMetricsEpochPending,
	}
	cb := &cubeboxstore.CubeBox{
		Metadata:          cubeboxstore.Metadata{ID: "sandbox"},
		GuestMetricsEpoch: previous,
	}
	syncer := &guestMetricsEpochSyncerFake{err: errors.New("metadata store unavailable")}

	err := beginAndPersistGuestMetricsEpoch(
		context.Background(), syncer, cb, cubeboxstore.GuestMetricsEpochRollback, startedAt.Add(time.Minute),
	)
	require.ErrorContains(t, err, "persist guest metrics epoch")
	require.Equal(t, previous, cb.GuestMetricsEpoch)
	require.Equal(t, uint64(1), cb.GuestMetricsEpoch.Generation)
}

func TestBeginFreshGuestMetricsEpochBestEffortKeepsPendingStateOnPersistenceFailure(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 1, 0, 0, 0, time.UTC)
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox"}}
	syncer := &guestMetricsEpochSyncerFake{err: errors.New("metadata store unavailable")}

	err := beginFreshGuestMetricsEpochBestEffort(context.Background(), syncer, cb, startedAt)
	require.ErrorContains(t, err, "persist guest metrics epoch")
	require.Equal(t, uint64(1), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, cubeboxstore.GuestMetricsEpochFreshCreate, cb.GuestMetricsEpoch.Reason)
	require.Equal(t, cubeboxstore.GuestMetricsEpochPending, cb.GuestMetricsEpoch.State)
}

func TestPrepareAndPersistRollbackGuestMetricsEpochDoesNotAdvanceBindings(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 2, 0, 0, 0, time.UTC)
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{
		ID:     "sandbox",
		Labels: map[string]string{constants.MasterAnnotationRuntimeSnapshotID: "previous"},
	}}

	require.NoError(t, prepareAndPersistRollbackGuestMetricsEpoch(
		context.Background(), &guestMetricsEpochSyncerFake{}, cb, startedAt,
	))
	require.Equal(t, uint64(1), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, cubeboxstore.GuestMetricsEpochRollback, cb.GuestMetricsEpoch.Reason)
	require.Equal(t, cubeboxstore.GuestMetricsEpochPrepared, cb.GuestMetricsEpoch.State)
	require.Equal(t, "previous", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
}

func TestPrepareAndPersistRollbackGuestMetricsEpochRestoresPreviousEpochOnFailure(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 2, 0, 0, 0, time.UTC)
	previousEpoch := &cubeboxstore.GuestMetricsEpoch{
		Generation: 1,
		StartedAt:  startedAt.Add(-time.Minute),
		Reason:     cubeboxstore.GuestMetricsEpochFreshCreate,
		State:      cubeboxstore.GuestMetricsEpochPending,
	}
	previousLabels := map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID:        "previous-snapshot",
		constants.MasterAnnotationRuntimeRestoreSnapshotID: "previous-restore",
	}
	cb := &cubeboxstore.CubeBox{
		Metadata:          cubeboxstore.Metadata{ID: "sandbox", Labels: previousLabels},
		GuestMetricsEpoch: previousEpoch,
	}

	err := prepareAndPersistRollbackGuestMetricsEpoch(
		context.Background(),
		&guestMetricsEpochSyncerFake{err: errors.New("metadata store unavailable")},
		cb,
		startedAt,
	)
	require.ErrorContains(t, err, "persist guest metrics epoch")
	require.Equal(t, previousEpoch, cb.GuestMetricsEpoch)
	require.Equal(t, previousLabels, cb.Labels)
}

func TestActivateAndPersistRollbackGuestMetricsEpochAdvancesBindings(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 2, 0, 0, 0, time.UTC)
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{
		ID:     "sandbox",
		Labels: map[string]string{constants.MasterAnnotationRuntimeSnapshotID: "previous"},
	}}
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(startedAt))
	syncer := &guestMetricsEpochSyncerFake{}

	require.NoError(t, activateAndPersistRollbackGuestMetricsEpoch(context.Background(), syncer, cb, "snapshot", startedAt.Add(time.Minute)))
	require.Equal(t, cubeboxstore.GuestMetricsEpochPending, cb.GuestMetricsEpoch.State)
	require.Equal(t, "snapshot", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	require.Equal(t, "snapshot", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID])
	require.Equal(t, []string{"sandbox"}, syncer.syncIDs)
}

func TestActivateRollbackGuestMetricsEpochRestoresPreparedStateAndBindingsOnPersistenceFailure(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 2, 0, 0, 0, time.UTC)
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{
		ID: "sandbox",
		Labels: map[string]string{
			constants.MasterAnnotationRuntimeSnapshotID:        "previous",
			constants.MasterAnnotationRuntimeRestoreSnapshotID: "previous-restore",
		},
	}}
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(startedAt))
	previousEpoch := cb.GuestMetricsEpochCopy()
	previousLabels := deepCopyStringMap(cb.Labels)
	syncer := &guestMetricsEpochSyncerFake{err: errors.New("metadata store unavailable")}

	err := activateAndPersistRollbackGuestMetricsEpoch(context.Background(), syncer, cb, "snapshot", startedAt.Add(time.Minute))
	require.ErrorContains(t, err, "persist activated guest metrics epoch")
	require.Equal(t, previousEpoch, cb.GuestMetricsEpochCopy())
	require.Equal(t, previousLabels, cb.Labels)
}
