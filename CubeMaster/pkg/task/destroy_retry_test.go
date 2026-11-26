// SPDX-License-Identifier: Apache-2.0

package task

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestDestroyFailureRequeuesBeforeRouteCleanup(t *testing.T) {
	for _, failure := range []string{"worker unavailable", "metadata cleanup"} {
		t.Run(failure, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
				return fn(ctx)
			})
			patches.ApplyFunc(reportMetric, func(*Task, error) {})
			patches.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return nil, false })
			recovered := false
			patches.ApplyFunc(pausesnap.TryDeletePaused, func(context.Context, string, string, string) (bool, error) {
				return recovered || failure == "metadata cleanup", nil
			})
			patches.ApplyFunc(runAfterDestroyTaskSuccessHook, func(context.Context, string) error {
				if !recovered {
					return errors.New("database unavailable")
				}
				return nil
			})
			deleted := 0
			patches.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error { deleted++; return nil })
			patches.ApplyFunc(localcache.DeleteSandboxCache, func(string) {})
			queued := 0
			patches.ApplyFunc(AddAsyncTask, func(*Task) error { queued++; return nil })
			errorcode.InitCubeCodeRetryMap(&config.Config{CubeletConf: &config.CubeletConf{}})
			h := &DestroySandboxTaskHandler{}
			h.SetLoopMaxRetry(1)
			old := l
			l = &localTask{handles: map[OpType]taskHandler{DestroySandbox: h}}
			defer func() { l = old }()
			job := &Task{BaseInfo: BaseInfo{InstanceType: cubebox.InstanceType_cubebox.String()}, Ctx: context.Background(), TaskType: DestroySandbox, CallEp: "127.0.0.1:1", Request: &cubebox.DestroyCubeSandboxRequest{SandboxID: "sb"}}
			require.Error(t, handleTask(job))
			require.Equal(t, 1, queued)
			require.True(t, job.loopRetry)
			require.Zero(t, deleted)
			recovered = true
			require.NoError(t, handleTask(job))
			require.Equal(t, 1, deleted)
			require.Equal(t, 1, queued)
		})
	}
}
