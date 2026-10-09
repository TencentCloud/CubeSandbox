// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telemetry"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestRollbackSandboxEarlyRejectionsAreTraced(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)

	cases := []struct {
		name string
		req  *cubeboxv1.RollbackSandboxRequest
		want errorcode.ErrorCode
	}{
		{
			name: "empty sandbox id",
			req:  &cubeboxv1.RollbackSandboxRequest{SnapshotID: "snap-1", NewGen: 2},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "empty snapshot id",
			req:  &cubeboxv1.RollbackSandboxRequest{SandboxID: "sb-1", NewGen: 2},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "unsafe snapshot id",
			req:  &cubeboxv1.RollbackSandboxRequest{SandboxID: "sb-1", SnapshotID: "bad/id", NewGen: 2},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "missing new gen",
			req:  &cubeboxv1.RollbackSandboxRequest{SandboxID: "sb-1", SnapshotID: "snap-1"},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "cow storage disabled",
			req:  &cubeboxv1.RollbackSandboxRequest{SandboxID: "sb-1", SnapshotID: "snap-1", NewGen: 2},
			want: errorcode.ErrorCode_PreConditionFailed,
		},
	}

	for _, tc := range cases {
		rsp, err := (&service{}).RollbackSandbox(context.Background(), tc.req)
		require.NoError(t, err, tc.name)
		require.NotNil(t, rsp, tc.name)
		require.Equal(t, tc.want, rsp.GetRet().GetRetCode(), tc.name)
	}
	flush()

	spans := rec.named(telemetry.SpanRollback)
	require.Len(t, spans, len(cases), "one root span per rejected request")
	for i, span := range spans {
		assert.Equal(t, codes.Error, span.Status().Code, cases[i].name)
		got, ok := retCodeAttr(span)
		require.True(t, ok, "%s: span carries no %s", cases[i].name, telemetry.AttrRetCode)
		assert.Equal(t, int(cases[i].want), got, cases[i].name)
		if cases[i].req.GetSandboxID() != "" {
			var sandboxID string
			for _, kv := range span.Attributes() {
				if string(kv.Key) == telemetry.AttrSandboxID {
					sandboxID = kv.Value.AsString()
				}
			}
			assert.Equal(t, cases[i].req.GetSandboxID(), sandboxID, cases[i].name)
		}
	}
}

const rollbackTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestRollbackSandboxAdoptsIncomingTraceparent(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("traceparent", rollbackTraceparent))

	rsp, err := (&service{}).RollbackSandbox(ctx, &cubeboxv1.RollbackSandboxRequest{
		SandboxID:  "sb-1",
		SnapshotID: "snap-1",
		NewGen:     2,
	})
	require.NoError(t, err)
	require.NotNil(t, rsp)
	flush()

	spans := rec.named(telemetry.SpanRollback)
	require.Len(t, spans, 1)
	parent := spans[0].Parent()
	require.True(t, parent.IsValid(), "rollback must continue the caller's trace")
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", parent.TraceID().String())
	assert.Equal(t, "00f067aa0ba902b7", parent.SpanID().String())
}

func TestEndRollbackTraceFinalizesOnPanic(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)

	run := func(code errorcode.ErrorCode, panics bool) {
		rsp := &cubeboxv1.RollbackSandboxResponse{Ret: &errorcode.Ret{RetCode: code}}
		func() {
			defer func() { _ = recover() }()
			_, root := telemetry.Start(context.Background(), telemetry.SpanRollback)
			_, stage := telemetry.Start(context.Background(), telemetry.SpanRollbackRestore)
			stagePtr := stage
			defer endRollbackTrace(&stagePtr, root, rsp)
			if panics {
				panic("boom")
			}
		}()
	}

	run(errorcode.ErrorCode_Success, false)
	run(errorcode.ErrorCode_Unknown, false)
	run(errorcode.ErrorCode_Success, true)
	flush()

	spans := rec.named(telemetry.SpanRollbackRestore)
	require.Len(t, spans, 3)
	assert.Equal(t, codes.Unset, spans[0].Status().Code, "success code is not a failure")
	assert.Equal(t, codes.Error, spans[1].Status().Code, "business failure is reported")
	assert.Equal(t, codes.Error, spans[2].Status().Code, "panicking phase is reported")
}

type spanCapturingTask struct {
	containerd.Task
	gotCtx context.Context
}

func (t *spanCapturingTask) Update(ctx context.Context, _ ...containerd.UpdateTaskOpts) error {
	t.gotCtx = ctx
	return nil
}

type stubContainer struct {
	containerd.Container
	task containerd.Task
}

func (c *stubContainer) Task(context.Context, cio.Attach) (containerd.Task, error) {
	return c.task, nil
}

// The shim reads the trace out of the ttrpc metadata containerd injects from
// this context, so the restore span and the containerd namespace must both
// survive the hop into task.Update.
func TestRollbackTaskContextCarriesSpanAndNamespaceToShim(t *testing.T) {
	setupSnapshotTracer(t)

	_, rootSpan := telemetry.Start(context.Background(), telemetry.SpanRollback)
	defer rootSpan.End()
	restoreCtx, restoreSpan := telemetry.Start(context.Background(), telemetry.SpanRollbackRestore)
	defer restoreSpan.End()

	task := &spanCapturingTask{}
	cb := newCubeboxWithStatusForTest("sb-ctx", cubeboxstore.Status{StartedAt: 1})
	cb.Namespace = "cube-ns"
	cb.FirstContainer().Container = &stubContainer{task: task}

	taskCtx, gotTask, err := (&service{}).taskForRollback(restoreCtx, cb)
	require.NoError(t, err)
	require.Same(t, task, gotTask)
	require.NoError(t, updateTaskForRollback(taskCtx, gotTask, "{}"))

	ns, ok := namespaces.Namespace(task.gotCtx)
	require.True(t, ok, "containerd namespace must be preserved on the task context")
	assert.Equal(t, "cube-ns", ns)
	got := trace.SpanContextFromContext(task.gotCtx)
	require.True(t, got.IsValid(), "shim task context must carry a span so containerd injects traceparent")
	assert.Equal(t, restoreSpan.SpanContext().SpanID(), got.SpanID())
}
