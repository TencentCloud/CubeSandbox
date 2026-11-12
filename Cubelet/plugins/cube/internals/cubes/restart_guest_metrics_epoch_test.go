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

// Recovery must not invent a memory base from create-request annotations: an
// empty lineage means "no base", so the next Commit/Pause takes a full capture.
// Recovery never leaves a legacy row nil, because "no base" has to be explicit
// once the field exists (see ReconcileRecoveredMemoryLineage).
func TestRecoverAllCubeboxDoesNotDeriveLineageFromMetadata(t *testing.T) {
	startedAt := time.Date(2026, time.October, 25, 1, 0, 0, 0, time.UTC)
	legacy := testRecoveryCubeBox(startedAt)
	legacy.AddLabels(map[string]string{
		// runtime.snapshot.id is the Commit binding, not a restore base, so it
		// must not be backfilled even though it is a legacy label.
		"cube.master.runtime.snapshot.id": "snap-legacy",
	})
	legacy.AddAnnotations(map[string]string{
		"cube.master.appsnapshot.template.id": "tpl-T",
	})
	got := recoverCubeBoxThroughDB(t, legacy)
	require.NotNil(t, got.MemoryLineage)
	require.Equal(t, "", got.RuntimeBaseID())
	require.Equal(t, "", got.RestoreBaseID())
	require.Equal(t, "", got.LaunchAncestorID())
}

// legacyRow is a row written before MemoryLineage existed: nil lineage, plus
// whatever metadata that build left behind.
func legacyRow(labels map[string]string) *cubeboxstore.CubeBox {
	cb := testRecoveryCubeBox(time.Date(2026, time.October, 25, 2, 0, 0, 0, time.UTC))
	if labels != nil {
		cb.AddLabels(labels)
	}
	return cb
}

const restoreBaseLabel = "cube.master.runtime.restore.snapshot.id"

// A legacy row that really was restored from a snapshot can be upgraded without
// paying for a full capture: the restore binding is backfilled, and only that
// one, so the next Commit takes the pagemap_anon path.
func TestRecoveryBackfillsRestoreOnlyForLegacyRow(t *testing.T) {
	got := recoverCubeBoxThroughDB(t, legacyRow(map[string]string{
		restoreBaseLabel:                      "snap-restored",
		"cube.master.runtime.snapshot.id":     "snap-committed",
		"cube.master.appsnapshot.template.id": "tpl-T",
	}))
	require.NotNil(t, got.MemoryLineage)
	require.Equal(t, "snap-restored", got.RestoreBaseID())
	require.Equal(t, "", got.RuntimeBaseID(), "soft-dirty bitmap is unknowable across a restart")
	require.Equal(t, "", got.LaunchAncestorID(), "no trustworthy legacy source")
}

// A restarted sandbox's restore label names memory this VM no longer has.
func TestRecoverySkipsBackfillAfterRestart(t *testing.T) {
	cb := legacyRow(map[string]string{restoreBaseLabel: "snap-pre-restart"})
	cb.LastRestartAt = time.Date(2026, time.October, 25, 2, 30, 0, 0, time.UTC).UnixNano()
	got := recoverCubeBoxThroughDB(t, cb)
	require.NotNil(t, got.MemoryLineage)
	require.Equal(t, "", got.RestoreBaseID())
	require.Equal(t, "", got.RuntimeBaseID())
	require.Equal(t, "", got.LaunchAncestorID())
}

func TestRecoveryLeavesLegacyRowWithoutLabelEmpty(t *testing.T) {
	got := recoverCubeBoxThroughDB(t, legacyRow(nil))
	require.NotNil(t, got.MemoryLineage, "nil must not leak past recovery")
	require.Equal(t, "", got.RestoreBaseID())
}

// A row written by this build already carries authoritative bindings.
func TestRecoveryDoesNotTouchRowsThatAlreadyHaveLineage(t *testing.T) {
	cb := testRecoveryCubeBox(time.Date(2026, time.October, 25, 3, 0, 0, 0, time.UTC))
	cb.RestoreMemoryLineage(&cubeboxstore.MemoryLineage{})
	cb.BindRuntimeBase("snap-runtime")
	cb.BindRestoreBase("snap-restore")
	cb.BindLaunchAncestorOnce("tpl-T")
	want := cb.MemoryLineageCopy()

	got := recoverCubeBoxThroughDB(t, cb)
	require.Equal(t, want, got.MemoryLineageCopy())
}

func TestRecoveryIgnoresBlankAndSentinelRestoreLabels(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"whitespace": "   ",
		"sentinel":   "invalid-runtime-restore-base",
	}
	for name, label := range cases {
		t.Run(name, func(t *testing.T) {
			got := recoverCubeBoxThroughDB(t, legacyRow(map[string]string{restoreBaseLabel: label}))
			require.NotNil(t, got.MemoryLineage)
			require.Equal(t, "", got.RestoreBaseID())
		})
	}
}

func TestRecoveryIgnoresLegacyAnnotations(t *testing.T) {
	cb := legacyRow(nil)
	cb.AddAnnotations(map[string]string{
		restoreBaseLabel:                      "snap-from-annotation",
		"cube.master.appsnapshot.template.id": "tpl-T",
	})
	got := recoverCubeBoxThroughDB(t, cb)
	require.NotNil(t, got.MemoryLineage)
	require.Equal(t, "", got.RestoreBaseID(), "annotations are user controllable")
	require.Equal(t, "", got.LaunchAncestorID())
}

// A rollback that reached "prepared" but never activated leaves a durable
// lineage pointing at the pre-rollback image. Recovery must not trust it.
func TestRecoveryDropsBasesWhenRollbackOutcomeIsUnknown(t *testing.T) {
	cb := testRecoveryCubeBox(time.Date(2026, time.October, 25, 4, 0, 0, 0, time.UTC))
	cb.BindLaunchAncestorOnce("tpl-T")
	cb.BindRuntimeBase("snap-pre-rollback")
	cb.BindRestoreBase("snap-pre-rollback")
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(
		time.Date(2026, time.October, 25, 4, 5, 0, 0, time.UTC)))

	got := recoverCubeBoxThroughDB(t, cb)
	require.Equal(t, "", got.RuntimeBaseID())
	require.Equal(t, "", got.RestoreBaseID())
	require.Equal(t, "tpl-T", got.LaunchAncestorID(), "ancestor only names the start image")
}

// The rollback guard wins over the legacy backfill: the label may well name the
// pre-rollback image, which no longer describes the guest.
func TestRecoveryRollbackGuardWinsOverBackfill(t *testing.T) {
	cb := legacyRow(map[string]string{restoreBaseLabel: "snap-pre-rollback"})
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(
		time.Date(2026, time.October, 25, 4, 5, 0, 0, time.UTC)))

	got := recoverCubeBoxThroughDB(t, cb)
	require.NotNil(t, got.MemoryLineage)
	require.Equal(t, "", got.RestoreBaseID())
}

// A successfully activated rollback leaves "pending", which is trustworthy.
func TestRecoveryKeepsBasesAfterActivatedRollback(t *testing.T) {
	cb := testRecoveryCubeBox(time.Date(2026, time.October, 25, 5, 0, 0, 0, time.UTC))
	cb.BindLaunchAncestorOnce("tpl-T")
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(
		time.Date(2026, time.October, 25, 5, 1, 0, 0, time.UTC)))
	require.NoError(t, cb.ActivatePreparedGuestMetricsEpoch(cb.GuestMetricsEpoch.Generation))
	cb.BindRuntimeBase("snap-rollback-target")
	cb.BindRestoreBase("snap-rollback-target")

	got := recoverCubeBoxThroughDB(t, cb)
	require.Equal(t, "snap-rollback-target", got.RuntimeBaseID())
	require.Equal(t, "snap-rollback-target", got.RestoreBaseID())
}

// The backfill survives a durable round-trip once anything syncs the row.
//
// The fixture's container already carries DeletedTime, so RecoverContainer
// returns before touching containerd and the local below needs no client. If
// that early return ever changes, recovery will silently skip this row and the
// RestoreBaseID assertions below (not a panic) are what will fail.
func TestRecoveryBackfillPersistsOnNextSync(t *testing.T) {
	basePath := t.TempDir()
	open := func() (*local, *utils.CubeStore) {
		db, err := utils.NewCubeStoreExt(basePath, "meta.db", 1, nil)
		require.NoError(t, err)
		return &local{db: db, cubeboxStore: cubeboxstore.NewStore(db)}, db
	}

	seed, seedDB := open()
	cb := legacyRow(map[string]string{restoreBaseLabel: "snap-restored"})
	seed.cubeboxStore.Add(cb)
	require.NoError(t, seed.cubeboxStore.Sync(cb.ID))
	require.NoError(t, seedDB.Close())

	// First start: the legacy row is backfilled in memory only.
	recovered, recoveredDB := open()
	require.NoError(t, recovered.RecoverAllCubebox(context.Background(),
		func(context.Context, *cubeboxstore.CubeBox) error { return nil }))
	first, err := recovered.Get(context.Background(), cb.ID)
	require.NoError(t, err)
	require.Equal(t, "snap-restored", first.RestoreBaseID())

	// Any later metadata write persists it.
	require.NoError(t, recovered.SyncByID(context.Background(), cb.ID))
	require.NoError(t, recoveredDB.Close())

	// Second start: read straight from disk, no backfill needed.
	afterRestart, afterDB := open()
	t.Cleanup(func() { require.NoError(t, afterDB.Close()) })
	require.NoError(t, afterRestart.RecoverAllCubebox(context.Background(),
		func(context.Context, *cubeboxstore.CubeBox) error { return nil }))
	final, err := afterRestart.Get(context.Background(), cb.ID)
	require.NoError(t, err)
	require.Equal(t, "snap-restored", final.RestoreBaseID(), "backfill must be durable")
}

// A row written by a path that never sets MemoryLineage (ociRuntime.Create)
// still lands here as nil, and recovery must give it an explicit empty lineage
// rather than leaving nil to mean two different things.
func TestRecoveryConvergesNilLineageFromNonCubecowCreate(t *testing.T) {
	cb := testRecoveryCubeBox(time.Date(2026, time.October, 25, 6, 0, 0, 0, time.UTC))
	require.Nil(t, cb.MemoryLineage, "fixture must start with the pre-MemoryLineage shape")

	got := recoverCubeBoxThroughDB(t, cb)
	require.NotNil(t, got.MemoryLineage)
	require.Equal(t, "", got.RuntimeBaseID())
	require.Equal(t, "", got.RestoreBaseID())
	require.Equal(t, "", got.LaunchAncestorID())
}
