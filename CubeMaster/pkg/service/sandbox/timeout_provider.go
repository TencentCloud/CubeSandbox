// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"sync"
)

// TimeoutProvider is the contract sandbox_timeout.go uses to mutate the
// lifecycle metadata channel without taking a hard build-time dependency on
// pkg/lifecycle. lifecycle.Init() injects a concrete implementation at startup;
type TimeoutProvider interface {
	RefreshTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) (endAtMs int64, err error)
	LookupEndAt(ctx context.Context, sandboxID string) (endAtMs int64, err error)
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
