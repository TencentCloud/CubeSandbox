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
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

type fakeCubeboxAPI struct {
	cb            *cubeboxstore.CubeBox
	list          []*cubeboxstore.CubeBox
	getCalls      int
	saveCalls     int
	getHook       func(call int)
	syncIDs       []string
	syncSnapshots []fakeCubeboxSyncSnapshot
	syncErr       error
	saveErr       error
}

type fakeCubeboxSyncSnapshot struct {
	pausedAt          int64
	startedAt         int64
	userMarkedDeleted bool
}

func (f *fakeCubeboxAPI) Init(ctx context.Context) error {
	return nil
}

func (f *fakeCubeboxAPI) Get(ctx context.Context, id string) (*cubeboxstore.CubeBox, error) {
	cb := f.cb
	f.getCalls++
	if f.getHook != nil {
		f.getHook(f.getCalls)
	}
	if cb != nil && cb.ID == id {
		return cb, nil
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
	if f.list != nil {
		return f.list
	}
	if f.cb == nil {
		return nil
	}
	return []*cubeboxstore.CubeBox{f.cb}
}

func (f *fakeCubeboxAPI) Save(ctx context.Context, info *cubeboxstore.CubeBox, opts ...cubes.UpdateCubeboxOpt) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.cb = info
	return nil
}

func (f *fakeCubeboxAPI) SyncByID(ctx context.Context, id string, opts ...cubes.UpdateCubeboxOpt) error {
	f.syncIDs = append(f.syncIDs, id)
	if f.cb != nil && f.cb.GetStatus() != nil {
		status := f.cb.GetStatus().Get()
		f.syncSnapshots = append(f.syncSnapshots, fakeCubeboxSyncSnapshot{
			pausedAt:          status.PausedAt,
			startedAt:         status.StartedAt,
			userMarkedDeleted: f.cb.UserMarkDeletedTime != nil,
		})
	}
	return f.syncErr
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
	cb.BindRuntimeBase("snap-before-resume")
	cb.BindRestoreBase("restore-before-resume")
	cb.Metadata.Labels = map[string]string{"keep": "value"}
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
		assert.NotZero(t, got.StartedAt, "resumed %s must have a running marker", id)
	}
	assert.Equal(t, "", cb.RuntimeBaseID())
	assert.Equal(t, "", cb.RestoreBaseID())
	assert.Equal(t, "value", cb.Labels["keep"], "unrelated labels must be preserved")
}

func TestHandleEventTaskResumedConvergesOpaqueRestoreBindings(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-resume-event", cubeboxstore.Status{
		PausedAt:  time.Now().Add(-2 * time.Minute).UnixNano(),
		PausingAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	cb.BindRuntimeBase("snap-before-event")
	cb.BindRestoreBase("restore-before-event")
	manager := &fakeCubeboxAPI{cb: cb}
	em := &eventMonitor{c: &local{cubeboxManger: manager}}

	err := em.handleEvent(context.Background(), &eventtypes.TaskResumed{ContainerID: cb.FirstContainer().ID})
	require.NoError(t, err)

	got := cb.GetStatus().Get()
	assert.Equal(t, int64(0), got.PausedAt, "TaskResumed must clear PausedAt")
	assert.Equal(t, int64(0), got.PausingAt, "TaskResumed must clear PausingAt")
	assert.NotZero(t, got.StartedAt, "TaskResumed must restore a running marker")
	assert.Equal(t, "", cb.RuntimeBaseID())
	assert.Equal(t, "", cb.RestoreBaseID())
	assert.Equal(t, []string{cb.ID}, manager.syncIDs, "TaskResumed must persist the converged cubebox")
}
