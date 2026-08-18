// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	dbmodels "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestValidatePauseResumeVolumesEmptyOK(t *testing.T) {
	t.Parallel()
	require.NoError(t, validatePauseResumeVolumes(nil))
	require.NoError(t, validatePauseResumeVolumes([]string{"", "  "}))
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

// paused/running/notFound Cubelet probe stubs for pauseSandbox-level tests.
func patchProbeState(patches *gomonkey.Patches, state cubebox.ContainerState, found bool) {
	patches.ApplyFunc(probePauseLiveState,
		func(_ context.Context, _, _ string) (cubebox.ContainerState, bool) {
			return state, found
		})
}

// A pause whose earlier RPC timed out on the caller side but actually completed
// leaves a READY pause snapshot binding behind. The next auto-pause attempt must
// not surface a generic MasterParamsError — the caller (CLM) would treat that as
// a hard failure and roll the dataplane state back to "running", which strands
// the sandbox (paused backend, proxy thinks running → auto-resume never fires →
// HTTP 504). With the Cubelet confirmed PAUSED, pauseSandbox must instead report
// the idempotent "already in state" code so the caller converges to paused.
func TestPauseSandboxAlreadyPausedReturnsTaskStateInvalid(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetNodesByIp,
		func(_ string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(pausesnap.GetBySandbox,
		func(_ context.Context, sandboxID string) (*pausesnap.Record, error) {
			return &pausesnap.Record{SandboxID: sandboxID, SnapshotID: "snap-x", Status: "READY"}, nil
		})
	// Live Cubelet confirms PAUSED — safe to converge.
	patchProbeState(patches, cubebox.ContainerState_CONTAINER_PAUSED, true)
	// Begin reports the sandbox already has a READY pause snapshot binding.
	patches.ApplyFunc(pausesnap.Begin,
		func(_ context.Context, sandboxID, _, _, _ string) (string, error) {
			return "", fmt.Errorf("%w: sandbox %s already has pause snapshot snap-x",
				pausesnap.ErrAlreadyExists, sandboxID)
		})

	rsp := pauseSandbox(context.Background(), &types.UpdateRequest{
		SandboxID:    "sb-already-paused",
		InstanceType: "cubebox",
		Action:       "pause",
	}, "10.0.0.1")

	require.Equal(t,
		int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid)),
		rsp.Ret.RetCode)
	require.Contains(t, rsp.Ret.RetMsg, "already has pause snapshot")
	// The already-paused marker is the load-bearing cross-service contract:
	// CubeAPI keys its redundant-pause → HTTP 200 decision on this exact token.
	require.Contains(t, rsp.Ret.RetMsg, alreadyPausedMarker)
	require.Equal(t, "[cube:already-paused]", alreadyPausedMarker)
	// Pin the wire codes: CLM only treats these exact numbers as already-paused
	// (130490) vs. hard failure (130400). An enum/offset renumber would silently
	// break the cross-service contract that this fix depends on.
	require.Equal(t, 130490, int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid)))
	require.Equal(t, 130400, int(errorcode.ErrorCode_MasterParamsError))
}

// A stale READY binding on a sandbox that is actually RUNNING (resume succeeded
// but pausesnap.Delete failed) must NOT be reported as already-paused when the
// probe does not confirm PAUSED — otherwise CLM would converge a live sandbox to
// paused and brick it. Here the probe confirms RUNNING, so Begin's
// ErrAlreadyExists stays a hard MasterParamsError.
func TestPauseSandboxStaleReadyRunningReturnsParamsError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetNodesByIp,
		func(_ string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(pausesnap.GetBySandbox,
		func(_ context.Context, sandboxID string) (*pausesnap.Record, error) {
			return &pausesnap.Record{SandboxID: sandboxID, SnapshotID: "snap-x", Status: "READY"}, nil
		})
	// Sandbox is RUNNING — clearStalePauseBinding fires and Begin still races a
	// leftover binding; convergence must NOT happen.
	patchProbeState(patches, cubebox.ContainerState_CONTAINER_RUNNING, true)
	patches.ApplyFunc(clearStalePauseBinding,
		func(_ context.Context, _, _ string, _ *pausesnap.Record) {})
	patches.ApplyFunc(pausesnap.Begin,
		func(_ context.Context, sandboxID, _, _, _ string) (string, error) {
			return "", fmt.Errorf("%w: sandbox %s already has pause snapshot snap-x",
				pausesnap.ErrAlreadyExists, sandboxID)
		})

	rsp := pauseSandbox(context.Background(), &types.UpdateRequest{
		SandboxID:    "sb-stale-ready",
		InstanceType: "cubebox",
		Action:       "pause",
	}, "10.0.0.1")

	require.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
}

// An inconclusive probe (List error / container not found) on a READY binding
// must also stay a hard error — convergence requires a CONFIRMED PAUSED probe.
func TestPauseSandboxReadyProbeInconclusiveReturnsParamsError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetNodesByIp,
		func(_ string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(pausesnap.GetBySandbox,
		func(_ context.Context, sandboxID string) (*pausesnap.Record, error) {
			return &pausesnap.Record{SandboxID: sandboxID, SnapshotID: "snap-x", Status: "READY"}, nil
		})
	// Probe could not be completed (found=false) → inconclusive.
	patchProbeState(patches, cubebox.ContainerState_CONTAINER_UNKNOWN, false)
	patches.ApplyFunc(pausesnap.Begin,
		func(_ context.Context, sandboxID, _, _, _ string) (string, error) {
			return "", fmt.Errorf("%w: sandbox %s already has pause snapshot snap-x",
				pausesnap.ErrAlreadyExists, sandboxID)
		})

	rsp := pauseSandbox(context.Background(), &types.UpdateRequest{
		SandboxID:    "sb-probe-fail",
		InstanceType: "cubebox",
		Action:       "pause",
	}, "10.0.0.1")

	require.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
}

// Any other Begin failure keeps the generic MasterParamsError contract.
func TestPauseSandboxBeginGenericErrorReturnsParamsError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetNodesByIp,
		func(_ string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(pausesnap.GetBySandbox,
		func(_ context.Context, _ string) (*pausesnap.Record, error) {
			return nil, pausesnap.ErrNotFound
		})
	patchProbeState(patches, cubebox.ContainerState_CONTAINER_UNKNOWN, false)
	patches.ApplyFunc(pausesnap.Begin,
		func(_ context.Context, _, _, _, _ string) (string, error) {
			return "", fmt.Errorf("db down")
		})

	rsp := pauseSandbox(context.Background(), &types.UpdateRequest{
		SandboxID:    "sb-db-down",
		InstanceType: "cubebox",
		Action:       "pause",
	}, "10.0.0.1")

	require.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
	require.Contains(t, rsp.Ret.RetMsg, "db down")
}

// A FAILED pause binding left by a timed-out Master→Cubelet RPC that actually
// completed (Cubelet probes PAUSED) is the same stranded-504 symptom as the
// READY case. Begin returns a generic (non-ErrAlreadyExists) error for FAILED,
// but pauseSandbox must still converge it to the idempotent already-paused code
// so CLM pushes the dataplane to paused rather than rolling back to running.
func TestPauseSandboxFailedTimeoutCompletedReturnsTaskStateInvalid(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetNodesByIp,
		func(_ string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(pausesnap.GetBySandbox,
		func(_ context.Context, sandboxID string) (*pausesnap.Record, error) {
			return &pausesnap.Record{
				SandboxID:  sandboxID,
				SnapshotID: "snap-x",
				Status:     pausesnap.StatusFailed,
				LastError:  "rpc error: context deadline exceeded",
			}, nil
		})
	// Timed-out pause that finished on the Cubelet: probe confirms PAUSED.
	patchProbeState(patches, cubebox.ContainerState_CONTAINER_PAUSED, true)
	patches.ApplyFunc(pausesnap.Begin,
		func(_ context.Context, sandboxID, _, _, _ string) (string, error) {
			return "", fmt.Errorf("sandbox %s has pause snapshot snap-x in status FAILED", sandboxID)
		})
	completed := false
	patches.ApplyFunc(pausesnap.Complete,
		func(context.Context, string, string, string, string, string, []string) error {
			completed = true
			return nil
		})

	rsp := pauseSandbox(context.Background(), &types.UpdateRequest{
		SandboxID:    "sb-failed-timeout",
		InstanceType: "cubebox",
		Action:       "pause",
	}, "10.0.0.1")

	require.True(t, completed)
	require.Equal(t,
		int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid)),
		rsp.Ret.RetCode)
	require.Equal(t, 130490, int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid)))
	// FAILED-timeout convergence must also carry the marker so CubeAPI treats it
	// as an idempotent 200, same as the READY path.
	require.Contains(t, rsp.Ret.RetMsg, alreadyPausedMarker)
}

// A FAILED binding that is NOT a healed timeout (explicit non-timeout failure)
// must stay a hard MasterParamsError so the caller does not mask a real pause
// failure as idempotent success — even if the Cubelet happens to probe PAUSED.
func TestPauseSandboxFailedNotCompletedReturnsParamsError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetNodesByIp,
		func(_ string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(pausesnap.GetBySandbox,
		func(_ context.Context, sandboxID string) (*pausesnap.Record, error) {
			return &pausesnap.Record{
				SandboxID:  sandboxID,
				SnapshotID: "snap-x",
				Status:     pausesnap.StatusFailed,
				LastError:  "cubelet pause: disk full",
			}, nil
		})
	patchProbeState(patches, cubebox.ContainerState_CONTAINER_PAUSED, true)
	patches.ApplyFunc(pausesnap.Begin,
		func(_ context.Context, sandboxID, _, _, _ string) (string, error) {
			return "", fmt.Errorf("sandbox %s has pause snapshot snap-x in status FAILED", sandboxID)
		})

	rsp := pauseSandbox(context.Background(), &types.UpdateRequest{
		SandboxID:    "sb-failed-hard",
		InstanceType: "cubebox",
		Action:       "pause",
	}, "10.0.0.1")

	require.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
	require.Contains(t, rsp.Ret.RetMsg, "FAILED")
}

func TestPauseSandboxCreatingPausedHealsBinding(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetNodesByIp,
		func(_ string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(pausesnap.GetBySandbox,
		func(_ context.Context, sandboxID string) (*pausesnap.Record, error) {
			return &pausesnap.Record{
				SandboxID:  sandboxID,
				SnapshotID: "snap-x",
				NodeID:     "node-x",
				NodeIP:     "10.0.0.2",
				Status:     pausesnap.StatusCreating,
			}, nil
		})
	patchProbeState(patches, cubebox.ContainerState_CONTAINER_PAUSED, true)
	patches.ApplyFunc(pausesnap.Begin,
		func(_ context.Context, sandboxID, _, _, _ string) (string, error) {
			return "", fmt.Errorf("sandbox %s has pause snapshot snap-x in status CREATING", sandboxID)
		})
	completed := false
	patches.ApplyFunc(pausesnap.Complete,
		func(context.Context, string, string, string, string, string, []string) error {
			completed = true
			return nil
		})

	rsp := pauseSandbox(context.Background(), &types.UpdateRequest{
		SandboxID:    "sb-creating",
		InstanceType: "cubebox",
		Action:       "pause",
	}, "10.0.0.1")

	require.True(t, completed)
	require.Equal(t,
		int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid)),
		rsp.Ret.RetCode)
	require.Contains(t, rsp.Ret.RetMsg, alreadyPausedMarker)
}

// shouldTreatAsAlreadyPaused is the pure classifier the fix depends on. Exercise
// its real branches directly (no gomonkey) so the READY-needs-PAUSED gate and
// FAILED-needs-timeout+PAUSED gate are pinned independently of pauseSandbox.
func TestShouldTreatAsAlreadyPaused(t *testing.T) {
	t.Parallel()
	alreadyExists := fmt.Errorf("%w: snap-x", pausesnap.ErrAlreadyExists)
	failedGeneric := fmt.Errorf("sandbox sb has pause snapshot snap-x in status FAILED")
	timeoutRec := &pausesnap.Record{Status: pausesnap.StatusFailed, LastError: "rpc error: context deadline exceeded"}
	explicitRec := &pausesnap.Record{Status: pausesnap.StatusFailed, LastError: "cubelet pause: disk full"}
	creatingRec := &pausesnap.Record{Status: pausesnap.StatusCreating}
	readyRec := &pausesnap.Record{Status: "READY"}

	cases := []struct {
		name      string
		beginErr  error
		rec       *pausesnap.Record
		liveState cubebox.ContainerState
		liveFound bool
		want      bool
	}{
		{"ready+paused → converge", alreadyExists, readyRec, cubebox.ContainerState_CONTAINER_PAUSED, true, true},
		{"ready+running → hard error", alreadyExists, readyRec, cubebox.ContainerState_CONTAINER_RUNNING, true, false},
		{"ready+probe inconclusive → hard error", alreadyExists, readyRec, cubebox.ContainerState_CONTAINER_UNKNOWN, false, false},
		{"failed+timeout+paused → converge", failedGeneric, timeoutRec, cubebox.ContainerState_CONTAINER_PAUSED, true, true},
		{"failed+timeout+not paused → hard error", failedGeneric, timeoutRec, cubebox.ContainerState_CONTAINER_RUNNING, true, false},
		{"failed+explicit+paused → hard error", failedGeneric, explicitRec, cubebox.ContainerState_CONTAINER_PAUSED, true, false},
		{"creating+paused → converge", failedGeneric, creatingRec, cubebox.ContainerState_CONTAINER_PAUSED, true, true},
		{"creating+running → hard error", failedGeneric, creatingRec, cubebox.ContainerState_CONTAINER_RUNNING, true, false},
		{"nil rec+alreadyexists → hard error", alreadyExists, nil, cubebox.ContainerState_CONTAINER_PAUSED, true, false},
		{"nil rec, non-alreadyexists → hard error", failedGeneric, nil, cubebox.ContainerState_CONTAINER_PAUSED, true, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want,
				shouldTreatAsAlreadyPaused(tc.beginErr, tc.rec, tc.liveState, tc.liveFound))
		})
	}
}
