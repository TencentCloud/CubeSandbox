package cubebox

import (
	"context"
	"testing"

	eventtypes "github.com/containerd/containerd/api/events"
	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestTaskOOMEventRemainsIndependentOfTaskMonitor(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sandbox-oom", cubeboxstore.Status{StartedAt: 1})
	manager := &fakeCubeboxAPI{cb: cb}
	monitor := &eventMonitor{c: &local{cubeboxManger: manager}}

	err := monitor.handleEvent(context.Background(), &eventtypes.TaskOOM{ContainerID: cb.FirstContainer().ID})
	require.NoError(t, err)
	status := cb.FirstContainer().Status.Get()
	require.Equal(t, int32(137), status.ExitCode)
	require.Equal(t, "OOMKilled", status.Reason)
	require.NotZero(t, status.FinishedAt)
	require.Equal(t, []string{cb.ID}, manager.syncIDs)
}
