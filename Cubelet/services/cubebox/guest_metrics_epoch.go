package cubebox

import (
	"context"
	"fmt"
	"maps"
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
	previousLabels := copyCubeBoxLabels(cb)
	if err := cb.ActivatePreparedGuestMetricsEpoch(epoch.Generation); err != nil {
		return err
	}
	setRuntimeSnapshotBindingLabels(cb, snapshotID, activatedAt)
	setRuntimeRestoreBaseLabels(cb, snapshotID, activatedAt)
	if err := syncer.SyncByID(ctx, cb.ID); err != nil {
		if cb.RestoreGuestMetricsEpochIfCurrent(epoch.Generation, cubeboxstore.GuestMetricsEpochPending, previousEpoch) {
			restoreCubeBoxLabels(cb, previousLabels)
		}
		return fmt.Errorf("persist activated guest metrics epoch for %s: %w", cb.ID, err)
	}
	return nil
}

func copyCubeBoxLabels(cb *cubeboxstore.CubeBox) map[string]string {
	cb.MetaLock.Lock()
	defer cb.MetaLock.Unlock()
	return maps.Clone(cb.Labels)
}

func restoreCubeBoxLabels(cb *cubeboxstore.CubeBox, labels map[string]string) {
	cb.MetaLock.Lock()
	defer cb.MetaLock.Unlock()
	cb.Labels = maps.Clone(labels)
}
