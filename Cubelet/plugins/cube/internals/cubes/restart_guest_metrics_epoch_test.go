package cubes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func TestRecoverAllCubeboxPreservesGuestMetricsEpoch(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 3, 4, 5, 0, time.UTC)
	cb := testRecoveryCubeBox(startedAt)
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochFreshCreate, startedAt))
	got := recoverCubeBoxThroughDB(t, cb)

	require.NotNil(t, got.GuestMetricsEpoch)
	require.Equal(t, cb.GuestMetricsEpoch, got.GuestMetricsEpoch)
}

func TestRecoverAllCubeboxPreservesReadyGuestMetricsBaseline(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 3, 4, 5, 0, time.UTC)
	readyAt := startedAt.Add(time.Second)
	cb := testRecoveryCubeBox(startedAt)
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochFreshCreate, startedAt))
	require.NoError(t, cb.ReadyGuestMetricsEpoch(1, cubeboxstore.GuestMetricsEpochBaseline{
		ContainerID:         cb.ID,
		CPUUsageTotalNS:     100,
		CPUUserTotalNS:      60,
		CPUSystemTotalNS:    40,
		MemoryFailuresTotal: 3,
	}, readyAt))
	got := recoverCubeBoxThroughDB(t, cb)

	require.Equal(t, cubeboxstore.GuestMetricsEpochReady, got.GuestMetricsEpoch.State)
	require.Equal(t, readyAt, *got.GuestMetricsEpoch.ReadyAt)
	require.Equal(t, cb.GuestMetricsEpoch.Baseline, got.GuestMetricsEpoch.Baseline)
}

func TestRecoverAllCubeboxPreservesPreparedRollbackEpoch(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 3, 4, 5, 0, time.UTC)
	cb := testRecoveryCubeBox(startedAt)
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(startedAt))
	got := recoverCubeBoxThroughDB(t, cb)

	require.NotNil(t, got.GuestMetricsEpoch)
	require.Equal(t, cubeboxstore.GuestMetricsEpochPrepared, got.GuestMetricsEpoch.State)
	require.Equal(t, cubeboxstore.GuestMetricsEpochRollback, got.GuestMetricsEpoch.Reason)
}

func testRecoveryCubeBox(startedAt time.Time) *cubeboxstore.CubeBox {
	deletedAt := startedAt.Add(time.Minute)
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox", CreatedAt: startedAt.UnixNano()}}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "sandbox", DeletedTime: &deletedAt},
		IsPod:    true,
	})
	return cb
}

func recoverCubeBoxThroughDB(t *testing.T, cb *cubeboxstore.CubeBox) *cubeboxstore.CubeBox {
	t.Helper()
	basePath := t.TempDir()
	db, err := utils.NewCubeStoreExt(basePath, "meta.db", 1, nil)
	require.NoError(t, err)
	store := cubeboxstore.NewStore(db)
	store.Add(cb)
	require.NoError(t, store.Sync(cb.ID))
	require.NoError(t, db.Close())

	recoveredDB, err := utils.NewCubeStoreExt(basePath, "meta.db", 1, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, recoveredDB.Close()) })

	recovered := &local{
		db:           recoveredDB,
		cubeboxStore: cubeboxstore.NewStore(recoveredDB),
	}
	require.NoError(t, recovered.RecoverAllCubebox(context.Background(), func(context.Context, *cubeboxstore.CubeBox) error { return nil }))
	got, err := recovered.Get(context.Background(), cb.ID)
	require.NoError(t, err)
	return got
}
