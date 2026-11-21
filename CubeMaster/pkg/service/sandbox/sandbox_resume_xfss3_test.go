// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/restoreplace"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type xfss3ResumeWorld struct {
	begun    []string
	released []string
	created  []string
	decided  int
	// holds is what the target answers when asked whether it has the
	// sandbox; holdsErr makes that question fail.
	holds    bool
	holdsErr error
}

// withXFSS3Resume runs resumeFromPauseSnapshot against an xfs-s3 binding on
// 10.0.0.1 that is remote ready. Placement would pick 10.0.0.3.
func withXFSS3Resume(t *testing.T, rec *pausesnap.Record, createRsp *cubebox.RunCubeSandboxResponse, createErr error) *xfss3ResumeWorld {
	t.Helper()
	w := &xfss3ResumeWorld{}
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return rec, nil
	})
	patches.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return nil, sandboxspec.ErrSandboxSpecNotFound
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP })

	prevDecide, prevBegin, prevFail, prevCreate, prevHolds := decidePauseResumePlacementFn, beginResumeAttemptFn, failResumeAttemptFn, createOnCubeletFn, targetHoldsLiveSandboxFn
	t.Cleanup(func() {
		decidePauseResumePlacementFn, beginResumeAttemptFn, failResumeAttemptFn, createOnCubeletFn, targetHoldsLiveSandboxFn = prevDecide, prevBegin, prevFail, prevCreate, prevHolds
	})
	targetHoldsLiveSandboxFn = func(context.Context, string, string) (bool, error) { return w.holds, w.holdsErr }
	decidePauseResumePlacementFn = func(context.Context, restoreplace.Input) (*restoreplace.Placement, error) {
		w.decided++
		return &restoreplace.Placement{NodeIP: "10.0.0.3", CrossNode: true}, nil
	}
	beginResumeAttemptFn = func(_ context.Context, _, target string) (*pausesnap.ResumeAttempt, error) {
		w.begun = append(w.begun, target)
		return &pausesnap.ResumeAttempt{Target: target, N: len(w.begun), Fresh: rec.ResumeState != pausesnap.ResumeInflight}, nil
	}
	failResumeAttemptFn = func(_ context.Context, _ string, a *pausesnap.ResumeAttempt) error {
		w.released = append(w.released, a.Target)
		return nil
	}
	createOnCubeletFn = func(_ context.Context, addr string, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
		w.created = append(w.created, addr+" cross_node="+req.GetAnnotations()[constants.CubeAnnotationSnapshotCrossNode])
		return createRsp, createErr
	}
	return w
}

func xfss3Binding(target, state string) *pausesnap.Record {
	return &pausesnap.Record{
		SandboxID: "sb-x", SnapshotID: "snap-x", NodeIP: "10.0.0.1", Status: "READY",
		Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusReady,
		ExportUUIDs:  `{"checkpoint":"cube-xfs-s3/snap-x/candidate.json","manifest_sha256":"abc"}`,
		ResumeTarget: target, ResumeState: state,
	}
}

func resumeX() *sandboxtypes.Res {
	return resumeFromPauseSnapshot(context.Background(), &sandboxtypes.UpdateRequest{SandboxID: "sb-x", Action: "resume"}, "10.0.0.1")
}

func TestXFSS3ResumeStaysOnTheUnresolvedTarget(t *testing.T) {
	w := withXFSS3Resume(t, xfss3Binding("10.0.0.2", pausesnap.ResumeInflight), nil, errors.New("deadline exceeded"))
	rsp := resumeX()
	require.NotEqual(t, 0, rsp.Ret.RetCode)
	require.Zero(t, w.decided, "placement must not be consulted while an attempt is unresolved")
	require.Equal(t, []string{"10.0.0.2"}, w.begun)
	require.Equal(t, []string{"10.0.0.2 cross_node=true"}, w.created)
	require.Empty(t, w.released, "no answer keeps the pin")
}

func TestXFSS3ResumeRecordsThePlacedTarget(t *testing.T) {
	w := withXFSS3Resume(t, xfss3Binding("", ""), nil, errors.New("connection refused"))
	resumeX()
	require.Equal(t, 1, w.decided)
	require.Equal(t, []string{"10.0.0.3"}, w.begun)
	require.Equal(t, []string{"10.0.0.3 cross_node=true"}, w.created)
}

func TestXFSS3ResumeReleasesOnlyOnADefinitiveFailure(t *testing.T) {
	failed := &cubebox.RunCubeSandboxResponse{Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_CreateStorageFailed, RetMsg: "materialize failed"}}
	w := withXFSS3Resume(t, xfss3Binding("10.0.0.2", pausesnap.ResumeInflight), failed, nil)
	resumeX()
	require.Equal(t, []string{"10.0.0.2"}, w.released)

	busy := &cubebox.RunCubeSandboxResponse{Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_TaskStateInvalid, RetMsg: "lifecycle operation is in progress"}}
	w = withXFSS3Resume(t, xfss3Binding("10.0.0.2", pausesnap.ResumeInflight), busy, nil)
	resumeX()
	require.Empty(t, w.released, "another Create of the same attempt may still run")
}

func TestXFSS3ResumeKeepsThePinWhileTheTargetHoldsTheSandbox(t *testing.T) {
	// An earlier attempt whose reply was lost started the sandbox; this one
	// fails on finding it. Releasing would let the next Resume start it on a
	// second node.
	exists := &cubebox.RunCubeSandboxResponse{Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_PreConditionFailed, RetMsg: "already exists"}}
	w := withXFSS3Resume(t, xfss3Binding("10.0.0.2", pausesnap.ResumeInflight), exists, nil)
	w.holds = true
	resumeX()
	require.Empty(t, w.released)

	w = withXFSS3Resume(t, xfss3Binding("10.0.0.2", pausesnap.ResumeInflight), exists, nil)
	w.holdsErr = errors.New("connection reset")
	resumeX()
	require.Empty(t, w.released, "a node that cannot say what it holds keeps the pin")
}

func TestXFSS3ResumeOnTheOriginReleasesThePinOverItsTombstone(t *testing.T) {
	// The origin always keeps the PAUSED tombstone of the pause. A Resume
	// that failed there leaves it as it was, and it runs nothing: keeping
	// the pin would send every later Resume to the origin, even once it is
	// drained or gone.
	failed := &cubebox.RunCubeSandboxResponse{Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_CreateStorageFailed, RetMsg: "restore failed"}}
	for state, release := range map[cubebox.ContainerState]bool{
		cubebox.ContainerState_CONTAINER_PAUSED:  true,
		cubebox.ContainerState_CONTAINER_RUNNING: false,
		cubebox.ContainerState_CONTAINER_EXITED:  false,
	} {
		w := withXFSS3Resume(t, xfss3Binding("10.0.0.1", pausesnap.ResumeInflight), failed, nil)
		targetHoldsLiveSandboxFn = cubeletHoldsLiveSandbox
		patches := gomonkey.ApplyFunc(cubelet.List, func(context.Context, string, *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
			return &cubebox.ListCubeSandboxResponse{Items: []*cubebox.CubeSandbox{{
				Id:         "sb-x",
				Containers: []*cubebox.Container{{Id: "sb-x", State: state}},
			}}}, nil
		})
		resumeX()
		patches.Reset()
		if release {
			require.Equal(t, []string{"10.0.0.1"}, w.released, state.String())
		} else {
			require.Empty(t, w.released, state.String())
		}
	}
}

func TestXFSS3ResumeRefusedWhenAnotherNodeHoldsTheAttempt(t *testing.T) {
	w := withXFSS3Resume(t, xfss3Binding("", ""), nil, nil)
	beginResumeAttemptFn = func(context.Context, string, string) (*pausesnap.ResumeAttempt, error) {
		return nil, pausesnap.ErrResumePinned
	}
	rsp := resumeX()
	require.NotEqual(t, 0, rsp.Ret.RetCode)
	require.Empty(t, w.created, "Create must not be sent when the attempt cannot be recorded")
}

func TestXFSS3ResumeOnOriginIsNotCrossNode(t *testing.T) {
	w := withXFSS3Resume(t, xfss3Binding("10.0.0.1", pausesnap.ResumeInflight), nil, errors.New("timeout"))
	resumeX()
	require.Equal(t, []string{"10.0.0.1 cross_node="}, w.created)
}

func TestXFSS3ResumeReleasesAFreshPinThatNeverLeftMaster(t *testing.T) {
	notSent := ret.Err(errorcode.ErrorCode_ConnHostFailed, "dial tcp 10.0.0.3:9999: connect: connection refused")
	w := withXFSS3Resume(t, xfss3Binding("", ""), nil, notSent)
	resumeX()
	require.Equal(t, []string{"10.0.0.3"}, w.released, "nothing can run on account of a request that never left")

	// The same error on an inherited pin says nothing about the earlier
	// attempt, which may have started the sandbox before the node went away.
	w = withXFSS3Resume(t, xfss3Binding("10.0.0.2", pausesnap.ResumeInflight), nil, notSent)
	resumeX()
	require.Empty(t, w.released)

	// A fresh pin whose request may have arrived keeps it.
	w = withXFSS3Resume(t, xfss3Binding("", ""), nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded"))
	resumeX()
	require.Empty(t, w.released)
}

func TestResumeNotSentRecognisesOnlyFailedDials(t *testing.T) {
	require.True(t, resumeNotSent(ret.Err(errorcode.ErrorCode_ConnHostFailed, "no conn")))
	require.True(t, resumeNotSent(status.Error(codes.Unavailable,
		`connection error: desc = "transport: Error while dialing: dial tcp 10.0.0.2:9999: connect: connection refused"`)))
	require.False(t, resumeNotSent(status.Error(codes.Unavailable, "error reading from server: EOF")))
	require.False(t, resumeNotSent(status.Error(codes.DeadlineExceeded, "context deadline exceeded")))
	require.False(t, resumeNotSent(errors.New("connection reset by peer")))
	require.False(t, resumeNotSent(nil))
}

func TestCrossNodeXFSS3ResumeRetiresTheCheckpointOnTheTargetToo(t *testing.T) {
	origin, packageNode := pausePackageNodes("10.0.0.1", "10.0.0.3", constants.SnapshotBackendXFSS3)
	require.Equal(t, "10.0.0.1", origin)
	require.Equal(t, []string{"10.0.0.1", "10.0.0.3"}, packageCleanupNodes(packageNode, "10.0.0.3", constants.SnapshotBackendXFSS3),
		"an origin that is down must not hold the remote checkpoint")

	_, packageNode = pausePackageNodes("10.0.0.1", "10.0.0.1", constants.SnapshotBackendXFSS3)
	require.Equal(t, []string{"10.0.0.1"}, packageCleanupNodes(packageNode, "10.0.0.1", constants.SnapshotBackendXFSS3))

	for _, backend := range []string{constants.SnapshotBackendXFS, constants.SnapshotBackendS3, constants.SnapshotBackendJFS} {
		_, packageNode = pausePackageNodes("10.0.0.1", "10.0.0.3", backend)
		require.Equal(t, []string{packageNode}, packageCleanupNodes(packageNode, "10.0.0.3", backend), backend)
	}
}
