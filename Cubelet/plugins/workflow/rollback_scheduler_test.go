// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/rollback"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
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
	e.failCreate(ctx, &CreateContext{
		BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "allocated-id"},
		ReqInfo:          &cubeboxpb.RunCubeSandboxRequest{}, Failover: true, RollbackCoordinator: decliningRollback{},
	})
	select {
	case id := <-f.called:
		require.Equal(t, "allocated-id", id)
	case <-time.After(time.Second):
		t.Fatal("declined coordinator disabled generic cleanup")
	}
}

type acceptingRollback struct{ calls int }

func (s *acceptingRollback) Schedule(context.Context, *CreateContext) bool {
	s.calls++
	return true
}

func TestRollbackSchedulingPreservesVolumeCleanupOwnership(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "declined", true: "accepted"}[accepted], func(t *testing.T) {
			receipts := make(chan *CreateContext, 1)
			e := newScopeEngine(flow_destroy)
			e.workflows[flow_destroy].AppendStep(&Step{Name: "detach", Actions: []Flow{
				volumeRollbackFlow{stubFlow: stubFlow{id: "detach"}, receipts: receipts},
			}})
			opts := &CreateContext{
				BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "failed-resume"},
				Failover:         true, PluginVolumeIDs: []string{"vol-a"}, RollbackCoordinator: decliningRollback{},
			}
			scheduler := &acceptingRollback{}
			if accepted {
				opts.RollbackCoordinator = scheduler
			}
			e.failCreate(namespaces.WithNamespace(tracedCtx(), namespaces.Default), opts)
			if accepted {
				require.Equal(t, 1, scheduler.calls)
				require.Empty(t, receipts, "coordinator-owned cleanup must not also enter generic failover")
				require.Nil(t, opts.PluginVolumeRollback)
			} else {
				select {
				case got := <-receipts:
					require.Same(t, opts, got)
					require.NotNil(t, opts.PluginVolumeRollback)
				default:
					t.Fatal("declined coordinator must preserve synchronous volume rollback")
				}
			}
		})
	}
}

func TestRollbackDestroyFailureDoesNotEnqueueGenericCleanup(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	for _, kind := range []string{"ordinary", "paused tombstone", "durable rollback"} {
		t.Run(kind, func(t *testing.T) {
			failure := errors.New("destroy failed")
			flow := &rollbackCleanupProbe{stubFlow: stubFlow{id: "probe"}, failure: failure}
			e := newScopeEngine(flow_destroy)
			e.workflows[flow_destroy].AppendStep(&Step{Name: "destroy", Actions: []Flow{flow}})
			e.AddCleaupFlow(flow)
			opts := &DestroyContext{BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "sandbox", IsRollBack: true}}
			switch kind {
			case "paused tombstone":
				opts.RestorePausedTombstone = &cubeboxstore.CubeBox{}
			case "durable rollback":
				opts.Rollback = &rollback.Task{}
			}
			require.ErrorIs(t, e.Destroy(tracedCtx(), opts), failure)
			require.Equal(t, kind == "ordinary", flow.cleanupEnqueued)
		})
	}
}

type rollbackCleanupProbe struct {
	stubFlow
	failure         error
	cleanupEnqueued bool
}

func (f *rollbackCleanupProbe) Destroy(context.Context, *DestroyContext) error { return f.failure }
func (f *rollbackCleanupProbe) Create(context.Context, *CreateContext) error {
	f.cleanupEnqueued = true
	return nil
}
