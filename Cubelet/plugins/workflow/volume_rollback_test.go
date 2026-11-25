// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
)

type volumeRollbackFlow struct {
	stubFlow
	createErr error
	receipts  chan<- *CreateContext
}

func (f volumeRollbackFlow) Create(_ context.Context, opts *CreateContext) error {
	opts.PluginVolumeIDs = []string{"vol-a"}
	return f.createErr
}

func (f volumeRollbackFlow) Destroy(_ context.Context, opts *DestroyContext) error {
	if opts.CreateVolumeRollback != nil {
		opts.CreateVolumeRollback.PluginVolumeRollback = []VolumeRefEvent{}
	}
	f.receipts <- opts.CreateVolumeRollback
	return nil
}

func TestCreateVolumeRollbackCompletesBeforeFailureReply(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	failure := errors.New("create failed after attaching the volume")
	receipts := make(chan *CreateContext, 1)
	e := newScopeEngine(flow_create)
	e.workflows[flow_create].AppendStep(&Step{Name: "attach", Actions: []Flow{
		volumeRollbackFlow{stubFlow: stubFlow{id: "attach"}, createErr: failure},
	}})
	destroy := newScopeEngine(flow_destroy).workflows[flow_destroy]
	destroy.AppendStep(&Step{Name: "detach", Actions: []Flow{
		volumeRollbackFlow{stubFlow: stubFlow{id: "detach"}, receipts: receipts},
	}})
	e.AddFlow(flow_destroy, destroy)

	opts := &CreateContext{BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "failed"}, Failover: true}
	err = e.Create(namespaces.WithNamespace(tracedCtx(), "default"), opts)
	require.ErrorIs(t, err, failure)
	select {
	case got := <-receipts:
		require.Same(t, opts, got)
		require.NotNil(t, opts.PluginVolumeRollback)
	default:
		t.Fatal("create returned before volume rollback completed")
	}
}
