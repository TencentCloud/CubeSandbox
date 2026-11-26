// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// listCache is the per-process routing hint read on the hot path. Entries are
// dropped by cache invalidation, by sandbox teardown, or by the periodic wipe.
var (
	listCache = &sync.Map{}
)

type SandboxCache struct {
	SandboxID string
	HostID    string
	HostIP    string
	// FromProxy marks a host taken from sandbox:proxy, so a later confirmed
	// absence of that key means teardown rather than "never had one".
	FromProxy bool

	// expireAt bounds routing freshness only; an expired entry still proves the
	// sandbox is known. Zero means no TTL was applied.
	expireAt time.Time
}

func cleanSandboxCache() {
	listCache = &sync.Map{}
}

// sandboxCacheTTL returns the per-entry TTL, or 0 when disabled or unconfigured.
func sandboxCacheTTL() time.Duration {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Common == nil {
		return 0
	}
	return cfg.Common.SandboxCacheTTL
}

// GetSandboxCache returns a copy of the entry regardless of age, with HostIP
// resolved by HostID, for existence checks and as a last-resort route.
func GetSandboxCache(sandboxID string) *SandboxCache {
	val, ok := listCache.Load(sandboxID)
	if !ok {
		return nil
	}
	stored, _ := val.(*SandboxCache)
	if stored == nil {
		return nil
	}
	cp := *stored
	cp.HostIP = ResolveNodeIP(cp.HostID, cp.HostIP)
	return &cp
}

// GetFreshSandboxCache returns the entry only within its TTL, so routing
// re-checks the shared proxy map once an invalidation might have been lost.
func GetFreshSandboxCache(sandboxID string) *SandboxCache {
	cache := GetSandboxCache(sandboxID)
	if cache == nil || (!cache.expireAt.IsZero() && time.Now().After(cache.expireAt)) {
		return nil
	}
	return cache
}

func DeleteSandboxCache(sandboxID string) {
	listCache.Delete(sandboxID)
}

func SetSandboxCache(sandboxID string, cache *SandboxCache) {
	if cache == nil {
		return
	}
	cp := *cache
	if cp.HostID == "" && cp.HostIP != "" {
		if n, ok := GetNodesByIp(cp.HostIP); ok {
			cp.HostID = n.ID()
		}
	}
	cp.expireAt = time.Time{}
	if ttl := sandboxCacheTTL(); ttl > 0 {
		cp.expireAt = time.Now().Add(ttl)
	}
	listCache.Store(sandboxID, &cp)
}

// SetSandboxCacheIfAbsent keeps a weaker source (a List response) from
// overwriting a host this replica resolved recently.
func SetSandboxCacheIfAbsent(sandboxID string, cache *SandboxCache) {
	if cache == nil || GetFreshSandboxCache(sandboxID) != nil {
		return
	}
	cp := *cache
	// Re-stamping the same host must not drop the proxy backing it had.
	if prev := GetSandboxCache(sandboxID); prev != nil && prev.HostIP == cp.HostIP {
		cp.FromProxy = cp.FromProxy || prev.FromProxy
	}
	SetSandboxCache(sandboxID, &cp)
}

func ListKnownSandboxIDs() []string {
	ids := make([]string, 0)
	listCache.Range(func(key, _ any) bool {
		if sandboxID, ok := key.(string); ok && sandboxID != "" {
			ids = append(ids, sandboxID)
		}
		return true
	})
	return ids
}

func (l *local) cleanSandboxCache(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	checkDeadline := time.Now().Add(config.GetConfig().Common.CleanSandboxCacheInterval)
	for {
		select {
		case <-ticker.C:
			recov.WithRecover(func() {
				if checkDeadline.After(time.Now()) {

					return
				}
				defer func() {
					checkDeadline = time.Now().Add(config.GetConfig().Common.CleanSandboxCacheInterval)
				}()
				cleanSandboxCache()
				CubeLog.WithContext(context.Background()).Errorf("clean_sandbox_cache")
			}, func(panicError interface{}) {
				checkDeadline = time.Now().Add(config.GetConfig().Common.CleanSandboxCacheInterval)
				CubeLog.WithContext(context.Background()).Fatalf("loop panic:%v", panicError)
			})
		case <-ctx.Done():
			return
		}
	}
}
