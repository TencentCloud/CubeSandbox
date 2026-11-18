// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	dbmodels "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/restoreplace"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestIsPauseBusyReject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"sandbox pause forbidding, terminate exec tasks first", true},
		{`Others("Other: update sandbox failed:sandbox pause forbidding, terminate exec tasks first")`, true},
		{"PAUSE FORBIDDING", true},
		{"terminate exec tasks first", true},
		{"task pause failed: oom", false},
		{"", false},
	} {
		require.Equal(t, tc.want, isPauseBusyReject(tc.msg), tc.msg)
	}
}

func TestFinalizePauseCubeletFailureBusyAborts(t *testing.T) {
	var aborted, failed []string
	patches := gomonkey.ApplyFunc(pausesnap.Abort, func(_ context.Context, sandboxID, snapshotID string) {
		aborted = append(aborted, sandboxID+"/"+snapshotID)
	})
	defer patches.Reset()
	patches.ApplyFunc(pausesnap.MarkFailed, func(_ context.Context, sandboxID, snapshotID, _ string) {
		failed = append(failed, sandboxID+"/"+snapshotID)
	})

	finalizePauseCubeletFailure(context.Background(), "sb-1", "snap-1",
		`Others("Other: update sandbox failed:sandbox pause forbidding, terminate exec tasks first")`)
	require.Equal(t, []string{"sb-1/snap-1"}, aborted)
	require.Empty(t, failed)
}

func TestFinalizePauseCubeletFailureOtherMarksFailed(t *testing.T) {
	var aborted, failed []string
	patches := gomonkey.ApplyFunc(pausesnap.Abort, func(_ context.Context, sandboxID, snapshotID string) {
		aborted = append(aborted, sandboxID+"/"+snapshotID)
	})
	defer patches.Reset()
	patches.ApplyFunc(pausesnap.MarkFailed, func(_ context.Context, sandboxID, snapshotID, _ string) {
		failed = append(failed, sandboxID+"/"+snapshotID)
	})

	finalizePauseCubeletFailure(context.Background(), "sb-2", "snap-2", "rootfs commit failed")
	require.Empty(t, aborted)
	require.Equal(t, []string{"sb-2/snap-2"}, failed)
}

func TestValidatePauseResumeVolumesEmptyOK(t *testing.T) {
	t.Parallel()
	require.NoError(t, validatePauseResumeVolumes(nil))
	require.NoError(t, validatePauseResumeVolumes([]string{"", "  "}))
}

func TestPauseNeedsRemoteStatusWatch(t *testing.T) {
	t.Parallel()
	require.True(t, pauseNeedsRemoteStatusWatch("jfs"))
	require.True(t, pauseNeedsRemoteStatusWatch("juicefs"))
	require.True(t, pauseNeedsRemoteStatusWatch("s3"))
	require.False(t, pauseNeedsRemoteStatusWatch("xfs"))
	require.False(t, pauseNeedsRemoteStatusWatch(""))
}

func TestPausePackageNodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, record, target, backend, origin, pkg string
	}{
		{"same node", "10.0.0.1", "10.0.0.1", "jfs", "", "10.0.0.1"},
		{"blank origin", " ", "10.0.0.1", "jfs", "", "10.0.0.1"},
		{"cross node xfs", " 10.0.0.2 ", "10.0.0.1", "xfs", "10.0.0.2", "10.0.0.2"},
		{"cross node s3", "10.0.0.2", "10.0.0.1", "s3", "10.0.0.2", "10.0.0.2"},
		{"cross node jfs", "10.0.0.2", "10.0.0.1", "jfs", "10.0.0.2", "10.0.0.1"},
		{"cross node juicefs", "10.0.0.2", "10.0.0.1", "juicefs", "10.0.0.2", "10.0.0.1"},
	} {
		origin, pkg := pausePackageNodes(tc.record, tc.target, tc.backend)
		require.Equal(t, tc.origin, origin, tc.name)
		require.Equal(t, tc.pkg, pkg, tc.name)
	}
}

func TestCleanupPausePackageAfterResumeOutlivesCaller(t *testing.T) {
	type call struct {
		ctxErr                  error
		node, snapshot, backend string
	}
	got := make(chan call, 1)
	patches := gomonkey.ApplyFunc(pausesnap.CleanupPauseSnapshot,
		func(ctx context.Context, _, nodeIP, snapshotID, backend string) error {
			got <- call{ctx.Err(), nodeIP, snapshotID, backend}
			return nil
		})
	defer patches.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanupPausePackageAfterResume(ctx, "req-1", "sb-1", "10.0.0.2", "snap-1", "jfs")
	select {
	case c := <-got:
		require.NoError(t, c.ctxErr)
		require.Equal(t, call{nil, "10.0.0.2", "snap-1", "jfs"}, c)
	case <-time.After(5 * time.Second):
		t.Fatal("pause package cleanup never ran")
	}
}

func TestValidatePauseResumeVolumesMissing(t *testing.T) {
	patches := gomonkey.ApplyFunc(resolveVolumeRecord, func(volumeID string) (*dbmodels.VolumeRecord, error) {
		return nil, fmt.Errorf("volume not found: %s", volumeID)
	})
	defer patches.Reset()

	err := validatePauseResumeVolumes([]string{"vol-gone"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot resume")
	require.Contains(t, err.Error(), "vol-gone")
}

func TestValidatePauseResumeVolumesPresent(t *testing.T) {
	patches := gomonkey.ApplyFunc(resolveVolumeRecord, func(volumeID string) (*dbmodels.VolumeRecord, error) {
		return &dbmodels.VolumeRecord{VolumeID: volumeID}, nil
	})
	defer patches.Reset()

	require.NoError(t, validatePauseResumeVolumes([]string{"vol-ok", "  vol-ok2  "}))
}

func TestPauseResumePlacementInputPinsHostMount(t *testing.T) {
	t.Parallel()
	rec := &pausesnap.Record{
		SnapshotID:   "snap-1",
		Backend:      "s3",
		RemoteStatus: "ready",
		NodeID:       "node-a",
		NodeIP:       "10.0.0.1",
	}
	got := pauseResumePlacementInput(rec, "cubebox", &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationHostDirMount: `[{"hostPath":"/data/shared/a","mountPath":"/mnt"}]`,
		},
	})
	if !got.PinToOrigin {
		t.Fatal("host-mount spec must pin resume to origin")
	}
	plain := pauseResumePlacementInput(rec, "cubebox", &types.CreateCubeSandboxReq{})
	if plain.PinToOrigin {
		t.Fatal("spec without host-mount must not pin")
	}
	missing := pauseResumePlacementInput(rec, "cubebox", nil)
	if missing.PinToOrigin {
		t.Fatal("missing spec must not pin")
	}
}

func TestResumePlacementHomesWhenCannotCrossNode(t *testing.T) {
	orig := decidePauseResumePlacementFn
	t.Cleanup(func() { decidePauseResumePlacementFn = orig })
	decidePauseResumePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		t.Fatal("Decide must not run when resume cannot leave origin")
		return nil, fmt.Errorf("Decide must not run")
	}

	rec := &pausesnap.Record{
		SnapshotID: "snap-1",
		Backend:    "xfs",
		NodeID:     "node-a",
		NodeIP:     "10.0.0.1",
	}
	got, err := resumePlacement(context.Background(), rec, "cubebox", nil)
	require.NoError(t, err)
	require.Equal(t, "node-a", got.NodeID)
	require.Equal(t, "10.0.0.1", got.NodeIP)
	require.False(t, got.CrossNode)

	rec.Backend = "s3"
	rec.RemoteStatus = "inprogress"
	got, err = resumePlacement(context.Background(), rec, "cubebox", nil)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1", got.NodeIP)
	require.False(t, got.CrossNode)
}

func TestResumePlacementCallsDecideWhenS3Ready(t *testing.T) {
	orig := decidePauseResumePlacementFn
	t.Cleanup(func() { decidePauseResumePlacementFn = orig })
	called := false
	decidePauseResumePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		called = true
		return &restoreplace.Placement{NodeID: "node-b", NodeIP: "10.0.0.2", CrossNode: true}, nil
	}

	got, err := resumePlacement(context.Background(), &pausesnap.Record{
		SnapshotID:   "snap-1",
		Backend:      "s3",
		RemoteStatus: "ready",
		NodeID:       "node-a",
		NodeIP:       "10.0.0.1",
	}, "cubebox", nil)
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "node-b", got.NodeID)
	require.True(t, got.CrossNode)
}

// A resume that lost the race to another resume finds no pause binding. If
// Cubelet reports the sandbox RUNNING the outcome is "already running"
// (TaskStateInvalid), not a hard params error.
func TestResumeWithoutBindingReportsAlreadyRunning(t *testing.T) {
	cases := []struct {
		name     string
		state    cubebox.ContainerState
		listErr  error
		wantCode int
		wantMsg  string
	}{
		{"running", cubebox.ContainerState_CONTAINER_RUNNING, nil,
			int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid)), "is already running"},
		{"paused", cubebox.ContainerState_CONTAINER_PAUSED, nil,
			int(errorcode.ErrorCode_MasterParamsError), "no pause snapshot"},
		{"cubelet error", cubebox.ContainerState_CONTAINER_RUNNING, errors.New("cubelet down"),
			int(errorcode.ErrorCode_MasterParamsError), "no pause snapshot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const sandboxID = "sb-resume-race"
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
				return nil, pausesnap.ErrNotFound
			})
			origGet := getSandboxProxyMapFn
			t.Cleanup(func() { getSandboxProxyMapFn = origGet })
			getSandboxProxyMapFn = func(context.Context, string) (*proxytypes.SandboxProxyMap, bool) {
				return &proxytypes.SandboxProxyMap{SandboxID: sandboxID, HostIP: "10.0.0.1"}, true
			}
			patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP + ":50051" })
			patches.ApplyFunc(cubelet.List, func(_ context.Context, addr string, _ *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
				require.Equal(t, "10.0.0.1:50051", addr)
				if tc.listErr != nil {
					return nil, tc.listErr
				}
				return &cubebox.ListCubeSandboxResponse{Items: []*cubebox.CubeSandbox{{
					Id:         sandboxID,
					Containers: []*cubebox.Container{{Id: sandboxID, State: tc.state}},
				}}}, nil
			})

			rsp := resumeFromPauseSnapshot(context.Background(),
				&types.UpdateRequest{SandboxID: sandboxID, Action: "resume"}, "10.0.0.1")
			require.Equal(t, tc.wantCode, rsp.Ret.RetCode)
			require.Contains(t, rsp.Ret.RetMsg, tc.wantMsg)
		})
	}
}

// On a multi-replica CubeMaster the losing resume can hold a process-local
// host that still names the pre-pause node while the winner resumed the
// sandbox on another node. The shared proxy map names the new node, so the
// sandbox is found RUNNING there, reported as already running, and this
// replica's cache is corrected.
func TestResumeWithoutBindingFindsSandboxResumedOnAnotherNode(t *testing.T) {
	const sandboxID = "sb-resume-race-cross"
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return nil, pausesnap.ErrNotFound
	})
	origGet := getSandboxProxyMapFn
	t.Cleanup(func() { getSandboxProxyMapFn = origGet })
	getSandboxProxyMapFn = func(context.Context, string) (*proxytypes.SandboxProxyMap, bool) {
		return &proxytypes.SandboxProxyMap{SandboxID: sandboxID, HostIP: "10.0.0.2"}, true
	}
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.1"})

	var probed []string
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP })
	patches.ApplyFunc(cubelet.List, func(_ context.Context, addr string, _ *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		probed = append(probed, addr)
		if addr != "10.0.0.2" {
			return &cubebox.ListCubeSandboxResponse{}, nil
		}
		return &cubebox.ListCubeSandboxResponse{Items: []*cubebox.CubeSandbox{{
			Id:         sandboxID,
			Containers: []*cubebox.Container{{Id: sandboxID, State: cubebox.ContainerState_CONTAINER_RUNNING}},
		}}}, nil
	})

	rsp := resumeFromPauseSnapshot(context.Background(),
		&types.UpdateRequest{SandboxID: sandboxID, Action: "resume"}, "10.0.0.1")
	require.Equal(t, int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid)), rsp.Ret.RetCode)
	require.Contains(t, rsp.Ret.RetMsg, "is already running")
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, probed)
	require.Equal(t, "10.0.0.2", localcache.GetSandboxCache(sandboxID).HostIP)
}
