// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"

	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

// sandboxHost is the node a sandbox runs on; ID survives a node IP change.
type sandboxHost struct {
	ID string
	IP string
}

// sandboxRouteWith resolves a sandbox's node: fresh cache, then proxy map (written
// back), then the last cached host unless its proxy route was torn down.
func sandboxRouteWith(ctx context.Context, sandboxID string,
	getProxy func(context.Context, string) (*basetypes.SandboxProxyMap, bool)) (sandboxHost, *basetypes.SandboxProxyMap, bool) {
	if v := localcache.GetFreshSandboxCache(sandboxID); v != nil && v.HostIP != "" {
		return sandboxHost{ID: v.HostID, IP: v.HostIP}, nil, true
	}
	if pm, ok := getProxy(ctx, sandboxID); ok && pm != nil && pm.HostIP != "" {
		localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
			SandboxID: sandboxID, HostID: pm.HostID, HostIP: pm.HostIP, FromProxy: true,
		})
		return sandboxHost{ID: pm.HostID, IP: pm.HostIP}, pm, true
	}
	v := localcache.GetSandboxCache(sandboxID)
	if v == nil || v.HostIP == "" {
		return sandboxHost{}, nil, false
	}
	// A proxy-backed host whose key is confirmed gone was torn down on another
	// replica whose invalidation we missed; anything else keeps the last host.
	if v.FromProxy && proxyRouteGoneFn(ctx, sandboxID) {
		localcache.DeleteSandboxCache(sandboxID)
		return sandboxHost{}, nil, false
	}
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID, HostID: v.HostID, HostIP: v.HostIP, FromProxy: v.FromProxy,
	})
	return sandboxHost{ID: v.HostID, IP: v.HostIP}, nil, true
}

// SandboxHost is the shared entry point for callers that also need the node ID.
func SandboxHost(ctx context.Context, sandboxID string) (string, string, bool) {
	host, _, ok := sandboxRouteWith(ctx, sandboxID, getSandboxProxyMapFn)
	return host.ID, host.IP, ok
}

// SandboxHostIP is the shared entry point for callers that only need the host.
func SandboxHostIP(ctx context.Context, sandboxID string) (string, bool) {
	_, ip, ok := SandboxHost(ctx, sandboxID)
	return ip, ok
}

// sandboxRoute also exposes the proxy entry when it supplied the host.
func sandboxRoute(ctx context.Context, sandboxID string) (sandboxHost, *basetypes.SandboxProxyMap, bool) {
	return sandboxRouteWith(ctx, sandboxID, getSandboxProxyMapFn)
}
