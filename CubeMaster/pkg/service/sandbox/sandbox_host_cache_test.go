// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const (
	movedSandboxID = "sb-moved"
	removedHostIP  = "10.0.0.2" // a node Master no longer knows
	liveHostIP     = "10.0.0.1"
	otherLiveIP    = "10.0.0.3"
)

func cachedHost(id string) string {
	if c := localcache.GetSandboxCache(id); c != nil {
		return c.HostIP
	}
	return ""
}

func withCachedHost(t *testing.T, id, hostIP string) {
	t.Helper()
	localcache.DeleteSandboxCache(id)
	if hostIP != "" {
		localcache.SetSandboxCache(id, &localcache.SandboxCache{SandboxID: id, HostIP: hostIP})
	}
	t.Cleanup(func() { localcache.DeleteSandboxCache(id) })
}

// hostLookup describes what Master knows for one test: the shared route, the
// pause binding, and which node IPs are known (and whether healthy).
type hostLookup struct {
	proxyHost string
	binding   *pausesnap.Record
	nodes     map[string]bool // IP -> healthy
	lookups   []string
	onLookup  func(ip string)
}

func (h *hostLookup) deps() sandboxInfoLookupDeps {
	return sandboxInfoLookupDeps{
		getPauseBinding: func(context.Context, string) (*pausesnap.Record, error) {
			if h.binding == nil {
				return nil, pausesnap.ErrNotFound
			}
			return h.binding, nil
		},
		getSandboxProxyMap: func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
			if h.proxyHost == "" {
				return nil, false
			}
			return &basetypes.SandboxProxyMap{HostIP: h.proxyHost}, true
		},
		getNode: func(string) (*node.Node, bool) { return nil, false },
		getNodesByIP: func(ip string) (*node.Node, bool) {
			h.lookups = append(h.lookups, ip)
			if h.onLookup != nil {
				h.onLookup(ip)
			}
			healthy, ok := h.nodes[ip]
			if !ok {
				return nil, false
			}
			return &node.Node{IP: ip, InsID: "node-" + ip, Healthy: healthy}, true
		},
		getCubeletAddr: func(hostIP string) string { return hostIP + ":50051" },
	}
}

func bindingOn(ip, status string) *pausesnap.Record {
	return &pausesnap.Record{SandboxID: movedSandboxID, SnapshotID: "snap-1", Status: status, NodeID: "node-" + ip, NodeIP: ip}
}

func locate(t *testing.T, h *hostLookup) (endpoint, locatedHost string, rsp *types.GetCubeSandboxRes, ok bool) {
	t.Helper()
	rsp = &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	endpoint, locatedHost, _, ok = checkValidAndGetReqWithDeps(context.Background(),
		&types.GetCubeSandboxReq{SandboxID: movedSandboxID}, &cubebox.ListCubeSandboxRequest{}, rsp, h.deps())
	return endpoint, locatedHost, rsp, ok
}

// The reported 404: the cache names the node the sandbox left, and that node
// was removed afterwards. Info must fall back to the shared route.
func TestLocateDropsCachedHostOfRemovedNode(t *testing.T) {
	withCachedHost(t, movedSandboxID, removedHostIP)
	h := &hostLookup{proxyHost: liveHostIP, nodes: map[string]bool{liveHostIP: true}}

	endpoint, locatedHost, rsp, ok := locate(t, h)
	require.True(t, ok, "ret=%+v", rsp.Ret)
	require.Equal(t, liveHostIP+":50051", endpoint)
	require.Equal(t, liveHostIP, locatedHost)
	require.Empty(t, cachedHost(movedSandboxID), "stale location is dropped")
}

// Only a node that is gone proves the cache wrong: a known node keeps
// winning, and its node is looked up once and reused.
func TestLocateKeepsCachedHostOfKnownNode(t *testing.T) {
	withCachedHost(t, movedSandboxID, removedHostIP)
	h := &hostLookup{proxyHost: liveHostIP, nodes: map[string]bool{removedHostIP: true, liveHostIP: true}}

	_, locatedHost, _, ok := locate(t, h)
	require.True(t, ok)
	require.Equal(t, removedHostIP, locatedHost)
	require.Equal(t, []string{removedHostIP}, h.lookups)
	require.Equal(t, removedHostIP, cachedHost(movedSandboxID))
}

// A known but unhealthy node is not a removed one: the cache is kept and
// Info reports the node unhealthy, as before.
func TestLocateKeepsCachedHostOfUnhealthyNode(t *testing.T) {
	withCachedHost(t, movedSandboxID, removedHostIP)
	h := &hostLookup{proxyHost: liveHostIP, nodes: map[string]bool{removedHostIP: false, liveHostIP: true}}

	_, _, rsp, ok := locate(t, h)
	require.False(t, ok)
	require.Equal(t, int(errorcode.ErrorCode_CubeletUnHealthy), rsp.Ret.RetCode)
	require.Equal(t, removedHostIP, cachedHost(movedSandboxID))
}

// The shared route can be stale too: when it also names the removed node,
// Info falls back to the pause binding's node.
func TestLocateFallsBackToBindingWhenProxyNodeIsRemoved(t *testing.T) {
	withCachedHost(t, movedSandboxID, removedHostIP)
	h := &hostLookup{
		proxyHost: removedHostIP,
		binding:   bindingOn(otherLiveIP, pausesnap.StatusCreating),
		nodes:     map[string]bool{otherLiveIP: true},
	}

	endpoint, locatedHost, rsp, ok := locate(t, h)
	require.True(t, ok, "ret=%+v", rsp.Ret)
	require.Equal(t, otherLiveIP+":50051", endpoint)
	require.Equal(t, otherLiveIP, locatedHost)
}

func TestLocateFallsBackToBindingWithoutCacheOrProxy(t *testing.T) {
	withCachedHost(t, movedSandboxID, "")
	h := &hostLookup{binding: bindingOn(otherLiveIP, pausesnap.StatusCreating), nodes: map[string]bool{otherLiveIP: true}}

	_, locatedHost, _, ok := locate(t, h)
	require.True(t, ok)
	require.Equal(t, otherLiveIP, locatedHost)
}

func TestLocateNotFoundWithoutAnyCandidate(t *testing.T) {
	withCachedHost(t, movedSandboxID, "")
	h := &hostLookup{nodes: map[string]bool{liveHostIP: true}}

	_, _, rsp, ok := locate(t, h)
	require.False(t, ok)
	require.Equal(t, int(errorcode.ErrorCode_NotFound), rsp.Ret.RetCode)
	require.Empty(t, h.lookups)
}

// When every candidate names a removed node, a shimless pause bound to the
// last of them is still answered from Master, and anything else is not found.
func TestLocateWithOnlyRemovedNodes(t *testing.T) {
	t.Run("shimless pause bound there", func(t *testing.T) {
		withCachedHost(t, movedSandboxID, removedHostIP)
		h := &hostLookup{proxyHost: removedHostIP, binding: bindingOn(removedHostIP, pausesnap.StatusReady)}

		endpoint, locatedHost, _, ok := locate(t, h)
		require.True(t, ok)
		require.Empty(t, endpoint)
		require.Equal(t, removedHostIP, locatedHost)
	})
	t.Run("no binding", func(t *testing.T) {
		withCachedHost(t, movedSandboxID, removedHostIP)
		h := &hostLookup{proxyHost: removedHostIP}

		_, _, rsp, ok := locate(t, h)
		require.False(t, ok)
		require.Equal(t, int(errorcode.ErrorCode_NotFound), rsp.Ret.RetCode)
	})
}

// A list or a resume can write a new location between reading the stale
// entry and dropping it. The new entry must survive.
func TestLocateDoesNotDropConcurrentlyWrittenCacheEntry(t *testing.T) {
	withCachedHost(t, movedSandboxID, removedHostIP)
	h := &hostLookup{proxyHost: liveHostIP, nodes: map[string]bool{liveHostIP: true}}
	h.onLookup = func(ip string) {
		if ip == removedHostIP {
			localcache.SetSandboxCache(movedSandboxID, &localcache.SandboxCache{SandboxID: movedSandboxID, HostIP: liveHostIP})
		}
	}

	_, _, _, ok := locate(t, h)
	require.True(t, ok)
	require.Equal(t, liveHostIP, cachedHost(movedSandboxID))
}
