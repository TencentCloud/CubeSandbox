// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// armPausedTombstone snapshots the local PAUSED row before the create workflow
// runs any step. Arming up front covers failures in earlier workflow steps
// (network, storage, cgroup) before local.Create deletes the PAUSED row, so
// failover preserves the tombstone instead of deleting the paused sandbox.
func (l *local) armPausedTombstone(ctx context.Context, req *cubebox.RunCubeSandboxRequest, opts *workflow.CreateContext) {
	if l == nil || l.cubeboxManger == nil || req == nil || opts == nil {
		return
	}
	if workflow.ReuseFor(opts).CubeboxMeta {
		return
	}
	desired := strings.TrimSpace(req.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID])
	if desired == "" {
		return
	}

	l.pausedTombstoneMu.Lock()
	defer l.pausedTombstoneMu.Unlock()

	sb, err := l.cubeboxManger.Get(ctx, desired)
	if err != nil || sb == nil || sb.SandboxID != desired {
		return
	}
	st := sb.GetStatus()
	if st == nil || st.Get().State() != cubebox.ContainerState_CONTAINER_PAUSED {
		return
	}
	epoch := l.nextPausedTombstoneEpochLocked(desired)
	if opts.SandboxID == "" {
		opts.SandboxID = desired
	}
	opts.PausedTombstone = sb.DeepCopy()
	opts.PausedTombstoneGuard = &workflow.PausedTombstoneGuard{
		Epoch:   epoch,
		OrigRow: sb,
	}
}

// snapshotPausedTombstoneForReplace refreshes the clean PAUSED snapshot right
// before local.Create marks and deletes the old row, and initializes the
// attempt guard when local.Create is driven directly without service.Create.
func (l *local) snapshotPausedTombstoneForReplace(desired string, sb *cubeboxstore.CubeBox, opts *workflow.CreateContext) {
	if l == nil || sb == nil || opts == nil {
		return
	}
	l.pausedTombstoneMu.Lock()
	defer l.pausedTombstoneMu.Unlock()

	opts.PausedTombstone = sb.DeepCopy()
	if opts.PausedTombstoneGuard == nil {
		opts.PausedTombstoneGuard = &workflow.PausedTombstoneGuard{
			Epoch:   l.nextPausedTombstoneEpochLocked(desired),
			OrigRow: sb,
		}
		return
	}
	if opts.PausedTombstoneGuard.Epoch == 0 {
		opts.PausedTombstoneGuard.Epoch = l.nextPausedTombstoneEpochLocked(desired)
	}
	opts.PausedTombstoneGuard.OrigRow = sb
}

// restorePausedTombstone puts the clean PAUSED row of a failed
// Create-from-pause back into the local store, but only when the current store
// slot is absent or still holds a row owned by this failed attempt.
//
// A same-node resume reuses its sandbox ID, and the create flow deletes the
// PAUSED row before saving its replacement. If the create then fails, the node
// would have no local record for that ID: a retry would be classified as a
// cross-node restore, skipping both admitAndReserveResume and
// gatePausedReplace and reopening #2042.
//
// Conversely, if a late rollback runs after a retry has already started or
// stored a newer RUNNING/CREATED/PAUSED row (or after the user deleted the
// sandbox), writing the old PAUSED snapshot unconditionally would clobber that
// newer state. Both the attempt epoch and exact row-pointer ownership are
// checked atomically under pausedTombstoneMu.
func (l *local) restorePausedTombstone(ctx context.Context, snapshot *cubeboxstore.CubeBox, guard *workflow.PausedTombstoneGuard) error {
	if snapshot == nil {
		return nil
	}
	clean := cleanPausedTombstone(snapshot)
	if clean == nil {
		return nil
	}

	l.pausedTombstoneMu.Lock()
	defer l.pausedTombstoneMu.Unlock()

	allowed, _, err := l.pausedTombstoneInspectLocked(ctx, snapshot.ID, guard)
	if err != nil {
		// A store read failure is not evidence that the row is gone. Refusing
		// to guess keeps a failed create from overwriting an unknown row.
		return fmt.Errorf("restore paused tombstone %s: cannot read sandbox state: %w", snapshot.ID, err)
	}
	if !allowed {
		log.G(ctx).Infof("restore paused tombstone %s: skipped, a newer operation owns the sandbox", snapshot.ID)
		return nil
	}

	if err := l.cubeboxManger.Save(ctx, clean, cubes.WithNoEvent); err != nil {
		return fmt.Errorf("restore paused tombstone %s: %w", clean.ID, err)
	}
	if guard != nil {
		guard.RestoredRow = clean
	}
	return nil
}

// pausedTombstoneRollbackRow decides whether a create failover's Destroy may
// clean up this sandbox, and returns the partial debris row (if any) that
// local.Destroy should tear down.
//
// When allowed is false (or err != nil), a newer attempt or user delete owns
// the sandbox ID and the caller must abort the entire rollback without touching
// either the store row or the live sandbox's runtime/network/storage. When
// allowed is true and debrisRow is nil, the store row is absent or is already
// this attempt's clean PAUSED tombstone, so local.Destroy skips container
// teardown and lets its deferred restore ensure the clean PAUSED row is saved.
func (l *local) pausedTombstoneRollbackRow(
	ctx context.Context,
	id string,
	guard *workflow.PausedTombstoneGuard,
) (allowed bool, debrisRow *cubeboxstore.CubeBox, err error) {
	l.pausedTombstoneMu.Lock()
	defer l.pausedTombstoneMu.Unlock()

	return l.pausedTombstoneInspectLocked(ctx, id, guard)
}

// pausedTombstoneInspectLocked is the single ownership check shared by
// restorePausedTombstone and pausedTombstoneRollbackRow. Caller must hold
// pausedTombstoneMu.
func (l *local) pausedTombstoneInspectLocked(
	ctx context.Context,
	id string,
	guard *workflow.PausedTombstoneGuard,
) (allowed bool, debrisRow *cubeboxstore.CubeBox, err error) {
	curEpoch := l.pausedTombstoneEpoch[id]
	if guard != nil && guard.Epoch != 0 {
		if curEpoch != guard.Epoch {
			return false, nil, nil
		}
	} else if curEpoch != 0 {
		return false, nil, nil
	}

	current, getErr := l.cubeboxManger.Get(ctx, id)
	if getErr != nil {
		if errors.Is(getErr, utils.ErrorKeyNotFound) || errors.Is(getErr, utils.ErrorBucketNotFound) {
			return true, nil, nil
		}
		return false, nil, getErr
	}
	if current == nil {
		// Empty slot (and store stubs that report a miss as (nil, nil)).
		return true, nil, nil
	}
	if guard == nil {
		return false, nil, nil
	}
	if (guard.OrigRow != nil && current == guard.OrigRow) ||
		(guard.RestoredRow != nil && current == guard.RestoredRow) {
		return true, nil, nil
	}
	if guard.CreatedRow != nil && current == guard.CreatedRow {
		return true, current, nil
	}
	return false, nil, nil
}

// cleanPausedTombstone drops everything a failed create or rollback may have
// stamped onto the snapshot. Save uses WithNoEvent so restoring a row does not
// emit a spurious sandbox update event.
func cleanPausedTombstone(snapshot *cubeboxstore.CubeBox) *cubeboxstore.CubeBox {
	clean := snapshot.DeepCopy()
	if clean == nil {
		return nil
	}
	clean.UserDeleteMark = cubeboxstore.UserDeleteMark{}
	if st := clean.GetStatus(); st != nil {
		_ = st.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
			status.Removing = false
			return status, nil
		})
	}
	return clean
}

// saveCreateRow persists the first row of a create while recording row
// ownership under pausedTombstoneMu. Keeping the registration and Save under
// the same mutex as restorePausedTombstone closes the check-then-write race
// between a late rollback and a newer create for the same sandbox ID.
func (l *local) saveCreateRow(ctx context.Context, opts *workflow.CreateContext, row *cubeboxstore.CubeBox) error {
	l.pausedTombstoneMu.Lock()
	defer l.pausedTombstoneMu.Unlock()

	if err := l.cubeboxManger.Save(ctx, row, cubes.WithNoEvent); err != nil {
		return err
	}
	if row == nil || row.ID == "" {
		return nil
	}
	if opts != nil && opts.PausedTombstoneGuard != nil {
		g := opts.PausedTombstoneGuard
		if g.Epoch == 0 {
			g.Epoch = l.nextPausedTombstoneEpochLocked(row.ID)
		} else if l.pausedTombstoneEpoch[row.ID] < g.Epoch {
			if l.pausedTombstoneEpoch == nil {
				l.pausedTombstoneEpoch = make(map[string]uint64)
			}
			l.pausedTombstoneEpoch[row.ID] = g.Epoch
		}
		g.CreatedRow = row
		return nil
	}
	// If this sandbox ID previously participated in a PAUSED replacement,
	// advance its epoch so any older rollback cannot overwrite this row.
	if _, tracked := l.pausedTombstoneEpoch[row.ID]; tracked {
		_ = l.nextPausedTombstoneEpochLocked(row.ID)
	}
	return nil
}

func (l *local) nextPausedTombstoneEpochLocked(id string) uint64 {
	l.pausedTombstoneSeq++
	epoch := l.pausedTombstoneSeq
	if id != "" {
		if l.pausedTombstoneEpoch == nil {
			l.pausedTombstoneEpoch = make(map[string]uint64)
		}
		l.pausedTombstoneEpoch[id] = epoch
	}
	return epoch
}

func (l *local) advancePausedTombstoneEpochIfTracked(id string) {
	if l == nil || id == "" {
		return
	}
	l.pausedTombstoneMu.Lock()
	defer l.pausedTombstoneMu.Unlock()
	if _, tracked := l.pausedTombstoneEpoch[id]; tracked {
		_ = l.nextPausedTombstoneEpochLocked(id)
	}
}
