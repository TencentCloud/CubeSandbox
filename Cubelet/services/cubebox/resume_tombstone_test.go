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
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

type tombstoneStoreAPI struct {
	fakeCubeboxAPI
	getErr error
}

func (f *tombstoneStoreAPI) Get(context.Context, string) (*cubeboxstore.CubeBox, error) {
	return nil, f.getErr
}

func pausedTombstoneForTest() *cubeboxstore.CubeBox {
	now := time.Now()
	sb := sandboxWithResourceForTest("sb-paused", cubeboxstore.Status{PausedAt: now.UnixNano()},
		"1000m", "2Gi", 1, 0, 0)
	sb.SandboxID = sb.ID
	return sb
}

func TestRestorePausedTombstoneWritesBackCleanPausedRow(t *testing.T) {
	now := time.Now()
	snap := pausedTombstoneForTest()
	// Simulate the mutations the create/rollback path may have made before the
	// restore: the restored row must not keep any of them.
	snap.UserMarkDeletedTime = &now
	snap.DeleteRequestID = "req-delete"
	_ = snap.GetStatus().Update(func(st cubeboxstore.Status) (cubeboxstore.Status, error) {
		st.Removing = true
		return st, nil
	})

	api := &fakeCubeboxAPI{}
	mgr := &local{cubeboxManger: api}
	guard := &workflow.PausedTombstoneGuard{}
	require.NoError(t, mgr.restorePausedTombstone(context.Background(), snap, guard))

	require.NotNil(t, api.cb)
	assert.Equal(t, 1, api.saveCalls)
	assert.Same(t, api.cb, guard.RestoredRow)
	assert.Equal(t, cubebox.ContainerState_CONTAINER_PAUSED, api.cb.GetStatus().Get().State())
	assert.Nil(t, api.cb.UserMarkDeletedTime, "the CDP delete marker must not survive")
	assert.Empty(t, api.cb.DeleteRequestID)
	assert.False(t, api.cb.GetStatus().Get().Removing)
	require.NotNil(t, api.cb.ResourceWithOverHead,
		"the restored row must still size a retry's admission demand")
	assert.Equal(t, int64(2048*1024*1024), api.cb.ResourceWithOverHead.HostMemQ.Value())
}

func TestRestorePausedTombstoneIsNoopWithoutSnapshot(t *testing.T) {
	api := &fakeCubeboxAPI{}
	mgr := &local{cubeboxManger: api}
	require.NoError(t, mgr.restorePausedTombstone(context.Background(), nil, &workflow.PausedTombstoneGuard{}))
	assert.Zero(t, api.saveCalls)
}

// This is the exact case from the review: the create flow deleted the row, so
// rollback's Get returns not-found and destroy.go returns nil early. The
// deferred restore must still put a PAUSED row back, or the retry is admitted
// as a cross-node restore.
func TestCreateRollbackRestoresPausedTombstoneWhenStoreRowIsGone(t *testing.T) {
	snap := pausedTombstoneForTest()
	api := &tombstoneStoreAPI{getErr: utils.ErrorKeyNotFound}
	mgr := &local{cubeboxManger: api}
	opts := &workflow.DestroyContext{
		DestroyInfo:            &cubebox.DestroyCubeSandboxRequest{SandboxID: snap.ID, RequestID: "req-rollback"},
		RestorePausedTombstone: snap,
		PausedTombstoneGuard:   &workflow.PausedTombstoneGuard{},
	}
	opts.IsRollBack = true
	opts.SandboxID = snap.ID

	require.NoError(t, mgr.Destroy(context.Background(), opts))
	require.NotNil(t, api.cb, "rollback must leave a local PAUSED record for the retry")
	assert.Equal(t, cubebox.ContainerState_CONTAINER_PAUSED, api.cb.GetStatus().Get().State())
	assert.Equal(t, snap.ID, api.cb.SandboxID)
}

// A normal user delete must not restore anything: only create failover sets
// RestorePausedTombstone.
func TestDestroyWithoutRestoreSnapshotLeavesStoreAlone(t *testing.T) {
	api := &tombstoneStoreAPI{getErr: utils.ErrorKeyNotFound}
	mgr := &local{cubeboxManger: api}
	opts := &workflow.DestroyContext{
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{SandboxID: "sb-other", RequestID: "req-delete"},
	}
	opts.SandboxID = "sb-other"

	require.NoError(t, mgr.Destroy(context.Background(), opts))
	assert.Nil(t, api.cb)
	assert.Zero(t, api.saveCalls)
}
