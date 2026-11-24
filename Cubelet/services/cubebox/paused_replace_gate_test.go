// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// pausedForReplace builds the record a resume leaves behind: reused sandbox ID,
// PAUSED status, and whatever endpoint the pause left recorded.
func pausedForReplace(id string, endpoint sandboxstore.Endpoint) *cubeboxstore.CubeBox {
	sb := newCubeboxWithStatusForTest(id, cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Unix(),
	})
	sb.SandboxID = id
	sb.Endpoint = endpoint
	return sb
}

func resumeReplaceRequest(desired string) *cubeboxpb.RunCubeSandboxRequest {
	req := &cubeboxpb.RunCubeSandboxRequest{
		RequestID: "resume-request",
		Containers: []*cubeboxpb.ContainerConfig{{
			Name:      "main",
			Resources: &cubeboxpb.Resource{Cpu: "1", Mem: "512Mi"},
		}},
	}
	if desired != "" {
		req.Annotations = map[string]string{constants.MasterAnnotationDesiredSandboxID: desired}
	}
	return req
}

func pauseResumeRequest(desired string) *cubeboxpb.RunCubeSandboxRequest {
	req := resumeReplaceRequest(desired)
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-pause"
	return req
}

func gateOnlyService(sb *cubeboxstore.CubeBox, ttl time.Duration) *service {
	return gateServiceForAPI(&fakeCubeboxAPI{cb: sb}, ttl)
}

func skipPauseExpansion(t *testing.T) {
	t.Helper()
	previous := expandPauseSnapshotPackageFn
	expandPauseSnapshotPackageFn = func(*cubeboxpb.RunCubeSandboxRequest) error { return nil }
	t.Cleanup(func() { expandPauseSnapshotPackageFn = previous })
}

func gateServiceForAPI(api *fakeCubeboxAPI, ttl time.Duration) *service {
	mgr := &local{cubeboxManger: api}
	mgr.SetShimIntentTTL(ttl)
	return &service{cubeboxMgr: mgr}
}

// noShimManager makes the row-less probe see an explicit "no shim" instead of
// the test environment's missing manager.
func noShimManager(t *testing.T) {
	t.Helper()
	previous := lookupShimBundleByID
	lookupShimBundleByID = func(*local, context.Context, string) (string, bool, error) {
		return "", false, nil
	}
	t.Cleanup(func() { lookupShimBundleByID = previous })
}

// fakeCreateFlow stands in for the create workflow so a test can tell whether
// Create reached the flow at all and what permission it carried.
type fakeCreateFlow struct {
	createCalls   int
	destroyCalls  int
	reclaimRootfs bool
}

func (f *fakeCreateFlow) ID() string                                     { return "fake-create" }
func (f *fakeCreateFlow) Init(context.Context, *workflow.InitInfo) error { return nil }
func (f *fakeCreateFlow) Create(_ context.Context, opts *workflow.CreateContext) error {
	f.createCalls++
	f.reclaimRootfs = opts.ReclaimStaleRootfs
	return nil
}
func (f *fakeCreateFlow) Destroy(context.Context, *workflow.DestroyContext) error {
	f.destroyCalls++
	return nil
}
func (f *fakeCreateFlow) CleanUp(context.Context, *workflow.CleanContext) error { return nil }

func createServiceForTest(sb *cubeboxstore.CubeBox, flow *fakeCreateFlow, ttl time.Duration) *service {
	return createServiceForAPI(&fakeCubeboxAPI{cb: sb}, flow, ttl)
}

func createServiceForAPI(api *fakeCubeboxAPI, flow *fakeCreateFlow, ttl time.Duration) *service {
	engine := &workflow.Engine{}
	engine.AddFlow("create", &workflow.Workflow{
		Name:    "create",
		Limiter: semaphore.NewLimiter(1),
		Steps: []*workflow.Step{{
			Name:    flow.ID(),
			Actions: []workflow.Flow{flow},
		}},
	})
	mgr := &local{
		config: &CubeConfig{
			DefaultRuntimeName: "io.containerd.cube.v2.task",
			Runtimes: map[string]cubeconfig.Runtime{
				"io.containerd.cube.v2.task": {Type: "io.containerd.cube.v2.task"},
			},
		},
		cubeboxManger: api,
	}
	mgr.SetShimIntentTTL(ttl)
	return &service{
		engine:                engine,
		cubeboxMgr:            mgr,
		sandboxLifecycleLocks: utils.NewResourceLocks(),
	}
}

// The gate's refusal is the same "still inside the shim-spawn intent TTL"
// failure the GC budget defers, so a transient refusal must be retryable.
func TestGatePausedReplaceRefusesWhileTheOldRuntimeMayLive(t *testing.T) {
	sb := pausedForReplace("sb-resume", sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now()})

	gate, err := gateOnlyService(sb, 10*time.Minute).
		gatePausedReplace(context.Background(), resumeReplaceRequest("sb-resume"))
	require.Error(t, err, "a paused sandbox whose shim may still hold the tap must not be replaced")
	assert.False(t, gate.reclaimRootfs, "a refused replacement must not authorize stale-rootfs cleanup")
	assert.True(t, pendingIntentOnlyError(t, err),
		"the refusal has to be the deferrable kind, or the retry budget would quarantine a healthy resume")
}

// Once the old runtime is provably gone the replacement proceeds; refusing
// forever would make a resume impossible.
func TestGatePausedReplaceAllowsWhenTheOldRuntimeIsGone(t *testing.T) {
	sb := pausedForReplace("sb-resume", sandboxstore.Endpoint{
		ShimSpawned:   true,
		ShimSpawnedAt: time.Now().Add(-time.Hour),
	})
	gate, err := gateOnlyService(sb, 10*time.Minute).
		gatePausedReplace(context.Background(), resumeReplaceRequest("sb-resume"))
	require.NoError(t, err)
	assert.False(t, gate.reclaimRootfs, "a replacement without a pause snapshot is not a resume")
}

func TestGatePausedReplaceAuthorizesReclaimForPauseResume(t *testing.T) {
	sb := pausedForReplace("sb-resume", sandboxstore.Endpoint{
		ShimSpawned:   true,
		ShimSpawnedAt: time.Now().Add(-time.Hour),
	})
	gate, err := gateOnlyService(sb, 10*time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.NoError(t, err)
	assert.True(t, gate.reclaimRootfs, "a pause resume with no live runtime may reclaim its stale rootfs")
}

// A failed resume can delete the paused tombstone before the final create step
// fails. The retry must still reclaim the stale rootfs via the host-level check.
func TestGatePausedReplaceAuthorizesReclaimWithoutLocalRecord(t *testing.T) {
	noShimManager(t)

	gate, err := gateOnlyService(nil, time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.NoError(t, err)
	assert.True(t, gate.reclaimRootfs)
}

// The no-row positive case must also cover the production not-found error, not
// only the fake's (nil, nil).
func TestGatePausedReplaceTreatsStoreNotFoundAsNoRow(t *testing.T) {
	noShimManager(t)

	gate, err := gateServiceForAPI(&fakeCubeboxAPI{getErr: utils.ErrorKeyNotFound}, time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.NoError(t, err)
	assert.True(t, gate.reclaimRootfs)
}

// A store read error is retryable and must not turn into permission to delete
// a rootfs, nor into a create that replaces a PAUSED row without the wait.
func TestGatePausedReplaceRefusesOnStoreReadError(t *testing.T) {
	gate, err := gateServiceForAPI(&fakeCubeboxAPI{getErr: errors.New("store closed")}, time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.Error(t, err)
	assert.False(t, gate.reclaimRootfs)
	status, ok := ret.FromError(err)
	require.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_TaskStateInvalid, status.Code())
}

// A row that claims a different sandbox id is a store inconsistency, not a
// missing row; do not turn it into permission to delete another sandbox's disk.
func TestGatePausedReplaceRejectsMismatchedRowID(t *testing.T) {
	mismatched := newCubeboxWithStatusForTest("sb-resume", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Unix(),
	})
	mismatched.SandboxID = "sb-other"

	gate, err := gateOnlyService(mismatched, time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.Error(t, err)
	assert.False(t, gate.reclaimRootfs)
	status, ok := ret.FromError(err)
	require.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_TaskStateInvalid, status.Code())
}

// The no-row probe must still fail closed while a shim/VMM from the failed
// attempt is alive; the missing store row is not proof that the disk is free.
func TestGatePausedReplaceRefusesWithoutLocalRecordWhileRuntimeLives(t *testing.T) {
	live := startSleeper(t)
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, shimPidFileName, live.Pid)
	previous := lookupShimBundleByID
	lookupShimBundleByID = func(*local, context.Context, string) (string, bool, error) {
		return bundle, true, nil
	}
	t.Cleanup(func() { lookupShimBundleByID = previous })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	gate, err := gateOnlyService(nil, time.Minute).
		gatePausedReplace(ctx, pauseResumeRequest("sb-resume"))
	require.Error(t, err)
	assert.False(t, gate.reclaimRootfs, "a live runtime must keep the stale rootfs protected even without a row")
}

// Workload container shims have random ids, but the packed pause spec still
// carries them. The row-less gate must check those, not only the pod id.
func TestGatePausedReplaceChecksWorkloadContainerShimsWithoutRow(t *testing.T) {
	live := startSleeper(t)
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, shimPidFileName, live.Pid)
	previous := lookupShimBundleByID
	lookupShimBundleByID = func(_ *local, _ context.Context, id string) (string, bool, error) {
		if id == "workload-1" {
			return bundle, true, nil
		}
		return "", false, nil
	}
	t.Cleanup(func() { lookupShimBundleByID = previous })

	req := pauseResumeRequest("sb-resume")
	req.Containers[0].Id = "workload-1"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	gate, err := gateOnlyService(nil, time.Minute).gatePausedReplace(ctx, req)
	require.Error(t, err)
	assert.False(t, gate.reclaimRootfs)
}

// A bundle with no readable pid files must age from its on-disk mtime, not
// from time.Now(), or a retry would keep the spawn intent fresh forever.
func TestGatePausedReplaceAgesStaleBundleWithoutPids(t *testing.T) {
	bundle := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(bundle, old, old))
	previous := lookupShimBundleByID
	lookupShimBundleByID = func(*local, context.Context, string) (string, bool, error) {
		return bundle, true, nil
	}
	t.Cleanup(func() { lookupShimBundleByID = previous })

	gate, err := gateOnlyService(nil, time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.NoError(t, err)
	assert.True(t, gate.reclaimRootfs, "a stale bundle may age out and allow the retry to heal")
}

// A registered shim with no bundle path has no timestamp to age from; it must
// fail closed instead of inventing a fresh intent.
func TestGatePausedReplaceRejectsShimWithoutBundle(t *testing.T) {
	previous := lookupShimBundleByID
	lookupShimBundleByID = func(*local, context.Context, string) (string, bool, error) {
		return "", true, nil
	}
	t.Cleanup(func() { lookupShimBundleByID = previous })

	gate, err := gateOnlyService(nil, time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.Error(t, err)
	assert.False(t, gate.reclaimRootfs)
}

// One unreadable shim bundle is enough to fail closed; a readable sibling must
// not hide the shim whose liveness is unknown.
func TestGatePausedReplaceRejectsMixedBundleVisibility(t *testing.T) {
	readable := t.TempDir()
	previous := lookupShimBundleByID
	lookupShimBundleByID = func(_ *local, _ context.Context, id string) (string, bool, error) {
		if id == "workload-1" {
			return readable, true, nil
		}
		return "", true, nil
	}
	t.Cleanup(func() { lookupShimBundleByID = previous })

	req := pauseResumeRequest("sb-resume")
	req.Containers[0].Id = "workload-1"
	gate, err := gateOnlyService(nil, time.Minute).gatePausedReplace(context.Background(), req)
	require.Error(t, err)
	assert.False(t, gate.reclaimRootfs)
}

// A shim lookup error is unknown, not "not present".
func TestGatePausedReplaceRefusesOnShimLookupError(t *testing.T) {
	previous := lookupShimBundleByID
	lookupShimBundleByID = func(*local, context.Context, string) (string, bool, error) {
		return "", false, errors.New("shim manager unavailable")
	}
	t.Cleanup(func() { lookupShimBundleByID = previous })

	gate, err := gateOnlyService(nil, time.Minute).
		gatePausedReplace(context.Background(), pauseResumeRequest("sb-resume"))
	require.Error(t, err)
	assert.False(t, gate.reclaimRootfs)
}

// The ownership check is what stops a previous attempt's failover from
// deleting a newer attempt's row, rootfs and shim.
func TestFailoverGuardOwnsOnlyTheAttemptThatWroteTheRow(t *testing.T) {
	sb := pausedForReplace("sb-resume", sandboxstore.Endpoint{})
	sb.CreateAttemptID = "attempt-2"
	guard := &failoverGuard{mgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	owned, err := guard.Owns(context.Background(), "sb-resume", "attempt-1")
	require.NoError(t, err)
	assert.False(t, owned, "a newer attempt's row must not be torn down by the old failover")

	owned, err = guard.Owns(context.Background(), "sb-resume", "attempt-2")
	require.NoError(t, err)
	assert.True(t, owned, "the attempt that wrote the row still owns cleanup")
}

func TestFailoverGuardKeepsLegacyAndMissingRows(t *testing.T) {
	legacy := pausedForReplace("sb-resume", sandboxstore.Endpoint{})
	guard := &failoverGuard{mgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: legacy}}}
	owned, err := guard.Owns(context.Background(), "sb-resume", "attempt-1")
	require.NoError(t, err)
	assert.True(t, owned, "rows written before the attempt id keep their cleanup")

	missing := &failoverGuard{mgr: &local{cubeboxManger: &fakeCubeboxAPI{}}}
	owned, err = missing.Owns(context.Background(), "sb-resume", "attempt-1")
	require.NoError(t, err)
	assert.True(t, owned, "a row the failed attempt already removed still has network/storage to release")

	failing := &failoverGuard{mgr: &local{cubeboxManger: &fakeCubeboxAPI{getErr: errors.New("store closed")}}}
	owned, err = failing.Owns(context.Background(), "sb-resume", "attempt-1")
	require.Error(t, err)
	assert.False(t, owned, "a store error is unknown and must fail closed")
}

// The gate only owns the PAUSED-replacement case. Everything else has to fall
// through untouched, or an ordinary create would start waiting on a live
// sandbox.
func TestGatePausedReplaceIgnoresEverythingButAPausedReplacement(t *testing.T) {
	running := newCubeboxWithStatusForTest("sb-run", cubeboxstore.Status{StartedAt: 1})
	running.SandboxID = "sb-run"
	s := gateOnlyService(running, time.Minute)

	gate, err := s.gatePausedReplace(context.Background(), resumeReplaceRequest(""))
	assert.NoError(t, err, "a create without the desired-id annotation is not a replacement")
	assert.False(t, gate.reclaimRootfs)

	gate, err = s.gatePausedReplace(context.Background(), resumeReplaceRequest("sb-run"))
	assert.NoError(t, err, "a live sandbox is rejected downstream as already exists, never waited on")
	assert.False(t, gate.reclaimRootfs)

	gate, err = s.gatePausedReplace(context.Background(), resumeReplaceRequest("sb-elsewhere"))
	assert.NoError(t, err, "a desired id this node does not hold is an ordinary create")
	assert.False(t, gate.reclaimRootfs)
}

// A refusal has to happen before the create flow runs. That is the whole point
// of moving the gate: the flow allocates network and volume, and the only way
// to fail after that without stranding them was a code that triggers the
// failover Destroy — which wipes the PAUSED sandbox this path exists to keep.
func TestCreateRefusesPausedReplacementBeforeTheFlowRuns(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)

	sb := pausedForReplace("sb-resume", sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now()})
	flow := &fakeCreateFlow{}
	s := createServiceForTest(sb, flow, 10*time.Minute)

	rsp, err := s.Create(context.Background(), resumeReplaceRequest("sb-resume"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_PreConditionFailed, rsp.Ret.RetCode,
		"PreConditionFailed skips create-flow failover, so the paused sandbox is not destroyed")
	assert.Zero(t, flow.createCalls, "the gate must run before the flow allocates anything")
	assert.Zero(t, flow.destroyCalls, "a refused resume must not roll the sandbox back")
}

// A store read error is retryable and must stop before the flow can replace
// the pending row or delete anything.
func TestCreateRefusesStoreReadErrorBeforeTheFlowRuns(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	skipPauseExpansion(t)

	flow := &fakeCreateFlow{}
	s := createServiceForAPI(&fakeCubeboxAPI{getErr: errors.New("store closed")}, flow, time.Minute)

	rsp, err := s.Create(context.Background(), pauseResumeRequest("sb-resume"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_TaskStateInvalid, rsp.Ret.RetCode)
	assert.Zero(t, flow.createCalls)
	assert.Zero(t, flow.destroyCalls)
}

// The same request with the old runtime provably gone reaches the flow
// normally, so the gate is a gate and not a wall.
func TestCreateReachesTheFlowOnceTheOldRuntimeIsGone(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)

	sb := pausedForReplace("sb-resume", sandboxstore.Endpoint{
		ShimSpawned:   true,
		ShimSpawnedAt: time.Now().Add(-time.Hour),
	})
	flow := &fakeCreateFlow{}
	s := createServiceForTest(sb, flow, 10*time.Minute)

	rsp, err := s.Create(context.Background(), resumeReplaceRequest("sb-resume"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Equal(t, 1, flow.createCalls)
	assert.False(t, flow.reclaimRootfs)
}

// A resume that fails before the final create step must still own the paused
// tombstone, or its failover would mistake the old row for a newer attempt and
// skip network/volume/storage cleanup.
func TestCreateClaimsPausedRowBeforeTheFlowRuns(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	skipPauseExpansion(t)

	sb := pausedForReplace("sb-resume", sandboxstore.Endpoint{
		ShimSpawned:   true,
		ShimSpawnedAt: time.Now().Add(-time.Hour),
	})
	sb.CreateAttemptID = "old-attempt"
	api := &fakeCubeboxAPI{cb: sb}
	flow := &fakeCreateFlow{}
	s := createServiceForAPI(api, flow, 10*time.Minute)

	rsp, err := s.Create(context.Background(), pauseResumeRequest("sb-resume"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	require.Equal(t, 1, flow.createCalls)
	require.NotNil(t, api.cb)
	assert.NotEmpty(t, api.cb.CreateAttemptID)
	assert.NotEqual(t, "old-attempt", api.cb.CreateAttemptID,
		"the attempt that is about to run must own the row for failover")

	owned, err := (&failoverGuard{mgr: &local{cubeboxManger: api}}).
		Owns(context.Background(), "sb-resume", api.cb.CreateAttemptID)
	require.NoError(t, err)
	assert.True(t, owned, "the claimed row belongs to the failing attempt's cleanup")
}

// The gate→storage assignment is the line that makes the fix work. A pause
// resume must carry the permission into the create context.
func TestCreatePropagatesReclaimPermission(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	noShimManager(t)
	skipPauseExpansion(t)

	flow := &fakeCreateFlow{}
	s := createServiceForTest(nil, flow, time.Minute)

	rsp, err := s.Create(context.Background(), pauseResumeRequest("sb-resume"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	require.Equal(t, 1, flow.createCalls)
	assert.True(t, flow.reclaimRootfs, "the gate's permission must reach storage")
}
