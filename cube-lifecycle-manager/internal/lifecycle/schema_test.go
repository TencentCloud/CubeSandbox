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

func TestTransitionValue(t *testing.T) {
	if got := TransitionValue(StateResuming, "pod-1:9f3ab2c1"); got != "resuming@pod-1:9f3ab2c1" {
		t.Fatalf("TransitionValue = %q, want %q", got, "resuming@pod-1:9f3ab2c1")
	}
	if got := TransitionValue(StateResuming, ""); got != "resuming" {
		t.Fatalf("empty owner must yield the bare legacy form, got %q", got)
	}
}

func TestIsTransition(t *testing.T) {
	cases := []struct {
		cur, transition string
		want            bool
	}{
		{"pausing", StatePausing, true},           // legacy bare form
		{"pausing@pod-1:x", StatePausing, true},   // owner-tagged form
		{"resuming@pod-2:y", StateResuming, true}, // owner-tagged form
		{"paused", StatePausing, false},           // terminal is not a transition
		{"pausing@pod-1:x", StateResuming, false}, // different transition
		{"pausingx", StatePausing, false},         // prefix without "@" must not match
		{"killing@pod-1:z", StateKilling, true},   // kill transition
		{"killed", StateKilling, false},           // terminal killed
		{"running", StateResuming, false},         // terminal running
	}
	for _, tc := range cases {
		if got := IsTransition(tc.cur, tc.transition); got != tc.want {
			t.Errorf("IsTransition(%q, %q) = %v, want %v", tc.cur, tc.transition, got, tc.want)
		}
	}
}
