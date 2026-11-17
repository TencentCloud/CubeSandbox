// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build cgo

package cubecow

import "testing"

// Keep the lightweight object-info C symbol in the native test link even when
// integration tests that initialize a real engine are not enabled.
func TestGetObjectInfoNativeBinding(t *testing.T) {
	var engine Engine
	if _, err := engine.GetObjectInfo("missing"); err == nil {
		t.Fatal("GetObjectInfo on an unopened engine unexpectedly succeeded")
	}
}
