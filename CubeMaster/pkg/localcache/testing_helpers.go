// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

// ResetNodeCacheForTest swaps in an isolated node cache and returns a restore
// function suitable for t.Cleanup. Intended for tests only.
func ResetNodeCacheForTest() func() {
	prev := l
	prevListCache := listCache
	l = &local{
		cache:                 cache.New(5*time.Minute, 10*time.Minute),
		locality:              newTemplateLocality(),
		sortedNodesByClusters: map[string]node.NodeList{},
	}
	cleanSandboxCache()
	return func() {
		l = prev
		listCache = prevListCache
	}
}

// SetNodeForTest stores a node directly in the node cache without requiring
// global config initialization. Intended for tests paired with
// ResetNodeCacheForTest.
func SetNodeForTest(n *node.Node) {
	if n == nil || l == nil {
		return
	}
	l.cache.Set(n.ID(), n, cache.NoExpiration)
}
