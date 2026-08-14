// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"encoding/json"
	"testing"
)

func TestSandboxStateFromInt(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{0, "unknown"},
		{1, "running"},
		{2, "unknown"},
		{3, "unknown"},
		{4, "pausing"},
		{5, "paused"},
		{99, "unknown"},
	}
	for _, test := range tests {
		if got := SandboxStateFromInt(test.status); got != test.want {
			t.Errorf("SandboxStateFromInt(%d) = %q, want %q", test.status, got, test.want)
		}
	}
}

func TestSandboxStateFromRaw(t *testing.T) {
	tests := []struct {
		raw  json.RawMessage
		want string
	}{
		{json.RawMessage(`1`), "running"},
		{json.RawMessage(`"running"`), "running"},
		{json.RawMessage(`4`), "pausing"},
		{json.RawMessage(`"pausing"`), "pausing"},
		{json.RawMessage(`5`), "paused"},
		{json.RawMessage(`"pause"`), "paused"},
		{json.RawMessage(`0`), "unknown"},
		{json.RawMessage(`2`), "unknown"},
		{json.RawMessage(`3`), "unknown"},
		{json.RawMessage(`"3"`), "unknown"},
		{json.RawMessage(`"UNKNOWN"`), "unknown"},
		{json.RawMessage(`99`), "unknown"},
		{json.RawMessage(`"invalid"`), "unknown"},
		{json.RawMessage(`null`), "unknown"},
		{json.RawMessage(`{`), "unknown"},
		{nil, "unknown"},
	}
	for _, test := range tests {
		if got := SandboxStateFromRaw(test.raw); got != test.want {
			t.Errorf("SandboxStateFromRaw(%s) = %q, want %q", test.raw, got, test.want)
		}
	}
}

func TestTransformSandboxListPreservesUnknownState(t *testing.T) {
	raw := json.RawMessage(`{
		"ret":{"ret_code":0},
		"data":[{"sandbox_id":"sb-unknown","status":3,"annotations":{},"labels":{}}]
	}`)
	items, ok := TransformSandboxList(raw).([]map[string]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("TransformSandboxList() = %#v, want one sandbox", TransformSandboxList(raw))
	}
	if got := items[0]["state"]; got != "unknown" {
		t.Errorf("state = %v, want unknown", got)
	}
}
