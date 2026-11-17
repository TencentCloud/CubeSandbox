package cubebox

import (
	"context"
	"fmt"
	"time"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

type guestMetricsEpochSyncer interface {
	SyncByID(context.Context, string, ...cubes.UpdateCubeboxOpt) error
}

func beginAndPersistGuestMetricsEpoch(
	ctx context.Context,
	syncer guestMetricsEpochSyncer,
	cb *cubeboxstore.CubeBox,
	reason cubeboxstore.GuestMetricsEpochReason,
	startedAt time.Time,
) error {
	if syncer == nil {
		return fmt.Errorf("guest metrics epoch syncer is required")
	}
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	previous := cb.GuestMetricsEpochCopy()
	if err := cb.BeginGuestMetricsEpoch(reason, startedAt); err != nil {
		return err
	}
	if err := syncer.SyncByID(ctx, cb.ID); err != nil {
		cb.RestoreGuestMetricsEpoch(previous)
		return fmt.Errorf("persist guest metrics epoch for %s: %w", cb.ID, err)
	}
	return nil
}

func beginFreshGuestMetricsEpochBestEffort(
	ctx context.Context,
	syncer guestMetricsEpochSyncer,
	cb *cubeboxstore.CubeBox,
	startedAt time.Time,
) error {
	err := beginAndPersistGuestMetricsEpoch(
		ctx, syncer, cb, cubeboxstore.GuestMetricsEpochFreshCreate, startedAt,
	)
	if err == nil {
		return nil
	}
	if cb == nil {
		return err
	}
	if beginErr := cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochFreshCreate, startedAt); beginErr != nil {
		return fmt.Errorf("%v; keep fresh guest metrics pending in memory: %w", err, beginErr)
	}
	return err
}

func prepareAndPersistRollbackGuestMetricsEpoch(
	ctx context.Context,
	syncer guestMetricsEpochSyncer,
	cb *cubeboxstore.CubeBox,
	startedAt time.Time,
) error {
	if syncer == nil {
		return fmt.Errorf("guest metrics epoch syncer is required")
	}
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	previous := cb.GuestMetricsEpochCopy()
	if err := cb.PrepareRollbackGuestMetricsEpoch(startedAt); err != nil {
		return err
	}
	if err := syncer.SyncByID(ctx, cb.ID); err != nil {
		cb.RestoreGuestMetricsEpoch(previous)
		return fmt.Errorf("persist guest metrics epoch for %s: %w", cb.ID, err)
	}
	return nil
}

func activateAndPersistRollbackGuestMetricsEpoch(
	ctx context.Context,
	syncer guestMetricsEpochSyncer,
	cb *cubeboxstore.CubeBox,
	snapshotID string,
	activatedAt time.Time,
) error {
	if syncer == nil {
		return fmt.Errorf("guest metrics epoch syncer is required")
	}
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	epoch := cb.GuestMetricsEpochCopy()
	if epoch == nil {
		return fmt.Errorf("prepared guest metrics epoch is required")
	}
	previousEpoch := epoch.DeepCopy()
	if err := cb.ActivatePreparedGuestMetricsEpoch(epoch.Generation); err != nil {
		return err
	}
	setRuntimeSnapshotBinding(cb, snapshotID)
	setRuntimeRestoreBase(cb, snapshotID)
	if err := syncer.SyncByID(ctx, cb.ID); err != nil {
		// The VM has already been restored to snapshotID, so the persisted
		// lineage is stale either way. Do NOT put the pre-rollback base back:
		// the next Commit would treat it as a valid soft-dirty/pagemap_anon
		// base and drop pages written since the rollback. Clearing forces a
		// correct full capture.
		//
		// The clear is in-memory only and the durable row still holds the
		// pre-rollback base. If this process dies before the next successful
		// SyncByID, recovery reads the "prepared" epoch this rollback left
		// behind and treats the lineage as untrusted (see
		// ReconcileRecoveredMemoryLineage). That is what keeps the stale
		// durable value from being reused; do not rely on the in-memory clear
		// alone.
		cb.RestoreGuestMetricsEpochIfCurrent(epoch.Generation, cubeboxstore.GuestMetricsEpochPending, previousEpoch)
		cb.ClearRuntimeLineage()
		return fmt.Errorf("persist activated guest metrics epoch for %s: %w", cb.ID, err)
	}
	return nil
}
