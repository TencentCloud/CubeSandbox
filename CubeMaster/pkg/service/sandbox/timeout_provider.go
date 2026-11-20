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

// DeadlineInfo is the live timeout state of one sandbox, read from the
// lifecycle metadata (the same source queries use), not from the create-time
// spec: a later set-timeout changes only the metadata.
type DeadlineInfo struct {
	EndAtMs        int64
	TimeoutSeconds *int
	// Found is false when no metadata row exists, which is "unknown", not
	// "never".
	Found bool
}

type deadlineProvider interface {
	LookupDeadlines(ctx context.Context, sandboxIDs []string) (map[string]DeadlineInfo, error)
}

// LookupSandboxDeadlines returns the live deadline state for the given ids
// when the installed provider supports it. A provider without the extension
// yields nil, which callers treat as unknown.
func LookupSandboxDeadlines(ctx context.Context, sandboxIDs []string) map[string]DeadlineInfo {
	p := getTimeoutProvider()
	if p == nil || len(sandboxIDs) == 0 {
		return nil
	}
	dp, ok := p.(deadlineProvider)
	if !ok {
		return nil
	}
	out, err := dp.LookupDeadlines(ctx, sandboxIDs)
	if err != nil {
		log.G(ctx).Warnf("lookup deadline batch failed: %v", err)
		return nil
	}
	return out
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
