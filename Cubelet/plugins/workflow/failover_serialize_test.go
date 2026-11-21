// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// failoverRecordingFlow records the DestroyContext the failover rollback
// delivers to the destroy flow and the order of the serialization hook.
type failoverRecordingFlow struct {
	mu    sync.Mutex
	order []string
	got   *DestroyContext
}

func (f *failoverRecordingFlow) ID() string                            { return "failover-recording" }
func (f *failoverRecordingFlow) Init(context.Context, *InitInfo) error { return nil }
func (f *failoverRecordingFlow) Create(context.Context, *CreateContext) error {
	return nil
}
func (f *failoverRecordingFlow) CleanUp(context.Context, *CleanContext) error { return nil }

func (f *failoverRecordingFlow) Destroy(_ context.Context, opts *DestroyContext) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "destroy")
	f.got = opts
	return nil
}

func (f *failoverRecordingFlow) snapshot() ([]string, *DestroyContext) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...), f.got
}

func newFailoverTestEngine(t *testing.T, flow *failoverRecordingFlow) *Engine {
	t.Helper()
	_, err := config.Init("", true)
	require.NoError(t, err)
	e := &Engine{}
	e.AddFlow("destroy", &Workflow{
		Name:    "destroy",
		Limiter: semaphore.NewLimiter(1),
		Steps: []*Step{{
			Name:    flow.ID(),
			Actions: []Flow{flow},
		}},
	})
	return e
}

func failoverTestContext() context.Context {
	ctx := namespaces.WithNamespace(context.Background(), "ns-failover-test")
	return CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{
		Action:    "Create",
		RequestID: "req-failover-test",
	})
}

// TestFailoverSerializesRollbackWithLifecycleLock pins the ordering the
// tombstone restore relies on: the async failover re-acquires the per-sandbox
// serialization hook BEFORE the destroy flow runs and releases it only after,
// so a retried Resume or user Delete (which take the same lock) can never
// interleave between the rollback's teardown and its restore.
func TestFailoverSerializesRollbackWithLifecycleLock(t *testing.T) {
	flow := &failoverRecordingFlow{}
	e := newFailoverTestEngine(t, flow)

	snap := &cubeboxstore.CubeBox{}
	snap.ID = "sb-failover"
	snap.SandboxID = "sb-failover"

	opts := &CreateContext{}
	opts.SandboxID = "sb-failover"
	opts.PausedReplaceSnapshot = snap
	opts.RollbackSerialize = func(_ context.Context, sandboxID string) (func(), error) {
		assert.Equal(t, "sb-failover", sandboxID)
		flow.mu.Lock()
		flow.order = append(flow.order, "lock")
		flow.mu.Unlock()
		return func() {
			flow.mu.Lock()
			flow.order = append(flow.order, "unlock")
			flow.mu.Unlock()
		}, nil
	}

	e.failover(failoverTestContext(), opts)

	order, got := flow.snapshot()
	require.NotNil(t, got, "the failover must run the destroy flow")
	assert.Equal(t, []string{"lock", "destroy", "unlock"}, order,
		"the rollback destroy must run entirely inside the serialization hook")
	assert.True(t, got.IsRollBack)
	assert.Same(t, snap, got.PausedReplaceSnapshot,
		"the tombstone snapshot must ride along for the restore")
}

// TestFailoverStandsDownWhenItCannotSerialize pins the degradation path:
// if the lifecycle lock cannot be acquired (a concurrent lifecycle operation
// owns the sandbox for longer than the rollback's wait budget), the rollback
// must NOT run an unsynchronized teardown — that would delete the row a
// retry's quota gate keys on (re-opening #2042) and could kill a runtime the
// lock holder is building. It stands down entirely and hands the snapshot to
// RollbackStanddown, which records the restore as pending so the next
// lifecycle operation on the sandbox completes it.
func TestFailoverStandsDownWhenItCannotSerialize(t *testing.T) {
	flow := &failoverRecordingFlow{}
	e := newFailoverTestEngine(t, flow)

	snap := &cubeboxstore.CubeBox{}
	snap.ID = "sb-failover-busy"
	snap.SandboxID = "sb-failover-busy"

	var standdownID string
	var standdownCalls int
	opts := &CreateContext{}
	opts.SandboxID = "sb-failover-busy"
	opts.PausedReplaceSnapshot = snap
	opts.PausedReplaceDebrisCreatedAt = 12345
	opts.RollbackSerialize = func(context.Context, string) (func(), error) {
		return nil, errors.New("context deadline exceeded")
	}
	opts.RollbackStanddown = func(_ context.Context, sandboxID string) {
		standdownCalls++
		standdownID = sandboxID
	}

	e.failover(failoverTestContext(), opts)

	order, got := flow.snapshot()
	assert.Nil(t, got, "an unserialized rollback must not run the destroy flow at all")
	assert.Empty(t, order)
	assert.Equal(t, 1, standdownCalls, "the snapshot must be handed off, not discarded")
	assert.Equal(t, "sb-failover-busy", standdownID)
}

// TestFailoverWithoutSerializeHookKeepsOrdinaryRollback pins that creates
// which are not Create-from-pause (no hook, no snapshot) roll back exactly as
// before: plain IsRollBack destroy, nothing to restore.
func TestFailoverWithoutSerializeHookKeepsOrdinaryRollback(t *testing.T) {
	flow := &failoverRecordingFlow{}
	e := newFailoverTestEngine(t, flow)

	opts := &CreateContext{}
	opts.SandboxID = "sb-plain-create"

	e.failover(failoverTestContext(), opts)

	order, got := flow.snapshot()
	require.NotNil(t, got)
	assert.Equal(t, []string{"destroy"}, order, "no hook, no lock step")
	assert.True(t, got.IsRollBack)
	assert.Nil(t, got.PausedReplaceSnapshot)
}
