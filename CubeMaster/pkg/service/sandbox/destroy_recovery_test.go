// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeerror "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"testing"
)

func TestDestroyDoesNotTrustCreateTimeSpecRouteAndSurfacesDatabaseErrors(t *testing.T) {
	p := gomonkey.NewPatches()
	defer p.Reset()
	p.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache { return nil })
	p.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) { return nil, false })
	p.ApplyFunc(resolvePauseHostIP, func(context.Context, string) (string, bool) { return "", false })
	for _, tc := range []struct {
		host  string
		err   error
		found bool
	}{{"10.0.0.1", nil, false}, {"", sandboxspec.ErrSandboxSpecNotFound, false}, {"", errors.New("database unavailable"), false}} {
		q := gomonkey.ApplyFunc(sandboxspec.GetHostIP, func(context.Context, string) (string, error) { return tc.host, tc.err })
		job := &task.Task{}
		found, err := dealScfSandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "gone"}, job)
		q.Reset()
		require.Equal(t, tc.found, found)
		if tc.host != "" {
			require.ErrorContains(t, err, "sandbox route is unavailable")
			require.Empty(t, job.CallEp)
		} else if tc.err != nil && !errors.Is(tc.err, sandboxspec.ErrSandboxSpecNotFound) {
			require.ErrorIs(t, err, tc.err)
		} else {
			require.NoError(t, err)
		}
		if found {
			require.Equal(t, cubelet.GetCubeletAddr(tc.host), job.CallEp)
		}
	}
}

func TestDestroyRequiresWorkerConfirmationAndKeepsRouteOnHookFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available bool
		hookError bool
	}{{"missing_worker", false, false}, {"cleanup_failure", true, true}, {"success", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := gomonkey.NewPatches()
			defer p.Reset()
			ResetAfterDestroySandboxSuccessHooks()
			defer ResetAfterDestroySandboxSuccessHooks()
			p.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
				return fn(ctx)
			})
			p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return nil, tc.available })
			p.ApplyFunc(pausesnap.TryDeletePaused, func(context.Context, string, string, string) (bool, error) { return false, nil })
			destroyed, hook, routeDeleted := 0, 0, 0
			p.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
				destroyed++
				return &cubebox.DestroyCubeSandboxResponse{Ret: &cubeerror.Ret{RetCode: cubeerror.ErrorCode_Success}}, nil
			})
			RegisterAfterDestroySandboxSuccessHook(func(context.Context, string) error {
				hook++
				if tc.hookError {
					return errors.New("metadata unavailable")
				}
				return nil
			})
			p.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error { routeDeleted++; require.Equal(t, 1, hook); return nil })
			p.ApplyFunc(localcache.DeleteSandboxCache, func(string) {})
			err := callCubelet(context.Background(), "10.0.0.1:9090", &cubebox.DestroyCubeSandboxRequest{SandboxID: "known"})
			if !tc.available {
				require.Error(t, err)
				require.Zero(t, destroyed)
				require.Zero(t, hook)
			} else if tc.hookError {
				require.ErrorContains(t, err, "metadata unavailable")
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, routeDeleted)
			}
			if !tc.available || tc.hookError {
				require.Zero(t, routeDeleted)
			}
		})
	}
}
