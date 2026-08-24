// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"sync"
)

// TimeoutProvider lets sandbox services read and update lifecycle metadata
// without taking a hard build-time dependency on pkg/lifecycle.
// lifecycle.Init() injects a concrete implementation at startup.
type TimeoutProvider interface {
	RefreshTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) (endAtMs int64, err error)
	RebaseTimeoutWindow(ctx context.Context, sandboxID string) (endAtMs int64, err error)
	LookupTimeout(ctx context.Context, sandboxID string) (endAtMs int64, timeoutSeconds *int, err error)
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

// LookupSandboxTimeout returns the projected endAt and configured timeout from
// one lifecycle metadata lookup. A nil timeout means that the metadata could
// not be resolved; NeverTimeout (-1) explicitly identifies no deadline.
func LookupSandboxTimeout(ctx context.Context, sandboxID string) (int64, *int) {
	p := getTimeoutProvider()
	if p == nil || sandboxID == "" {
		return 0, nil
	}
	endAt, timeoutSeconds, err := p.LookupTimeout(ctx, sandboxID)
	if err != nil {
		return 0, nil
	}
	return endAt, timeoutSeconds
}
