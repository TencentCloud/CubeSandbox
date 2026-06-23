// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

type fakeCubeboxAPI struct {
	cb      *cubeboxstore.CubeBox
	syncIDs []string
}

func (f *fakeCubeboxAPI) Init(ctx context.Context) error {
	return nil
}

func (f *fakeCubeboxAPI) Get(ctx context.Context, id string) (*cubeboxstore.CubeBox, error) {
	if f.cb != nil && f.cb.ID == id {
		return f.cb, nil
	}
	return nil, nil
}

func (f *fakeCubeboxAPI) FindContainerOfCubebox(ctx context.Context, id string) (*cubeboxstore.Container, *cubeboxstore.CubeBox, error) {
	if f.cb == nil {
		return nil, nil, nil
	}
	cntr, err := f.cb.Get(id)
	if err != nil {
		return nil, f.cb, nil
	}
	return cntr, f.cb, nil
}

func (f *fakeCubeboxAPI) List() []*cubeboxstore.CubeBox {
	if f.cb == nil {
		return nil
	}
	return []*cubeboxstore.CubeBox{f.cb}
}

func (f *fakeCubeboxAPI) Save(ctx context.Context, info *cubeboxstore.CubeBox, opts ...cubes.UpdateCubeboxOpt) error {
	f.cb = info
	return nil
}

func (f *fakeCubeboxAPI) SyncByID(ctx context.Context, id string, opts ...cubes.UpdateCubeboxOpt) error {
	f.syncIDs = append(f.syncIDs, id)
	return nil
}

func (f *fakeCubeboxAPI) Delete(ctx context.Context, opt *cubes.DeleteOption) error {
	return nil
}

func (f *fakeCubeboxAPI) IsImageInUse(imageID string) (bool, error) {
	return false, nil
}

func TestConvergeResumeStateAfterOpaqueRestoreClearsPauseStateAndInvalidatesBindings(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-resume-helper", cubeboxstore.Status{
		PausedAt:  time.Now().Add(-2 * time.Minute).UnixNano(),
		PausingAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	cb.Metadata.Labels = map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID:                "snap-before-resume",
		constants.MasterAnnotationRuntimeRestoreSnapshotID:         "restore-before-resume",
		constants.MasterAnnotationRuntimeSnapshotAttachedAt:        time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano),
		constants.MasterAnnotationRuntimeRestoreSnapshotAttachedAt: time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano),
		"keep": "value",
	}
	cb.ContainersMap.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "sb-resume-helper-sidecar"},
		Status: cubeboxstore.StoreStatus(cubeboxstore.Status{
			PausedAt:  time.Now().Add(-90 * time.Second).UnixNano(),
			PausingAt: time.Now().Add(-45 * time.Second).UnixNano(),
		}),
	})

	attachedAt := time.Date(2026, 5, 31, 9, 30, 0, 0, time.UTC)
	convergeResumeStateAfterOpaqueRestore(cb, attachedAt)

	for id, cntr := range cb.AllContainers() {
		got := cntr.Status.Get()
		assert.Equal(t, int64(0), got.PausedAt, "PausedAt must be cleared for %s", id)
		assert.Equal(t, int64(0), got.PausingAt, "PausingAt must be cleared for %s", id)
	}
	assert.Equal(t, runtimeSnapshotBindingInvalidID, cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, runtimeSnapshotBindingInvalidID, cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID])
	assert.Equal(t, attachedAt.Format(time.RFC3339Nano), cb.Labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt])
	assert.Equal(t, attachedAt.Format(time.RFC3339Nano), cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotAttachedAt])
	assert.Equal(t, "value", cb.Labels["keep"], "unrelated labels must be preserved")
}

func TestHandleEventTaskResumedConvergesOpaqueRestoreBindings(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-resume-event", cubeboxstore.Status{
		PausedAt:  time.Now().Add(-2 * time.Minute).UnixNano(),
		PausingAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	cb.Metadata.Labels = map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID:        "snap-before-event",
		constants.MasterAnnotationRuntimeRestoreSnapshotID: "restore-before-event",
	}
	manager := &fakeCubeboxAPI{cb: cb}
	em := &eventMonitor{c: &local{cubeboxManger: manager}}

	err := em.handleEvent(context.Background(), &eventtypes.TaskResumed{ContainerID: cb.FirstContainer().ID})
	require.NoError(t, err)

	got := cb.GetStatus().Get()
	assert.Equal(t, int64(0), got.PausedAt, "TaskResumed must clear PausedAt")
	assert.Equal(t, int64(0), got.PausingAt, "TaskResumed must clear PausingAt")
	assert.Equal(t, runtimeSnapshotBindingInvalidID, cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, runtimeSnapshotBindingInvalidID, cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID])
	assert.NotEmpty(t, cb.Labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt], "TaskResumed must stamp attached_at")
	assert.NotEmpty(t, cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotAttachedAt], "TaskResumed must stamp restore attached_at")
	assert.Equal(t, []string{cb.ID}, manager.syncIDs, "TaskResumed must persist the converged cubebox")
}
