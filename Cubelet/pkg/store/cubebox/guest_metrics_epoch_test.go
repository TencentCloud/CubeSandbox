package cubebox

import (
	"sync"
	"testing"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/stretchr/testify/require"
)

func TestBeginGuestMetricsEpochCreatesPendingGenerations(t *testing.T) {
	cb := &CubeBox{}
	freshAt := time.Date(2026, time.July, 18, 1, 2, 3, 4, time.FixedZone("test", 8*60*60))
	require.NoError(t, cb.BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, freshAt))
	require.Equal(t, uint64(1), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, freshAt.UTC(), cb.GuestMetricsEpoch.StartedAt)
	require.Equal(t, GuestMetricsEpochFreshCreate, cb.GuestMetricsEpoch.Reason)
	require.Equal(t, GuestMetricsEpochPending, cb.GuestMetricsEpoch.State)
	require.Nil(t, cb.GuestMetricsEpoch.ReadyAt)

	rollbackAt := freshAt.Add(time.Minute)
	require.NoError(t, cb.BeginGuestMetricsEpoch(GuestMetricsEpochRollback, rollbackAt))
	require.Equal(t, uint64(2), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, rollbackAt.UTC(), cb.GuestMetricsEpoch.StartedAt)
	require.Equal(t, GuestMetricsEpochRollback, cb.GuestMetricsEpoch.Reason)
}

func TestPrepareRollbackGuestMetricsEpochRequiresActivationBeforeBaseline(t *testing.T) {
	cb := &CubeBox{}
	startedAt := time.Date(2026, time.July, 18, 1, 2, 3, 4, time.UTC)
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(startedAt))
	require.Equal(t, uint64(1), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, GuestMetricsEpochRollback, cb.GuestMetricsEpoch.Reason)
	require.Equal(t, GuestMetricsEpochPrepared, cb.GuestMetricsEpoch.State)
	require.Error(t, cb.ReadyGuestMetricsEpoch(1, GuestMetricsEpochBaseline{ContainerID: "sandbox"}, startedAt.Add(time.Second)))

	require.NoError(t, cb.ActivatePreparedGuestMetricsEpoch(1))
	require.Equal(t, GuestMetricsEpochPending, cb.GuestMetricsEpoch.State)
	require.Error(t, cb.ActivatePreparedGuestMetricsEpoch(1))
}

func TestBeginGuestMetricsEpochRejectsInvalidInput(t *testing.T) {
	require.Error(t, (*CubeBox)(nil).BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, time.Now()))
	require.Error(t, (&CubeBox{}).BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, time.Time{}))
	require.Error(t, (&CubeBox{}).BeginGuestMetricsEpoch("pause", time.Now()))
	require.Error(t, (&CubeBox{GuestMetricsEpoch: &GuestMetricsEpoch{Generation: ^uint64(0)}}).
		BeginGuestMetricsEpoch(GuestMetricsEpochRollback, time.Now()))
	require.Error(t, (*CubeBox)(nil).PrepareRollbackGuestMetricsEpoch(time.Now()))
	require.Error(t, (&CubeBox{}).PrepareRollbackGuestMetricsEpoch(time.Time{}))
}

func TestBeginFreshGuestMetricsEpochIfMissingIsIdempotent(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 1, 2, 3, 4, time.UTC)
	cb := &CubeBox{}

	created, err := cb.BeginFreshGuestMetricsEpochIfMissing(startedAt)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, &GuestMetricsEpoch{
		Generation: 1,
		StartedAt:  startedAt,
		Reason:     GuestMetricsEpochFreshCreate,
		State:      GuestMetricsEpochPending,
	}, cb.GuestMetricsEpochCopy())

	created, err = cb.BeginFreshGuestMetricsEpochIfMissing(startedAt.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, startedAt, cb.GuestMetricsEpochCopy().StartedAt)
}

func TestBeginFreshGuestMetricsEpochIfMissingRejectsInvalidInput(t *testing.T) {
	_, err := (*CubeBox)(nil).BeginFreshGuestMetricsEpochIfMissing(time.Now())
	require.Error(t, err)
	_, err = (&CubeBox{}).BeginFreshGuestMetricsEpochIfMissing(time.Time{})
	require.Error(t, err)
}

func TestGuestMetricsEpochPersistsReadyBaselineAndDegradedState(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 1, 2, 3, 0, time.UTC)
	readyAt := startedAt.Add(time.Second)
	cb := &CubeBox{}
	require.NoError(t, cb.BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, startedAt))
	baseline := GuestMetricsEpochBaseline{
		ContainerID:              "sandbox",
		CPUUsageTotalNS:          100,
		CPUUserTotalNS:           60,
		CPUSystemTotalNS:         40,
		CPUThrottledTotalNS:      10,
		CPUPeriodsTotal:          8,
		CPUThrottledPeriodsTotal: 2,
		MemoryFailuresTotal:      3,
	}
	require.NoError(t, cb.ReadyGuestMetricsEpoch(1, baseline, readyAt))

	epoch := cb.GuestMetricsEpochCopy()
	require.Equal(t, GuestMetricsEpochReady, epoch.State)
	require.Equal(t, readyAt, *epoch.ReadyAt)
	require.Equal(t, baseline, *epoch.Baseline)
	require.NoError(t, cb.DegradeGuestMetricsEpoch(1))
	require.Equal(t, GuestMetricsEpochDegraded, cb.GuestMetricsEpochCopy().State)
}

func TestGuestMetricsEpochRejectsInvalidStateTransitions(t *testing.T) {
	cb := &CubeBox{}
	baseline := GuestMetricsEpochBaseline{ContainerID: "sandbox"}
	require.Error(t, cb.ReadyGuestMetricsEpoch(1, baseline, time.Now()))
	require.NoError(t, cb.BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, time.Now()))
	require.Error(t, cb.ReadyGuestMetricsEpoch(2, baseline, time.Now()))
	require.Error(t, cb.ReadyGuestMetricsEpoch(1, GuestMetricsEpochBaseline{}, time.Now()))
	require.NoError(t, cb.ReadyGuestMetricsEpoch(1, baseline, time.Now()))
	require.Error(t, cb.ReadyGuestMetricsEpoch(1, baseline, time.Now()))
	require.Error(t, cb.DegradeGuestMetricsEpoch(2))
}

func TestGuestMetricsEpochJSONRoundTripAndLegacyCompatibility(t *testing.T) {
	startedAt := time.Date(2026, time.July, 18, 1, 2, 3, 4, time.UTC)
	cb := &CubeBox{Metadata: Metadata{ID: "sandbox"}}
	require.NoError(t, cb.BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, startedAt))
	encoded, err := jsoniter.Marshal(cb)
	require.NoError(t, err)

	var decoded CubeBox
	require.NoError(t, jsoniter.Unmarshal(encoded, &decoded))
	require.Equal(t, cb.GuestMetricsEpoch, decoded.GuestMetricsEpoch)

	var legacy CubeBox
	require.NoError(t, jsoniter.Unmarshal([]byte(`{"ID":"legacy"}`), &legacy))
	require.Nil(t, legacy.GuestMetricsEpoch)
}

func TestCubeBoxDeepCopyDoesNotShareGuestMetricsEpoch(t *testing.T) {
	readyAt := time.Date(2026, time.July, 18, 2, 0, 0, 0, time.UTC)
	cb := &CubeBox{GuestMetricsEpoch: &GuestMetricsEpoch{
		Generation: 1,
		StartedAt:  readyAt.Add(-time.Minute),
		ReadyAt:    &readyAt,
		Reason:     GuestMetricsEpochFreshCreate,
		State:      GuestMetricsEpochReady,
	}}

	copied := cb.DeepCopy()
	require.NotSame(t, cb.GuestMetricsEpoch, copied.GuestMetricsEpoch)
	require.NotSame(t, cb.GuestMetricsEpoch.ReadyAt, copied.GuestMetricsEpoch.ReadyAt)
	copied.GuestMetricsEpoch.Generation = 2
	*copied.GuestMetricsEpoch.ReadyAt = readyAt.Add(time.Minute)
	require.Equal(t, uint64(1), cb.GuestMetricsEpoch.Generation)
	require.Equal(t, readyAt, *cb.GuestMetricsEpoch.ReadyAt)
}

func TestGuestMetricsEpochConcurrentMutationAndCopy(t *testing.T) {
	cb := &CubeBox{}
	startedAt := time.Date(2026, time.July, 18, 3, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(offset int) {
			defer wg.Done()
			if err := cb.BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, startedAt.Add(time.Duration(offset))); err != nil {
				t.Errorf("begin guest metrics epoch: %v", err)
			}
		}(i)
		go func() {
			defer wg.Done()
			_ = cb.DeepCopy()
		}()
	}
	wg.Wait()
	require.Equal(t, uint64(32), cb.GuestMetricsEpochCopy().Generation)
}

func TestCubeBoxDeepCopyWhileLifecycleLockHeld(t *testing.T) {
	cb := &CubeBox{}
	require.NoError(t, cb.BeginGuestMetricsEpoch(GuestMetricsEpochFreshCreate, time.Now()))

	cb.Lock()
	copied := cb.DeepCopy()
	cb.Unlock()

	require.NotNil(t, copied.GuestMetricsEpoch)
	require.Equal(t, uint64(1), copied.GuestMetricsEpoch.Generation)
}
