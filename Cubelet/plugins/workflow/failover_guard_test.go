// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type failoverRecordingFlow struct {
	destroyCalls int
}

func (f *failoverRecordingFlow) ID() string                                   { return "record-destroy" }
func (f *failoverRecordingFlow) Init(context.Context, *InitInfo) error        { return nil }
func (f *failoverRecordingFlow) Create(context.Context, *CreateContext) error { return nil }
func (f *failoverRecordingFlow) Destroy(context.Context, *DestroyContext) error {
	f.destroyCalls++
	return nil
}
func (f *failoverRecordingFlow) CleanUp(context.Context, *CleanContext) error { return nil }

type fakeFailoverGuard struct {
	owns     bool
	ownsErr  error
	locked   bool
	released bool
}

func (g *fakeFailoverGuard) Lock(context.Context, string) (func(), error) {
	g.locked = true
	return func() { g.released = true }, nil
}

func (g *fakeFailoverGuard) Owns(context.Context, string, string) (bool, error) {
	return g.owns, g.ownsErr
}

func failoverEngineForTest(t *testing.T) (*Engine, *failoverRecordingFlow) {
	t.Helper()
	_, err := config.Init("", true)
	require.NoError(t, err)

	flow := &failoverRecordingFlow{}
	wf := &Workflow{Name: flow_destroy, Limiter: semaphore.NewLimiter(1)}
	wf.AppendStep(&Step{Name: "step-1", Actions: []Flow{flow}})
	e := &Engine{}
	e.AddFlow(flow_destroy, wf)
	return e, flow
}

func failoverCreateContext(guard FailoverGuard) *CreateContext {
	return &CreateContext{
		BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "sb-resume"},
		Failover:         true,
		AttemptID:        "attempt-1",
		FailoverGuard:    guard,
	}
}

func failoverContext() context.Context {
	ctx := namespaces.WithNamespace(context.Background(), "default")
	return CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "req-1"})
}

// The previous attempt's async cleanup must not delete a row, rootfs or shim a
// newer attempt has already installed. The guard is the ownership boundary.
func TestFailoverSkipsWhenNewerAttemptOwnsSandbox(t *testing.T) {
	e, flow := failoverEngineForTest(t)
	guard := &fakeFailoverGuard{owns: false}

	e.failover(failoverContext(), failoverCreateContext(guard))

	require.True(t, guard.locked, "failover must take the lifecycle lock")
	require.True(t, guard.released, "the lock must be released")
	require.Zero(t, flow.destroyCalls, "a newer attempt's resources must not be destroyed")
}

// The positive control: when the failed attempt still owns the row, cleanup
// runs as before.
func TestFailoverDestroysWhenFailedAttemptOwnsSandbox(t *testing.T) {
	e, flow := failoverEngineForTest(t)
	guard := &fakeFailoverGuard{owns: true}

	e.failover(failoverContext(), failoverCreateContext(guard))

	require.True(t, guard.locked)
	require.True(t, guard.released)
	require.Equal(t, 1, flow.destroyCalls)
}
