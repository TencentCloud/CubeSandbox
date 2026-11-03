// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"testing"

	eventtypes "github.com/containerd/containerd/api/events"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTaskExitReasonReachesSandboxList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pid      uint32
		exitCode uint32
		oomFirst bool
		oomLast  bool
		want     string
	}{
		{"exit one", 12345, 1, false, false, "Error"},
		{"killed", 12345, 137, false, false, "Error"},
		{"clean exit", 12345, 0, false, false, "Completed"},
		{"shim crash", 0, 0, false, false, "SandboxCrashed"},
		{"OOM before exit", 12345, 137, true, false, "OOMKilled"},
		{"OOM after exit", 12345, 137, false, true, "OOMKilled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newCubeboxWithStatusForTest("sandbox-exit", cubeboxstore.Status{StartedAt: 1})
			container := box.FirstContainer()
			container.IsPod = true
			container.Container = &fakeDestroyContainer{taskErr: errdefs.ErrNotFound}
			manager := &fakeCubeboxAPI{cb: box}
			local := &local{cubeboxManger: manager}
			monitor := &eventMonitor{c: local}
			ctx := context.Background()
			if tc.oomFirst {
				require.NoError(t, monitor.handleEvent(ctx, &eventtypes.TaskOOM{ContainerID: container.ID}))
			}
			require.NoError(t, monitor.handleEvent(ctx, &eventtypes.TaskExit{
				ContainerID: container.ID,
				ID:          container.ID,
				Pid:         tc.pid,
				ExitStatus:  tc.exitCode,
				ExitedAt:    timestamppb.Now(),
			}))
			if tc.oomLast {
				require.NoError(t, monitor.handleEvent(ctx, &eventtypes.TaskOOM{ContainerID: container.ID}))
			}
			require.Equal(t, tc.want, container.Status.Get().Reason)
			require.Contains(t, manager.syncIDs, box.ID)
			require.Empty(t, box.LastExitReason, "status reasons must not depend on the restart manager")
			client, err := containerd.New("", containerd.WithServices(
				containerd.WithContainerStore(exitRecoveryStore{}),
				containerd.WithTaskClient(exitRecoveryTasks{}),
			))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			// The periodic scan recovers metadata after the exit handler deletes the task.
			_, err = cubes.RecoverContainer(ctx, client, box, container)
			require.NoError(t, err)
			s := &service{cubeboxMgr: local}
			rsp, err := s.List(ctx, &cubeboxv1.ListCubeSandboxRequest{Id: &box.ID})
			require.NoError(t, err)
			encoded, err := proto.Marshal(rsp)
			require.NoError(t, err)
			wire := &cubeboxv1.ListCubeSandboxResponse{}
			require.NoError(t, proto.Unmarshal(encoded, wire))
			require.Len(t, wire.Items, 1)
			require.Equal(t, tc.want, wire.Items[0].GetErrorReason())
			require.Equal(t, cubeboxv1.ContainerState_CONTAINER_EXITED, wire.Items[0].Containers[0].State)
			require.Empty(t, wire.Items[0].GetPrivateCubeboxStorageData())
		})
	}
}

type exitRecoveryStore struct{ containers.Store }

func (exitRecoveryStore) Get(_ context.Context, id string) (containers.Container, error) {
	return containers.Container{ID: id}, nil
}

type exitRecoveryTasks struct{ tasksapi.TasksClient }

func (exitRecoveryTasks) Get(context.Context, *tasksapi.GetRequest, ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	return nil, errdefs.ErrNotFound
}
