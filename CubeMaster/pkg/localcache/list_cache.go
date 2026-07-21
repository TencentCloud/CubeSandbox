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
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	listCache = &sync.Map{}
)

type SandboxCache struct {
	SandboxID string
	HostIP    string
}

func cleanSandboxCache() {
	listCache = &sync.Map{}
}

func GetSandboxCache(sandboxID string) *SandboxCache {
	val, ok := listCache.Load(sandboxID)
	if !ok {
		return nil
	}
	return val.(*SandboxCache)
}

func DeleteSandboxCache(sandboxID string) {
	listCache.Delete(sandboxID)
}

func SetSandboxCache(sandboxID string, cache *SandboxCache) {
	listCache.Store(sandboxID, cache)
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
