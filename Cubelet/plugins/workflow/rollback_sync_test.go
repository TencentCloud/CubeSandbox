// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// destroyRecordingStepFlow records Destroy calls and can fail them, so a test
// can drive engine.Rollback down its success and failure branches. When order
// is non-nil, the cleanup handoff (cleanUp → cleanupFlow.Create) appends
// "cleanup" so a test can assert afterDestroy ran first.
type destroyRecordingStepFlow struct {
	id          string
	destroyErr  error
	destroyCall int
	cleanupCall int
	order       *[]string
}

func (f *destroyRecordingStepFlow) ID() string                            { return f.id }
func (f *destroyRecordingStepFlow) Init(context.Context, *InitInfo) error { return nil }
func (f *destroyRecordingStepFlow) Create(context.Context, *CreateContext) error {
	f.cleanupCall++
	if f.order != nil {
		*f.order = append(*f.order, "cleanup")
	}
	return nil
}
func (f *destroyRecordingStepFlow) Destroy(context.Context, *DestroyContext) error {
	f.destroyCall++
	return f.destroyErr
}
func (f *destroyRecordingStepFlow) CleanUp(context.Context, *CleanContext) error { return nil }

func rollbackTestEngine(flow *destroyRecordingStepFlow) *Engine {
	e := &Engine{}
	wf := &Workflow{Name: flow_destroy, Limiter: semaphore.NewLimiter(8)}
	wf.AppendStep(&Step{Name: "step-1", Actions: []Flow{flow}})
	e.AddFlow(flow_destroy, wf)
	e.AddCleaupFlow(flow)
	return e
}

func rollbackTestCtx() context.Context {
	ctx := namespaces.WithNamespace(context.Background(), "default")
	return CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "req-rollback"})
}

// TestRollbackRunsAfterDestroyOnSuccess pins the synchronous-rollback contract
// service.Create depends on: Rollback tears the failed attempt down and then
// runs afterDestroy (the paused-tombstone restore) before returning — on the
// caller's goroutine, while its lifecycle lock is still held.
func TestRollbackRunsAfterDestroyOnSuccess(t *testing.T) {
	_, err := config.Init("", true)
	if err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	flow := &destroyRecordingStepFlow{id: "destroy-ok"}
	e := rollbackTestEngine(flow)

	afterRan := false
	e.Rollback(rollbackTestCtx(), &CreateContext{}, func() { afterRan = true })

	if flow.destroyCall != 1 {
		t.Fatalf("destroy calls = %d, want 1", flow.destroyCall)
	}
	if !afterRan {
		t.Fatal("afterDestroy must run synchronously inside Rollback")
	}
	if flow.cleanupCall != 0 {
		t.Fatalf("cleanup calls = %d, want 0 on a successful teardown", flow.cleanupCall)
	}
}

// TestRollbackAfterDestroyRunsBeforeCleanupHandoff pins the defer ordering on
// the failure branch. A failed destroy hands over to the cleanup flow twice:
// once inside run's destroy-error path (pre-existing, it sees the attempt's
// debris — the gc stamp it may leave is scrubbed by the restore) and once via
// Rollback's own defer. The contract this test pins is that afterDestroy (the
// tombstone restore) runs between them: the LAST cleanup handoff — the one a
// later gc tick acts on — always observes the restored tombstone, never the
// debris (the gc plugin's failover+PAUSED stamp-skip depends on it).
func TestRollbackAfterDestroyRunsBeforeCleanupHandoff(t *testing.T) {
	_, err := config.Init("", true)
	if err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	flow := &destroyRecordingStepFlow{id: "destroy-fail", destroyErr: errors.New("shim gone")}
	order := []string{}
	flow.order = &order
	e := rollbackTestEngine(flow)

	e.Rollback(rollbackTestCtx(), &CreateContext{}, func() { order = append(order, "afterDestroy") })

	want := []string{"cleanup", "afterDestroy", "cleanup"}
	if len(order) != len(want) {
		t.Fatalf("event order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("event order = %v, want %v", order, want)
		}
	}
}

// TestRollbackPropagatesKeepsRow pins the CreateContext→DestroyContext
// handoff: the rollback destroy of a Create-from-pause must carry
// RollbackKeepsRow so the cubebox destroy keeps the store row for the
// synchronous tombstone restore (#2042's no-absent-row contract).
func TestRollbackPropagatesKeepsRow(t *testing.T) {
	_, err := config.Init("", true)
	if err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	flow := &destroyRecordingStepFlow{id: "destroy-keepsrow"}

	var got *DestroyContext
	recording := &destroyOptCaptureFlow{inner: flow, captured: &got}
	wf := &Workflow{Name: flow_destroy, Limiter: semaphore.NewLimiter(8)}
	wf.AppendStep(&Step{Name: "step-capture", Actions: []Flow{recording}})
	e := &Engine{}
	e.AddFlow(flow_destroy, wf)

	opts := &CreateContext{}
	opts.RollbackKeepsRow = true
	e.Rollback(rollbackTestCtx(), opts, nil)

	if got == nil {
		t.Fatal("the destroy flow never ran")
	}
	if !got.RollbackKeepsRow {
		t.Fatal("RollbackKeepsRow must propagate from the create context to the rollback destroy")
	}
	if !got.IsRollBack {
		t.Fatal("the rollback destroy must still carry IsRollBack")
	}
}

type destroyOptCaptureFlow struct {
	inner    Flow
	captured **DestroyContext
}

func (f *destroyOptCaptureFlow) ID() string { return f.inner.ID() }
func (f *destroyOptCaptureFlow) Init(ctx context.Context, o *InitInfo) error {
	return f.inner.Init(ctx, o)
}
func (f *destroyOptCaptureFlow) Create(ctx context.Context, o *CreateContext) error {
	return f.inner.Create(ctx, o)
}
func (f *destroyOptCaptureFlow) Destroy(ctx context.Context, o *DestroyContext) error {
	*f.captured = o
	return f.inner.Destroy(ctx, o)
}
func (f *destroyOptCaptureFlow) CleanUp(ctx context.Context, o *CleanContext) error {
	return f.inner.CleanUp(ctx, o)
}
