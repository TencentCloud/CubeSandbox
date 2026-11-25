// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"
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

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
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
	get *tasktypes.Process
}

func (r recoveryTasks) Get(context.Context, *tasksapi.GetRequest, ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	if r.get == nil {
		return nil, errdefs.ErrNotFound
	}
	return &tasksapi.GetResponse{Process: r.get}, nil
}

func (recoveryTasks) Delete(context.Context, *tasksapi.DeleteTaskRequest, ...grpc.CallOption) (*tasksapi.DeleteResponse, error) {
	return &tasksapi.DeleteResponse{}, nil
}
