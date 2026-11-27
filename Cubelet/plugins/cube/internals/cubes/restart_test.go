// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"
	"os"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func newRecoveryClient(t *testing.T, store containers.Store, tasks tasksapi.TasksClient) *containerd.Client {
	t.Helper()
	client, err := containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(store),
		containerd.WithTaskClient(tasks),
	))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}

func recoverySandbox(status cubeboxstore.Status) (*cubeboxstore.CubeBox, *cubeboxstore.Container) {
	box := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	box.ID = "sb"
	ctr := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "sb"},
		IsPod:    true,
		Status:   cubeboxstore.StoreStatus(status),
	}
	box.AddContainer(ctr)
	return box, ctr
}

func TestRecoverContainerMarksUnobservedExitWhenContainerIsGone(t *testing.T) {
	client := newRecoveryClient(t,
		recoveryMissingStore{},
		recoveryTasks{})
	box, ctr := recoverySandbox(cubeboxstore.Status{StartedAt: time.Now().Add(-time.Minute).UnixNano()})

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.NotZero(t, st.FinishedAt)
	require.True(t, st.ExitUnobserved, "an exit nobody observed must be recorded as such")
	require.Zero(t, st.ExitCode)
	require.Empty(t, st.Reason, "recovery itself must not invent an exit reason")
}

func TestRecoverContainerKeepsRecordedExitWhenContainerIsGone(t *testing.T) {
	finished := time.Now().Add(-2 * time.Minute).UnixNano()
	client := newRecoveryClient(t, recoveryMissingStore{}, recoveryTasks{})
	box, ctr := recoverySandbox(cubeboxstore.Status{
		StartedAt:      finished - int64(time.Minute),
		FinishedAt:     finished,
		ExitCode:       7,
		Reason:         string(restartpolicy.ReasonError),
		ExitUnobserved: true,
	})

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.Equal(t, finished, st.FinishedAt)
	require.Equal(t, int32(7), st.ExitCode, "a recorded exit code must survive recovery")
	require.True(t, st.ExitUnobserved, "the observation flag must survive recovery")
	require.Equal(t, string(restartpolicy.ReasonError), st.Reason)
}

func TestLoadStatusMarksUnknownWhenTaskIsGoneAndNothingWasRecorded(t *testing.T) {
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{})
	ctr, err := client.LoadContainer(context.Background(), "sb")
	require.NoError(t, err)

	got, err := loadStatus(context.Background(), ctr, &cubeboxstore.Status{StartedAt: 1})
	require.NoError(t, err)
	require.True(t, got.Unknown)
	require.Zero(t, got.FinishedAt)
	require.False(t, got.ExitUnobserved)
}

func TestLoadStatusKeepsRecordedExitCodeWhenTaskIsGone(t *testing.T) {
	finished := time.Now().Add(-time.Minute).UnixNano()
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{})
	ctr, err := client.LoadContainer(context.Background(), "sb")
	require.NoError(t, err)

	got, err := loadStatus(context.Background(), ctr, &cubeboxstore.Status{
		StartedAt:      finished - int64(time.Minute),
		FinishedAt:     finished,
		ExitCode:       9,
		ExitUnobserved: false,
	})
	require.NoError(t, err)
	require.Equal(t, finished, got.FinishedAt)
	require.Equal(t, int32(9), got.ExitCode)
	require.False(t, got.Unknown)
}

func TestLoadStatusDerivesReasonFromStoppedTask(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing cubeboxstore.Status
		exitCode uint32
		wantCode int32
		want     restartpolicy.Reason
	}{
		{
			name:     "clean exit while cubelet was down",
			existing: cubeboxstore.Status{StartedAt: time.Now().Add(-time.Hour).UnixNano()},
			exitCode: 0,
			want:     restartpolicy.ReasonCompleted,
		},
		{
			name:     "failure exit while cubelet was down",
			existing: cubeboxstore.Status{StartedAt: time.Now().Add(-time.Hour).UnixNano()},
			exitCode: 42,
			wantCode: 42,
			want:     restartpolicy.ReasonError,
		},
		{
			name: "oom recorded for this run",
			existing: cubeboxstore.Status{
				StartedAt:  time.Now().Add(-time.Hour).UnixNano(),
				FinishedAt: time.Now().Add(-time.Minute).UnixNano(),
				Reason:     string(restartpolicy.ReasonOOMKilled),
			},
			exitCode: 137,
			wantCode: 137,
			want:     restartpolicy.ReasonOOMKilled,
		},
		{
			name: "stale oom from an earlier run",
			existing: cubeboxstore.Status{
				StartedAt:  time.Now().Add(-time.Minute).UnixNano(),
				FinishedAt: time.Now().Add(-time.Hour).UnixNano(),
				Reason:     string(restartpolicy.ReasonOOMKilled),
			},
			exitCode: 3,
			wantCode: 3,
			want:     restartpolicy.ReasonError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{
				get: &tasktypes.Process{
					ID:         "sb",
					Status:     tasktypes.Status_STOPPED,
					ExitStatus: tc.exitCode,
					ExitedAt:   timestamppb.New(time.Now().Add(-time.Minute)),
				},
			})
			ctr, err := client.LoadContainer(context.Background(), "sb")
			require.NoError(t, err)

			existing := tc.existing
			got, err := loadStatus(context.Background(), ctr, &existing)
			require.NoError(t, err)
			require.NotZero(t, got.FinishedAt)
			require.Equal(t, tc.wantCode, got.ExitCode)
			require.Equal(t, string(tc.want), got.Reason)
			require.False(t, got.ExitUnobserved)
		})
	}
}

func TestLoadStatusBackfillsMissingExitTime(t *testing.T) {
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{
		get: &tasktypes.Process{ID: "sb", Status: tasktypes.Status_STOPPED, ExitStatus: 1},
	})
	ctr, err := client.LoadContainer(context.Background(), "sb")
	require.NoError(t, err)

	got, err := loadStatus(context.Background(), ctr, &cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	require.NoError(t, err)
	require.NotZero(t, got.FinishedAt)
	require.Equal(t, int32(1), got.ExitCode)
	require.Equal(t, string(restartpolicy.ReasonError), got.Reason)
}

func TestRecoverContainerClearsStaleExitReasonButKeepsPauseFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		want   string
	}{
		{name: "stale exit reason", reason: string(restartpolicy.ReasonSandboxCrashed)},
		{name: "pause failed", reason: "PauseFailed", want: "PauseFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{
				get: &tasktypes.Process{ID: "sb", Pid: 4242, Status: tasktypes.Status_RUNNING},
			})
			finished := time.Now().Add(-time.Minute).UnixNano()
			box, ctr := recoverySandbox(cubeboxstore.Status{
				StartedAt:  finished - int64(time.Minute),
				FinishedAt: finished,
				Reason:     tc.reason,
			})

			got, err := RecoverContainer(context.Background(), client, box, ctr)
			require.NoError(t, err)
			st := got.Status.Get()
			require.Zero(t, st.FinishedAt)
			require.Equal(t, tc.want, st.Reason)
		})
	}
}

func TestRecoverContainerKeepsStatusOnTaskStateTimeout(t *testing.T) {
	started := time.Now().Add(-time.Minute).UnixNano()
	for _, tc := range []struct {
		name  string
		tasks recoveryTasks
	}{
		{
			name:  "containerd returns Status_UNKNOWN on state timeout",
			tasks: recoveryTasks{get: &tasktypes.Process{ID: "sb", Status: tasktypes.Status_UNKNOWN}},
		},
		{
			name:  "task query returns deadline exceeded",
			tasks: recoveryTasks{err: context.DeadlineExceeded},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newRecoveryClient(t, recoveryStore{}, tc.tasks)
			box, ctr := recoverySandbox(cubeboxstore.Status{StartedAt: started, Pid: 4242})

			got, err := RecoverContainer(context.Background(), client, box, ctr)
			require.NoError(t, err)

			st := got.Status.Get()
			require.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, st.State())
			require.False(t, st.Unknown)
			require.False(t, st.ExitUnobserved)
			require.Zero(t, st.FinishedAt)
			require.Equal(t, uint32(4242), st.Pid)
		})
	}
}

func TestRecoverContainerMarksUnobservedWhenPriorStatusIsUnknownOrCreated(t *testing.T) {
	self, err := utils.ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)

	created := time.Now().Add(-time.Minute).UnixNano()
	for _, tc := range []struct {
		name   string
		status cubeboxstore.Status
		tasks  recoveryTasks
		alive  bool
	}{
		{
			name:   "empty prior status on timeout",
			status: cubeboxstore.Status{},
			tasks:  recoveryTasks{err: context.DeadlineExceeded},
		},
		{
			name:   "created prior status on timeout",
			status: cubeboxstore.Status{CreatedAt: created},
			tasks:  recoveryTasks{err: context.DeadlineExceeded},
		},
		{
			name:   "created prior status with missing task and live shim",
			status: cubeboxstore.Status{CreatedAt: created},
			tasks:  recoveryTasks{},
			alive:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newRecoveryClient(t, recoveryStore{}, tc.tasks)
			box, ctr := recoverySandbox(tc.status)
			if tc.alive {
				box.Endpoint = sandboxstore.Endpoint{Pid: uint32(self.Pid), PidStartTime: self.StartTime}
			}

			got, err := RecoverContainer(context.Background(), client, box, ctr)
			require.NoError(t, err)

			st := got.Status.Get()
			require.True(t, st.Unknown)
			require.True(t, st.ExitUnobserved)
			require.NotZero(t, st.FinishedAt)
		})
	}
}

func TestRecoverContainerKeepsRunningWhenTaskMissingButShimAlive(t *testing.T) {
	self, err := utils.ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)

	started := time.Now().Add(-time.Minute).UnixNano()
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{})
	box, ctr := recoverySandbox(cubeboxstore.Status{StartedAt: started, Pid: uint32(self.Pid)})
	box.Endpoint = sandboxstore.Endpoint{Pid: uint32(self.Pid), PidStartTime: self.StartTime}

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, st.State())
	require.False(t, st.Unknown)
	require.False(t, st.ExitUnobserved)
	require.Zero(t, st.FinishedAt)
}

func TestRecoverContainerMarksUnknownWhenTaskMissingAndShimRecycled(t *testing.T) {
	self, err := utils.ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)

	started := time.Now().Add(-time.Minute).UnixNano()
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{})
	box, ctr := recoverySandbox(cubeboxstore.Status{StartedAt: started, Pid: uint32(self.Pid)})
	box.Endpoint = sandboxstore.Endpoint{Pid: uint32(self.Pid), PidStartTime: self.StartTime + 1}

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.True(t, st.Unknown)
	require.Equal(t, cubebox.ContainerState_CONTAINER_UNKNOWN, st.State())
}

func TestRecoverContainerMarksUnknownWhenTaskMissingAndStartTimeZero(t *testing.T) {
	self, err := utils.ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)

	started := time.Now().Add(-time.Minute).UnixNano()
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{})
	box, ctr := recoverySandbox(cubeboxstore.Status{StartedAt: started, Pid: uint32(self.Pid)})
	box.Endpoint = sandboxstore.Endpoint{Pid: uint32(self.Pid)}

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.True(t, st.Unknown)
	require.Equal(t, cubebox.ContainerState_CONTAINER_UNKNOWN, st.State())
}

func TestRecoverContainerMarksUnobservedOnErrorWhenShimIsGone(t *testing.T) {
	self, err := utils.ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)

	started := time.Now().Add(-time.Minute).UnixNano()
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{err: context.DeadlineExceeded})
	box, ctr := recoverySandbox(cubeboxstore.Status{StartedAt: started, Pid: uint32(self.Pid)})
	box.Endpoint = sandboxstore.Endpoint{Pid: uint32(self.Pid), PidStartTime: self.StartTime + 1}

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.True(t, st.Unknown)
	require.True(t, st.ExitUnobserved)
	require.NotZero(t, st.FinishedAt)
}

func TestRecoverContainerPreservesStoppedExitWhenDeleteFails(t *testing.T) {
	started := time.Now().Add(-time.Hour).UnixNano()
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{
		get: &tasktypes.Process{
			ID:         "sb",
			Status:     tasktypes.Status_STOPPED,
			ExitStatus: 42,
			ExitedAt:   timestamppb.New(time.Now().Add(-time.Minute)),
		},
		deleteErr: context.DeadlineExceeded,
	})
	box, ctr := recoverySandbox(cubeboxstore.Status{StartedAt: started, Pid: 4242})

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.Equal(t, cubebox.ContainerState_CONTAINER_EXITED, st.State())
	require.False(t, st.Unknown)
	require.False(t, st.ExitUnobserved)
	require.Equal(t, int32(42), st.ExitCode)
	require.Equal(t, string(restartpolicy.ReasonError), st.Reason)
}

func TestRecoverContainerDoesNotUsePodShimForNonPodContainer(t *testing.T) {
	self, err := utils.ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)

	started := time.Now().Add(-time.Minute).UnixNano()
	client := newRecoveryClient(t, recoveryStore{}, recoveryTasks{})
	box := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	box.ID = "sb"
	box.Endpoint = sandboxstore.Endpoint{Pid: uint32(self.Pid), PidStartTime: self.StartTime}
	ctr := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "workload"},
		IsPod:    false,
		Status:   cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: started}),
	}
	box.AddContainer(ctr)

	got, err := RecoverContainer(context.Background(), client, box, ctr)
	require.NoError(t, err)

	st := got.Status.Get()
	require.True(t, st.Unknown)
	require.Equal(t, cubebox.ContainerState_CONTAINER_UNKNOWN, st.State())
}

type recoveryStore struct{ containers.Store }

func (recoveryStore) Get(_ context.Context, id string) (containers.Container, error) {
	return containers.Container{ID: id}, nil
}

type recoveryMissingStore struct{ containers.Store }

func (recoveryMissingStore) Get(_ context.Context, id string) (containers.Container, error) {
	return containers.Container{}, errdefs.ErrNotFound
}

type recoveryTasks struct {
	tasksapi.TasksClient
	get       *tasktypes.Process
	err       error
	deleteErr error
}

func (r recoveryTasks) Get(context.Context, *tasksapi.GetRequest, ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.get == nil {
		return nil, errdefs.ErrNotFound
	}
	return &tasksapi.GetResponse{Process: r.get}, nil
}

func (r recoveryTasks) Delete(context.Context, *tasksapi.DeleteTaskRequest, ...grpc.CallOption) (*tasksapi.DeleteResponse, error) {
	if r.deleteErr != nil {
		return nil, r.deleteErr
	}
	return &tasksapi.DeleteResponse{}, nil
}
