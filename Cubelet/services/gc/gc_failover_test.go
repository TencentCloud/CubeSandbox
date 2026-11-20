// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import (
	"context"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func gcRow(id string, status cubeboxstore.Status) *cubeboxstore.CubeBox {
	return &cubeboxstore.CubeBox{
		Metadata:           cubeboxstore.Metadata{ID: id, SandboxID: id, CreatedAt: time.Now().UnixNano()},
		FirstContainerName: id,
		ContainersMap: &cubeboxstore.ContainersMap{
			ContainerMap: map[string]*cubeboxstore.Container{id: {
				Metadata: cubeboxstore.Metadata{ID: id},
				Status:   cubeboxstore.StoreStatus(status),
			}},
		},
	}
}

func gcCreateCtx() context.Context {
	return namespaces.WithNamespace(context.Background(), "default")
}

// TestCreateSkipsStampingRestoredTombstoneOnFailover pins the gc-cleanup
// interaction with the rollback restore: when a create rollback has just
// restored/kept a PAUSED tombstone (failover marker set) and its teardown
// failed — which routes here via engine.cleanUp — gc's Create must NOT
// re-stamp UserMarkDeletedTime. Re-stamping would make gcservice.allowCleanup
// proceed against the row and reclaim the pause package the user's retry
// resumes from, undoing the scrub the rollback guards just performed.
func TestCreateSkipsStampingRestoredTombstoneOnFailover(t *testing.T) {
	cb := gcRow("sb-failover-paused", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().UnixNano(),
	})
	l := newTestGC(t)
	l.cubeboxManger = gcBoxStub{cb: cb}

	opts := &workflow.CreateContext{}
	opts.SandboxID = "sb-failover-paused"
	ctx := constants.WithFailoverOperation(gcCreateCtx())

	require.NoError(t, l.Create(ctx, opts))
	assert.Nil(t, cb.UserMarkDeletedTime,
		"a failover over a paused tombstone must not be re-marked user-deleted")
}

// TestCreateStillStampsOrdinaryDebrisOnFailover confirms the skip is narrow: a
// failover over a NON-paused row (genuine create debris) still gets stamped so
// cleanup can quarantine and reclaim it as before.
func TestCreateStillStampsOrdinaryDebrisOnFailover(t *testing.T) {
	cb := gcRow("sb-debris", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	l := newTestGC(t)
	l.cubeboxManger = gcBoxStub{cb: cb}

	opts := &workflow.CreateContext{}
	opts.SandboxID = "sb-debris"
	ctx := constants.WithFailoverOperation(gcCreateCtx())

	require.NoError(t, l.Create(ctx, opts))
	assert.NotNil(t, cb.UserMarkDeletedTime,
		"a failover over ordinary debris must still stamp it for cleanup")
}

// TestCreateStampsPausedRowWithoutFailoverMarker confirms the skip keys on the
// failover marker: an ordinary cleanup Create over a paused row (not a create
// rollback) keeps the existing stamping behaviour.
func TestCreateStampsPausedRowWithoutFailoverMarker(t *testing.T) {
	cb := gcRow("sb-plain-paused", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().UnixNano(),
	})
	l := newTestGC(t)
	l.cubeboxManger = gcBoxStub{cb: cb}

	opts := &workflow.CreateContext{}
	opts.SandboxID = "sb-plain-paused"

	require.NoError(t, l.Create(gcCreateCtx(), opts))
	assert.NotNil(t, cb.UserMarkDeletedTime,
		"without the failover marker the existing stamping behaviour is unchanged")
}
