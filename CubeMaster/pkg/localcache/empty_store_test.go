// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package localcache

import "testing"

// TestGetNodeWithoutInitMatchesEmptyStore locks the contract #1489 callers
// rely on: HostFacts queries must be fail-closed before localcache.Init,
// matching the pre-migration global.nodes empty map. Do not replace the
// package-level store or call Init — a missing id must return (nil, false).
func TestGetNodeWithoutInitMatchesEmptyStore(t *testing.T) {
	got, ok := GetNode("node-store-exists-without-init-missing")
	if ok {
		t.Fatalf("missing node must return ok=false, got %v", got)
	}
	if got != nil {
		t.Fatalf("missing node must return nil, got %+v", got)
	}
}
