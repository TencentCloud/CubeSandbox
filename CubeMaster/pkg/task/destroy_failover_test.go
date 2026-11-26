// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package task

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestCreateFailoverDestroyMissingNode(t *testing.T) {
	errorcode.InitCubeCodeRetryMap(&config.Config{CubeletConf: &config.CubeletConf{}})
	handler := &DestroySandboxTaskHandler{}
	handler.SetLoopMaxRetry(1)
	old := l
	l = &localTask{handles: map[OpType]taskHandler{DestroySandbox: handler}}
	defer func() { l = old }()

	for _, tc := range []struct {
		name           string
		createFailover bool
		cacheRestored  bool
		wantRPCs       int
		wantCleanups   int
	}{
		{name: "cache recovers", createFailover: true, cacheRestored: true, wantRPCs: 1, wantCleanups: 1},
		{name: "retry exhausted", createFailover: true},
		{name: "ordinary destroy", wantCleanups: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := gomonkey.NewPatches()
			defer p.Reset()
			p.ApplyFunc(reportMetric, func(*Task, error) {})
			p.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, f func(context.Context) error) error {
				return f(ctx)
			})
			p.ApplyFunc(sandboxspec.EnsureNoPendingRuntimeAttach, func(context.Context, string) error { return nil })
			p.ApplyFunc(pausesnap.TryDeletePaused, func(context.Context, string, string, string) (bool, error) { return false, nil })

			var cachedNode *node.Node
			p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return cachedNode, cachedNode != nil })
			var queued []*Task
			p.ApplyFunc(AddAsyncTask, func(t *Task) error {
				queued = append(queued, t)
				return nil
			})

			var destroys, proxyDeletes, cacheDeletes, hooks int
			p.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
				destroys++
				return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
			})
			p.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error { proxyDeletes++; return nil })
			p.ApplyFunc(localcache.DeleteSandboxCache, func(string) { cacheDeletes++ })
			p.ApplyFunc(runAfterDestroyTaskSuccessHook, func(context.Context, string) error { hooks++; return nil })

			job := &Task{
				BaseInfo:       BaseInfo{InstanceType: cubebox.InstanceType_cubebox.String()},
				Ctx:            t.Context(),
				Request:        &cubebox.DestroyCubeSandboxRequest{SandboxID: "sb"},
				CallEp:         "127.0.0.1:1",
				TaskType:       DestroySandbox,
				CreateFailover: tc.createFailover,
			}
			err := handleTask(job)
			if tc.createFailover {
				require.ErrorContains(t, err, "missing from cache")
				require.Len(t, queued, 1)
				require.Same(t, job, queued[0])
				require.Zero(t, destroys)
				require.Zero(t, proxyDeletes)
				require.Zero(t, cacheDeletes)
				require.Zero(t, hooks)
				if tc.cacheRestored {
					cachedNode = &node.Node{}
					require.NoError(t, handleTask(queued[0]))
				} else {
					require.ErrorContains(t, handleTask(queued[0]), "missing from cache")
				}
				require.Len(t, queued, 1)
			} else {
				require.NoError(t, err)
				require.Empty(t, queued)
			}
			require.Equal(t, tc.wantRPCs, destroys)
			require.Equal(t, tc.wantCleanups, proxyDeletes)
			require.Equal(t, tc.wantCleanups, cacheDeletes)
			require.Equal(t, tc.wantCleanups, hooks)
		})
	}
}
