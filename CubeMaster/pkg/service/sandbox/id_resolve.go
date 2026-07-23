// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxid"
)

// Bound fan-out when listing sandboxes from healthy cubelets. Matches the
// ListSandbox-style concurrency cap so a large cluster cannot open unbounded
// concurrent gRPC calls on the request path.
const clusterSandboxCollectConcurrency = 32

type clusterSandboxEntry struct {
	SandboxID string
	HostIP    string
}

// ResolveSandboxID accepts a short or full sandbox ID and returns the canonical full ID.
func ResolveSandboxID(ctx context.Context, input string) (string, error) {
	input = sandboxid.NormalizeInput(input)
	if input == "" {
		return "", sandboxid.ErrNotFound
	}

	candidates := localcache.ListKnownSandboxIDs()
	resolved, err := sandboxid.Resolve(input, candidates)
	if err == nil {
		return resolved, nil
	}
	// Local cache may be stale or partial: fall through to a cluster scan for
	// both missing and ambiguous prefixes so a unique live match can win.
	if !cacheResolveNeedsClusterFallback(err) {
		return "", err
	}

	entries := collectClusterSandboxIDs(ctx)
	cacheCollectedSandboxEntries(entries)
	clusterIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		clusterIDs = append(clusterIDs, entry.SandboxID)
	}
	merged := mergeSandboxIDs(candidates, clusterIDs)
	return sandboxid.Resolve(input, merged)
}

func cacheResolveNeedsClusterFallback(err error) bool {
	return errors.Is(err, sandboxid.ErrNotFound) || errors.Is(err, sandboxid.ErrAmbiguous)
}

func mergeSandboxIDs(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, id := range append(base, extra...) {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, id)
	}
	return merged
}

func cacheCollectedSandboxEntries(entries []clusterSandboxEntry) {
	for _, entry := range entries {
		localcache.SetSandboxCache(entry.SandboxID, &localcache.SandboxCache{
			SandboxID: entry.SandboxID,
			HostIP:    entry.HostIP,
		})
	}
}

func collectClusterSandboxIDs(ctx context.Context) []clusterSandboxEntry {
	nodes := localcache.GetHealthyNodes(-1)
	if len(nodes) == 0 {
		return nil
	}

	var (
		mu      sync.Mutex
		entries []clusterSandboxEntry
		wg      sync.WaitGroup
		sem     = make(chan struct{}, clusterSandboxCollectConcurrency)
	)
	for _, nodeItem := range nodes {
		if ctx.Err() != nil {
			break
		}
		nodeItem := nodeItem
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				return
			}

			hostIP := nodeItem.HostIP()
			cubeletReq := &cubebox.ListCubeSandboxRequest{
				Filter: &cubebox.CubeSandboxFilter{
					LabelSelector: map[string]string{"io.kubernetes.cri.container-type": "sandbox"},
				},
			}
			cubeRsp, err := cubelet.List(ctx, cubelet.GetCubeletAddr(hostIP), cubeletReq)
			if err != nil {
				log.G(ctx).Warnf("collect sandbox ids from %s failed: %v", hostIP, err)
				return
			}
			for _, sandbox := range cubeRsp.GetItems() {
				sandboxID := sandbox.GetId()
				if sandboxID == "" {
					continue
				}
				mu.Lock()
				entries = append(entries, clusterSandboxEntry{
					SandboxID: sandboxID,
					HostIP:    hostIP,
				})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return entries
}
