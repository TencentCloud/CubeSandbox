// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package admission

import "github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"

// FakeRecoveryManager is a RecoveryManager test double that records every
// OnEviction call. Exported so other test suites (e.g. the integration
// package) don't need their own copy.
type FakeRecoveryManager struct {
	Events []*types.EvictionEvent
}

// OnEviction implements RecoveryManager.
func (f *FakeRecoveryManager) OnEviction(event *types.EvictionEvent) {
	f.Events = append(f.Events, event)
}
