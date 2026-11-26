// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
)

const routeTestSandboxID = "aabbccddeeff00112233445566778899"

// countProxyReads stubs the proxy map getter and reports how often it was used.
func countProxyReads(t *testing.T, pm *basetypes.SandboxProxyMap) *int {
	t.Helper()
	reads := 0
	p := gomonkey.NewPatches()
	t.Cleanup(p.Reset)
	p.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		reads++
		if pm == nil {
			return nil, false
		}
		return pm, true
	})
	return &reads
}

func seedCache(t *testing.T, sandboxID, hostIP string) {
	t.Helper()
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: hostIP})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })
}

// TestSandboxHostIPCacheHitSkipsRedis pins the whole point of the change: a
// warm entry must not cost a Redis round trip.
func TestSandboxHostIPCacheHitSkipsRedis(t *testing.T) {
	seedCache(t, routeTestSandboxID, "10.0.0.1")
	reads := countProxyReads(t, &basetypes.SandboxProxyMap{HostIP: "10.0.0.9", SandboxID: routeTestSandboxID})

	ip, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.1", ip, "cache wins over the proxy map")
	assert.Zero(t, *reads, "a cache hit must not read Redis")
}

// TestSandboxHostIPMissFallsBackAndBackfills: one miss pays one Redis read and
// every later call stays local.
func TestSandboxHostIPMissFallsBackAndBackfills(t *testing.T) {
	localcache.DeleteSandboxCache(routeTestSandboxID)
	reads := countProxyReads(t, &basetypes.SandboxProxyMap{HostIP: "10.0.0.2", SandboxID: routeTestSandboxID})

	ip, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.2", ip)
	assert.Equal(t, 1, *reads)

	cached := localcache.GetSandboxCache(routeTestSandboxID)
	require.NotNil(t, cached, "a proxy hit must be written back")
	assert.Equal(t, "10.0.0.2", cached.HostIP)

	_, ok = SandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, 1, *reads, "the backfilled entry serves the next call")
}

func TestSandboxHostIPReportsMissingSandbox(t *testing.T) {
	localcache.DeleteSandboxCache(routeTestSandboxID)
	countProxyReads(t, nil)

	_, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	assert.False(t, ok)
}

// TestSandboxHostIPSkipsProxyEntryWithEmptyHostIP: a proxy row without a route
// is not a host, but its other fields may still be meaningful to callers.
func TestSandboxHostIPSkipsProxyEntryWithEmptyHostIP(t *testing.T) {
	localcache.DeleteSandboxCache(routeTestSandboxID)
	countProxyReads(t, &basetypes.SandboxProxyMap{HostIP: "", SandboxID: routeTestSandboxID})

	_, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	assert.False(t, ok)
	assert.Nil(t, localcache.GetSandboxCache(routeTestSandboxID), "an empty host must not be cached")
}

// TestResolveSandboxHostFallsBackToPauseBinding keeps the legacy fallback chain.
func TestResolveSandboxHostFallsBackToPauseBinding(t *testing.T) {
	localcache.DeleteSandboxCache(routeTestSandboxID)
	countProxyReads(t, nil)
	p := gomonkey.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return &pausesnap.Record{SandboxID: routeTestSandboxID, NodeIP: "10.0.0.7"}, nil
	})
	t.Cleanup(p.Reset)

	ip, ok := resolveSandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.7", ip)
}

func TestResolvePauseHostIPPrefersPauseBinding(t *testing.T) {
	seedCache(t, routeTestSandboxID, "10.0.0.1")
	p := gomonkey.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return &pausesnap.Record{SandboxID: routeTestSandboxID, NodeIP: "10.0.0.7"}, nil
	})
	t.Cleanup(p.Reset)

	ip, ok := resolvePauseHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.7", ip)
}

// TestCacheCollectedEntriesPrefersProxyHost covers the sticky ghost from #2017:
// two nodes report one ID and only the proxy map knows which is live.
func TestCacheCollectedEntriesPrefersProxyHost(t *testing.T) {
	const id = "11111111111111111111111111111111"
	t.Cleanup(func() { localcache.DeleteSandboxCache(id) })
	countProxyReads(t, &basetypes.SandboxProxyMap{SandboxID: id, HostIP: "10.0.0.1"})

	// The ghost is reported last, which used to win by goroutine completion order.
	cacheCollectedSandboxEntries(context.Background(), []clusterSandboxEntry{
		{SandboxID: id, HostIP: "10.0.0.1"},
		{SandboxID: id, HostIP: "10.0.0.2"},
	})

	cached := localcache.GetSandboxCache(id)
	require.NotNil(t, cached)
	assert.Equal(t, "10.0.0.1", cached.HostIP, "the routed host must beat the ghost")
}

// TestCacheCollectedEntriesKeepsSingleReportWithoutRedisRead: a unique ID cannot
// be a ghost, so the scan must not pay a Redis read per sandbox.
func TestCacheCollectedEntriesKeepsSingleReportWithoutRedisRead(t *testing.T) {
	const id = "22222222222222222222222222222222"
	t.Cleanup(func() { localcache.DeleteSandboxCache(id) })
	reads := countProxyReads(t, &basetypes.SandboxProxyMap{SandboxID: id, HostIP: "10.0.0.1"})

	cacheCollectedSandboxEntries(context.Background(), []clusterSandboxEntry{
		{SandboxID: id, HostIP: "10.0.0.2"},
	})

	assert.Zero(t, *reads)
	cached := localcache.GetSandboxCache(id)
	require.NotNil(t, cached)
	assert.Equal(t, "10.0.0.2", cached.HostIP)
}

// TestCacheCollectedEntriesIgnoresUnroutedProxyHost: a proxy host nobody
// reports is stale too, so it must not replace a host a node actually claims.
func TestCacheCollectedEntriesIgnoresUnroutedProxyHost(t *testing.T) {
	const id = "33333333333333333333333333333333"
	t.Cleanup(func() { localcache.DeleteSandboxCache(id) })
	countProxyReads(t, &basetypes.SandboxProxyMap{SandboxID: id, HostIP: "10.0.0.9"})

	cacheCollectedSandboxEntries(context.Background(), []clusterSandboxEntry{
		{SandboxID: id, HostIP: "10.0.0.1"},
		{SandboxID: id, HostIP: "10.0.0.2"},
	})

	cached := localcache.GetSandboxCache(id)
	require.NotNil(t, cached)
	assert.NotEqual(t, "10.0.0.9", cached.HostIP)
}

// TestCachedSandboxHostIPWithUsesCache covers the Info path delegate.
func TestCachedSandboxHostIPWithUsesCache(t *testing.T) {
	seedCache(t, routeTestSandboxID, "10.0.0.1")
	reads := countProxyReads(t, &basetypes.SandboxProxyMap{HostIP: "10.0.0.9", SandboxID: routeTestSandboxID})

	got := cachedSandboxHostIPWith(context.Background(), routeTestSandboxID, localcache.GetSandboxProxyMap)
	assert.Equal(t, "10.0.0.1", got)
	assert.Zero(t, *reads)
}

// TestDealScfSandboxUsesCache covers the Delete path delegate.
func TestDealScfSandboxUsesCache(t *testing.T) {
	localcache.SetSandboxCache(routeTestSandboxID, &localcache.SandboxCache{
		SandboxID: routeTestSandboxID, HostID: "node-a", HostIP: "10.0.0.1",
	})
	t.Cleanup(func() { localcache.DeleteSandboxCache(routeTestSandboxID) })
	reads := countProxyReads(t, &basetypes.SandboxProxyMap{HostIP: "10.0.0.9", SandboxID: routeTestSandboxID})

	tk := &task.Task{}
	ok := dealScfSandbox(context.Background(), &types.DeleteCubeSandboxReq{RequestID: "r", SandboxID: routeTestSandboxID}, tk)
	require.True(t, ok)
	assert.Equal(t, cubelet.GetCubeletAddr("10.0.0.1"), tk.CallEp)
	assert.Equal(t, "node-a", tk.NodeID, "the destroy task must carry the node ID")
	assert.Zero(t, *reads)
}

func withShortSandboxCacheTTL(t *testing.T) {
	t.Helper()
	cfg := ensureSandboxTestConfig(t)
	orig := cfg.Common.SandboxCacheTTL
	t.Cleanup(func() { cfg.Common.SandboxCacheTTL = orig })
	cfg.Common.SandboxCacheTTL = 20 * time.Millisecond
}

// TestSandboxHostIPExpiredEntryRechecksProxy: once the TTL passes, routing must
// consult the shared route so a lost invalidation cannot pin a stale host.
func TestSandboxHostIPExpiredEntryRechecksProxy(t *testing.T) {
	withShortSandboxCacheTTL(t)
	seedCache(t, routeTestSandboxID, "10.0.0.9")
	time.Sleep(40 * time.Millisecond)
	reads := countProxyReads(t, &basetypes.SandboxProxyMap{HostIP: "10.0.0.1", SandboxID: routeTestSandboxID})

	ip, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.1", ip, "the shared route wins over an expired entry")
	assert.Equal(t, 1, *reads)
}

// TestSandboxHostIPExpiredEntryWithoutProxyStaysRoutable keeps a host that never
// came from sandbox:proxy (scan, List, a future non-cubebox type) routable.
func TestSandboxHostIPExpiredEntryWithoutProxyStaysRoutable(t *testing.T) {
	withShortSandboxCacheTTL(t)
	seedCache(t, routeTestSandboxID, "10.0.0.9")
	time.Sleep(40 * time.Millisecond)
	reads := countProxyReads(t, nil)
	checks := stubProxyRouteGone(t, true)

	ip, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.9", ip)
	assert.Zero(t, *checks, "a host without proxy backing has nothing to confirm")
	_, _ = SandboxHostIP(context.Background(), routeTestSandboxID)
	assert.Equal(t, 1, *reads, "a re-stamped entry must serve the next call locally")
}

// stubProxyRouteGone stubs the teardown check and reports how often it ran.
func stubProxyRouteGone(t *testing.T, gone bool) *int {
	t.Helper()
	calls := 0
	orig := proxyRouteGoneFn
	t.Cleanup(func() { proxyRouteGoneFn = orig })
	proxyRouteGoneFn = func(context.Context, string) bool {
		calls++
		return gone
	}
	return &calls
}

func seedProxyBackedCache(t *testing.T, sandboxID, hostIP string) {
	t.Helper()
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: hostIP, FromProxy: true})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })
}

// TestSandboxHostIPDropsProxyBackedHostAfterTeardown: a replica that missed the
// delete invalidation must stop routing within one TTL, not the 2h wipe.
func TestSandboxHostIPDropsProxyBackedHostAfterTeardown(t *testing.T) {
	withShortSandboxCacheTTL(t)
	seedProxyBackedCache(t, routeTestSandboxID, "10.0.0.9")
	time.Sleep(40 * time.Millisecond)
	countProxyReads(t, nil)
	checks := stubProxyRouteGone(t, true)

	_, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	assert.False(t, ok, "a torn-down sandbox must not route to its old host")
	assert.Equal(t, 1, *checks)
	assert.Nil(t, localcache.GetSandboxCache(routeTestSandboxID))
}

// TestSandboxHostIPKeepsProxyBackedHostWhenUnsure: an unreachable Redis is not
// teardown, so the last host survives the outage and keeps its backing.
func TestSandboxHostIPKeepsProxyBackedHostWhenUnsure(t *testing.T) {
	withShortSandboxCacheTTL(t)
	seedProxyBackedCache(t, routeTestSandboxID, "10.0.0.9")
	time.Sleep(40 * time.Millisecond)
	countProxyReads(t, nil)
	stubProxyRouteGone(t, false)

	ip, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.9", ip)
	require.NotNil(t, localcache.GetFreshSandboxCache(routeTestSandboxID))
	assert.True(t, localcache.GetSandboxCache(routeTestSandboxID).FromProxy, "re-stamping must keep the backing")
}

// TestSandboxHostIPMarksProxyBackfill: only a host read from sandbox:proxy is
// eligible for the teardown check.
func TestSandboxHostIPMarksProxyBackfill(t *testing.T) {
	countProxyReads(t, &basetypes.SandboxProxyMap{HostIP: "10.0.0.1", SandboxID: routeTestSandboxID})
	t.Cleanup(func() { localcache.DeleteSandboxCache(routeTestSandboxID) })

	_, ok := SandboxHostIP(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.True(t, localcache.GetSandboxCache(routeTestSandboxID).FromProxy)
}

// TestSandboxHostBackfillFollowsNodeIPChange: the backfilled entry must keep the
// proxy's HostID; the stale route IP no longer maps back to any node.
func TestSandboxHostBackfillFollowsNodeIPChange(t *testing.T) {
	t.Cleanup(localcache.ResetNodeCacheForTest())
	localcache.UpsertNode(&node.Node{InsID: "node-a", IP: "10.0.0.2", Healthy: true})
	reads := countProxyReads(t, &basetypes.SandboxProxyMap{HostID: "node-a", HostIP: "10.0.0.1", SandboxID: routeTestSandboxID})

	_, _, ok := SandboxHost(context.Background(), routeTestSandboxID)
	require.True(t, ok)

	id, ip, ok := SandboxHost(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "node-a", id)
	assert.Equal(t, "10.0.0.2", ip, "a cached NodeID must follow the node's new IP")
	assert.Equal(t, 1, *reads, "the second call must be served from the cache")
}

// TestSandboxHostTeardownCheckKeepsHostID: re-stamping on an unsure check must
// keep the node ID even while that node is briefly unknown to this replica.
func TestSandboxHostTeardownCheckKeepsHostID(t *testing.T) {
	t.Cleanup(localcache.ResetNodeCacheForTest())
	withShortSandboxCacheTTL(t)
	localcache.SetSandboxCache(routeTestSandboxID, &localcache.SandboxCache{
		SandboxID: routeTestSandboxID, HostID: "node-a", HostIP: "10.0.0.1", FromProxy: true,
	})
	time.Sleep(40 * time.Millisecond)
	countProxyReads(t, nil)
	stubProxyRouteGone(t, false)

	id, _, ok := SandboxHost(context.Background(), routeTestSandboxID)
	require.True(t, ok)
	assert.Equal(t, "node-a", id)
	require.Equal(t, "node-a", localcache.GetSandboxCache(routeTestSandboxID).HostID)

	localcache.UpsertNode(&node.Node{InsID: "node-a", IP: "10.0.0.2", Healthy: true})
	assert.Equal(t, "10.0.0.2", localcache.GetSandboxCache(routeTestSandboxID).HostIP)
}

// TestResolveSandboxIDExpiredEntrySkipsClusterScan guards the hot path: an
// expired entry must still resolve a full ID without fanning out to every node.
func TestResolveSandboxIDExpiredEntrySkipsClusterScan(t *testing.T) {
	withShortSandboxCacheTTL(t)
	seedCache(t, routeTestSandboxID, "10.0.0.1")
	time.Sleep(40 * time.Millisecond)
	p := gomonkey.NewPatches()
	t.Cleanup(p.Reset)
	p.ApplyFunc(collectClusterSandboxIDs, func(context.Context) []clusterSandboxEntry {
		t.Error("an expired entry must not trigger a cluster scan")
		return nil
	})
	p.ApplyFunc(collectPausedSandboxIDs, func(context.Context) []clusterSandboxEntry {
		t.Error("an expired entry must not trigger a pause binding scan")
		return nil
	})

	got, err := ResolveSandboxID(context.Background(), routeTestSandboxID)
	require.NoError(t, err)
	assert.Equal(t, routeTestSandboxID, got)
}

// TestDealRspDataDoesNotClobberResolvedHost guards the List path: a List answer
// is a weaker source than a host already resolved from create/resume/proxy.
func TestDealRspDataDoesNotClobberResolvedHost(t *testing.T) {
	seedCache(t, routeTestSandboxID, "10.0.0.1")
	resChan := make(chan *types.SandboxBriefData, 1)
	done := make(chan struct{})
	rsp := &types.ListCubeSandboxRes{}
	resChan <- &types.SandboxBriefData{SandboxID: routeTestSandboxID, HostID: "node-x", HostIP: "10.0.0.9"}
	close(resChan)

	dealRspData(context.Background(), done, resChan, rsp)
	<-done

	assert.Equal(t, "10.0.0.1", localcache.GetSandboxCache(routeTestSandboxID).HostIP,
		"a List response must not overwrite a freshly resolved host")
}

// TestDealRspDataRefreshesExpiredHost: an expired entry is no longer
// authoritative, so a List answer must be allowed to correct it.
func TestDealRspDataRefreshesExpiredHost(t *testing.T) {
	withShortSandboxCacheTTL(t)
	seedCache(t, routeTestSandboxID, "10.0.0.1")
	time.Sleep(40 * time.Millisecond)
	resChan := make(chan *types.SandboxBriefData, 1)
	done := make(chan struct{})
	resChan <- &types.SandboxBriefData{SandboxID: routeTestSandboxID, HostIP: "10.0.0.9"}
	close(resChan)

	dealRspData(context.Background(), done, resChan, &types.ListCubeSandboxRes{})
	<-done

	assert.Equal(t, "10.0.0.9", localcache.GetSandboxCache(routeTestSandboxID).HostIP,
		"a List response must refresh an expired host")
}
