// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// TimeoutProvider is the contract sandbox_timeout.go uses to mutate the
// lifecycle metadata channel without taking a hard build-time dependency on
// pkg/lifecycle. lifecycle.Init() injects a concrete implementation at startup;
type TimeoutProvider interface {
	RefreshTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) (endAtMs int64, err error)
	LookupEndAt(ctx context.Context, sandboxID string) (endAtMs int64, err error)
}

type batchTimeoutProvider interface {
	LookupEndAts(ctx context.Context, sandboxIDs []string) (map[string]int64, error)
}

var (
	timeoutProviderMu sync.RWMutex
	timeoutProvider   TimeoutProvider
)

// SetTimeoutProvider installs the singleton implementation. lifecycle.Init
// calls it exactly once during process startup.
func SetTimeoutProvider(p TimeoutProvider) {
	timeoutProviderMu.Lock()
	timeoutProvider = p
	timeoutProviderMu.Unlock()
}

func getTimeoutProvider() TimeoutProvider {
	timeoutProviderMu.RLock()
	defer timeoutProviderMu.RUnlock()
	return timeoutProvider
}

// LookupSandboxEndAt is a thin convenience wrapper around the installed
// TimeoutProvider's LookupEndAt.
func LookupSandboxEndAt(ctx context.Context, sandboxID string) int64 {
	p := getTimeoutProvider()
	if p == nil || sandboxID == "" {
		return 0
	}
	endAt, err := p.LookupEndAt(ctx, sandboxID)
	if err != nil {
		return 0
	}
	return endAt
}

func lookupSandboxEndAts(ctx context.Context, sandboxIDs []string) map[string]int64 {
	p := getTimeoutProvider()
	if p == nil || len(sandboxIDs) == 0 {
		return nil
	}
	if batch, ok := p.(batchTimeoutProvider); ok {
		endAts, err := batch.LookupEndAts(ctx, sandboxIDs)
		if err != nil {
			log.G(ctx).Warnf("lookup endAt batch failed: %v", err)
			return nil
		}
		return endAts
	}
	endAts := make(map[string]int64, len(sandboxIDs))
	for _, sandboxID := range sandboxIDs {
		if sandboxID == "" {
			continue
		}
		endAt, err := p.LookupEndAt(ctx, sandboxID)
		if err == nil {
			endAts[sandboxID] = endAt
		}
	}
	return endAts
}
