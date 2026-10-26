// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import (
	"encoding/json"
	"testing"
)

func TestStateNotify_JSONRoundTrip(t *testing.T) {
	in := StateNotify{
		SandboxID: "sbx-1",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out StateNotify
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if in != out {
		t.Fatalf("round-trip lost fields: in=%+v out=%+v", in, out)
	}
}

func TestEventChannel(t *testing.T) {
	const want = "cube:v1:shared:sandbox:lifecycle:notify"
	if EventChannel != want {
		t.Fatalf("EventChannel = %q, want %q", EventChannel, want)
	}
}

func TestLeaderLeaseKey(t *testing.T) {
	const want = "cube:v1:shared:lock:lifecycle-manager:leader"
	if LeaderLeaseKey != want {
		t.Fatalf("LeaderLeaseKey = %q, want %q", LeaderLeaseKey, want)
	}
}
