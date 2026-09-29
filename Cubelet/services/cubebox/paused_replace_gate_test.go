// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
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

func gateOnlyService(sb *cubeboxstore.CubeBox, ttl time.Duration) *service {
	mgr := &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}
	mgr.SetShimIntentTTL(ttl)
	return &service{cubeboxMgr: mgr}
}

// fakeCreateFlow stands in for the create workflow so a test can tell whether
// Create reached the flow at all.
type fakeCreateFlow struct {
	createCalls  int
	destroyCalls int
}

func (f *fakeCreateFlow) ID() string                                     { return "fake-create" }
func (f *fakeCreateFlow) Init(context.Context, *workflow.InitInfo) error { return nil }
func (f *fakeCreateFlow) Create(context.Context, *workflow.CreateContext) error {
	f.createCalls++
	return nil
}
func (f *fakeCreateFlow) Destroy(context.Context, *workflow.DestroyContext) error {
	f.destroyCalls++
	return nil
}
func (f *fakeCreateFlow) CleanUp(context.Context, *workflow.CleanContext) error { return nil }

func createServiceForTest(sb *cubeboxstore.CubeBox, flow *fakeCreateFlow, ttl time.Duration) *service {
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
		cubeboxManger: &fakeCubeboxAPI{cb: sb},
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

	err := gateOnlyService(sb, 10*time.Minute).
		gatePausedReplace(context.Background(), resumeReplaceRequest("sb-resume"))
	require.Error(t, err, "a paused sandbox whose shim may still hold the tap must not be replaced")
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
	require.NoError(t, gateOnlyService(sb, 10*time.Minute).
		gatePausedReplace(context.Background(), resumeReplaceRequest("sb-resume")))
}

// The gate only owns the PAUSED-replacement case. Everything else has to fall
// through untouched, or an ordinary create would start waiting on a live
// sandbox.
func TestGatePausedReplaceIgnoresEverythingButAPausedReplacement(t *testing.T) {
	running := newCubeboxWithStatusForTest("sb-run", cubeboxstore.Status{StartedAt: 1})
	running.SandboxID = "sb-run"
	s := gateOnlyService(running, time.Minute)

	assert.NoError(t, s.gatePausedReplace(context.Background(), resumeReplaceRequest("")),
		"a create without the desired-id annotation is not a replacement")
	assert.NoError(t, s.gatePausedReplace(context.Background(), resumeReplaceRequest("sb-run")),
		"a live sandbox is rejected downstream as already exists, never waited on")
	assert.NoError(t, s.gatePausedReplace(context.Background(), resumeReplaceRequest("sb-elsewhere")),
		"a desired id this node does not hold is an ordinary create")
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
}
