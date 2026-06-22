// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import "testing"

// TestSchemaConstants is a fence: if anyone changes one of these values
// without updating CubeMaster/pkg/lifecycle/schema.go in lockstep, the test
// fails and the diff makes the divergence obvious.
//
// Source of truth lives in CubeMaster; we hardcode the same values here so
// the sidecar can be built without taking a build-time dependency on the
// CubeMaster module. Whenever you touch the constants, update both files in
// the same commit.
func TestSchemaConstants(t *testing.T) {
	cases := []struct {
		name, got, want string
	}{
		{"MetaKey", MetaKey, "cube:v1:shared:sandbox:lifecycle:meta"},
		{"EventStreamKey", EventStreamKey, "cube:v1:shared:sandbox:lifecycle:events"},
		{"StateKey", StateKey("test-id"), "cube:v1:shared:sandbox:lifecycle:state:test-id"},
		{"OpCreate", OpCreate, "create"},
		{"OpDelete", OpDelete, "delete"},
		{"OpUpdate", OpUpdate, "update"},
		{"FieldOp", FieldOp, "op"},
		{"FieldSandboxID", FieldSandboxID, "sandbox_id"},
		{"FieldPayload", FieldPayload, "payload"},
		{"FieldTimestamp", FieldTimestamp, "ts"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %q != %q (CubeMaster schema drifted?)",
				c.name, c.got, c.want)
		}
	}
	if EventStreamMaxLen != 100000 {
		t.Errorf("EventStreamMaxLen = %d; CubeMaster expects 100000", EventStreamMaxLen)
	}
}
