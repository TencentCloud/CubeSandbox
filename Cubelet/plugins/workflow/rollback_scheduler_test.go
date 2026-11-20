// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

type decliningRollback struct{}

func (decliningRollback) Schedule(context.Context, *CreateContext) bool { return false }

type fallbackDestroy struct {
	recordingDestroyStep
	called chan string
}

func (f *fallbackDestroy) Destroy(_ context.Context, opts *DestroyContext) error {
	f.called <- opts.SandboxID
	return nil
}

func TestDeclinedRollbackRetainsGenericFailover(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	f := &fallbackDestroy{called: make(chan string, 1)}
	e := &Engine{}
	e.AddFlow(flow_destroy, &Workflow{Name: flow_destroy, Limiter: semaphore.NewLimiter(1), Steps: []*Step{{Name: "destroy", Actions: []Flow{f}}}})
	ctx := namespaces.WithNamespace(t.Context(), namespaces.Default)
	ctx = CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "declined"})
	e.scheduleFailover(ctx, &CreateContext{
		BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "allocated-id"},
		ReqInfo:          &cubeboxpb.RunCubeSandboxRequest{}, RollbackCoordinator: decliningRollback{},
	})
	select {
	case id := <-f.called:
		require.Equal(t, "allocated-id", id)
	case <-time.After(time.Second):
		t.Fatal("declined coordinator disabled generic cleanup")
	}
}
