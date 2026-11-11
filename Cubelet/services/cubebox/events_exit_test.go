// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	eventtypes "github.com/containerd/containerd/api/events"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
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
		reason   string
		finished int64
		want     string
	}{
		{name: "exit one", pid: 12345, exitCode: 1, want: "Error"},
		{name: "killed", pid: 12345, exitCode: 137, want: "Error"},
		{name: "clean exit", pid: 12345, want: "Completed"},
		{name: "shim crash", want: "SandboxCrashed"},
		{name: "OOM before exit", pid: 12345, exitCode: 137, oomFirst: true, want: "OOMKilled"},
		{name: "OOM after exit", pid: 12345, exitCode: 137, oomLast: true, want: "OOMKilled"},
		{name: "stale OOM without finish time", pid: 12345, reason: "OOMKilled", want: "Completed"},
		{name: "OOM from previous run", pid: 12345, reason: "OOMKilled", finished: 1, want: "Completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newCubeboxWithStatusForTest("sandbox-exit", cubeboxstore.Status{
				StartedAt: 2, FinishedAt: tc.finished, Reason: tc.reason,
			})
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

func TestRunningRecoveryClearsExitReasons(t *testing.T) {
	for _, reason := range []string{"Completed", "Error", "OOMKilled", "SandboxCrashed", "PauseFailed"} {
		t.Run(reason, func(t *testing.T) {
			stored, err := json.Marshal(cubeboxstore.Status{StartedAt: 1, FinishedAt: 2, Reason: reason})
			require.NoError(t, err)
			var status cubeboxstore.Status
			require.NoError(t, json.Unmarshal(stored, &status))
			box := newCubeboxWithStatusForTest("sandbox-recovered", status)
			container := box.FirstContainer()
			client, err := containerd.New("", containerd.WithServices(
				containerd.WithContainerStore(exitRecoveryStore{}),
				containerd.WithTaskClient(exitRecoveryTasks{process: &tasktypes.Process{
					ID: container.ID, Pid: 12345, Status: tasktypes.Status_RUNNING,
				}}),
			))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			ctx := context.Background()
			_, err = cubes.RecoverContainer(ctx, client, box, container)
			require.NoError(t, err)
			require.Equal(t, cubeboxv1.ContainerState_CONTAINER_RUNNING, container.Status.Get().State())
			if reason == "PauseFailed" {
				require.Equal(t, reason, container.Status.Get().Reason)
			} else {
				require.Empty(t, container.Status.Get().Reason)
			}

			hookStatus := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hookStatus <- r.Header.Get("status")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			addr := server.Listener.Addr().(*net.TCPAddr)
			container.IP = addr.IP.String()
			container.Config = &cubeboxv1.ContainerConfig{Poststop: &cubeboxv1.PostStop{
				TimeoutMs: 1000,
				LifecyleHandler: &cubeboxv1.LifecycleHandler{HttpGet: &cubeboxv1.HTTPGetAction{
					Port: int32(addr.Port),
				}},
			}}
			container.Container = &fakeDestroyContainer{taskErr: errdefs.ErrNotFound}
			monitor := &eventMonitor{c: &local{cubeboxManger: &fakeCubeboxAPI{cb: box}}}
			require.NoError(t, monitor.handleEvent(ctx, &eventtypes.TaskExit{
				ContainerID: container.ID, ID: container.ID, Pid: 12345, ExitedAt: timestamppb.Now(),
			}))
			require.Equal(t, "Completed", container.Status.Get().Reason)
			select {
			case got := <-hookStatus:
				require.Equal(t, "EXIT", got)
			default:
				t.Fatal("poststop hook was not called")
			}
		})
	}
}

type exitRecoveryStore struct{ containers.Store }

func (exitRecoveryStore) Get(_ context.Context, id string) (containers.Container, error) {
	return containers.Container{ID: id}, nil
}

type exitRecoveryTasks struct {
	tasksapi.TasksClient
	process *tasktypes.Process
}

func (t exitRecoveryTasks) Get(context.Context, *tasksapi.GetRequest, ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	if t.process != nil {
		return &tasksapi.GetResponse{Process: t.process}, nil
	}
	return nil, errdefs.ErrNotFound
}
