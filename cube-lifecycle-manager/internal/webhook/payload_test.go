// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
)

func TestMapEvent_Create(t *testing.T) {
	ev := redisstream.Event{
		StreamID:  "1-0",
		Op:        lifecycle.OpCreate,
		SandboxID: "sbx-1",
		Meta: &lifecycle.SandboxLifecycleMeta{
			SandboxID:  "sbx-1",
			TemplateID: "tpl-9",
		},
		Timestamp: 1700000000000,
	}
	p := MapEvent(ev, nil)
	if p == nil {
		t.Fatal("MapEvent returned nil for create")
	}
	if p.Event != EventCreated || p.SandboxID != "sbx-1" || p.TemplateID != "tpl-9" {
		t.Fatalf("wrong payload: %+v", p)
	}
	if p.Version != PayloadVersion {
		t.Fatalf("version = %q", p.Version)
	}
	want := time.UnixMilli(1700000000000).UTC().Format(time.RFC3339)
	if p.Timestamp != want {
		t.Fatalf("timestamp = %q, want %q", p.Timestamp, want)
	}
	if p.State != "" {
		t.Fatalf("create must not carry state: %+v", p)
	}
}

func TestMapEvent_CreateWithoutMeta(t *testing.T) {
	ev := redisstream.Event{Op: lifecycle.OpCreate, SandboxID: "sbx-1", Timestamp: 1}
	p := MapEvent(ev, nil)
	if p == nil || p.Event != EventCreated {
		t.Fatalf("create without meta must still map: %+v", p)
	}
	if p.TemplateID != "" {
		t.Fatalf("template id must be empty: %+v", p)
	}
}

func TestMapEvent_Delete(t *testing.T) {
	ev := redisstream.Event{Op: lifecycle.OpDelete, SandboxID: "sbx-1", Timestamp: 1}
	p := MapEvent(ev, nil)
	if p == nil || p.Event != EventDeleted || p.SandboxID != "sbx-1" {
		t.Fatalf("wrong delete payload: %+v", p)
	}
}

func TestMapEvent_State(t *testing.T) {
	cases := []struct {
		state string
		event string
	}{
		{lifecycle.StatePaused, EventPaused},
		{lifecycle.StateRunning, EventResumed},
	}
	for _, tc := range cases {
		ev := redisstream.Event{
			Op:        lifecycle.OpState,
			SandboxID: "sbx-1",
			State:     &lifecycle.StatePayload{State: tc.state},
			Timestamp: 1,
		}
		p := MapEvent(ev, nil)
		if p == nil {
			t.Fatalf("state %q: MapEvent returned nil", tc.state)
		}
		if p.Event != tc.event || p.State != tc.state {
			t.Fatalf("state %q: wrong payload %+v", tc.state, p)
		}
	}
}

func TestMapEvent_SkipsNonTransitions(t *testing.T) {
	cases := map[string]redisstream.Event{
		"metadata update":       {Op: lifecycle.OpUpdate, SandboxID: "sbx-1", Timestamp: 1},
		"unknown op":            {Op: "future-op", SandboxID: "sbx-1", Timestamp: 1},
		"state without payload": {Op: lifecycle.OpState, SandboxID: "sbx-1", Timestamp: 1},
		"transition marker": {
			Op:        lifecycle.OpState,
			SandboxID: "sbx-1",
			State:     &lifecycle.StatePayload{State: "pausing"},
			Timestamp: 1,
		},
	}
	for name, ev := range cases {
		if p := MapEvent(ev, nil); p != nil {
			t.Fatalf("%s: expected nil, got %+v", name, p)
		}
	}
}

func TestMapEvent_FallbackTimestamp(t *testing.T) {
	fixed := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	now := func() time.Time { return fixed }
	ev := redisstream.Event{Op: lifecycle.OpDelete, SandboxID: "sbx-1"} // no ts
	p := MapEvent(ev, now)
	if p == nil {
		t.Fatal("MapEvent returned nil")
	}
	if p.Timestamp != fixed.Format(time.RFC3339) {
		t.Fatalf("timestamp = %q, want fallback %q", p.Timestamp, fixed.Format(time.RFC3339))
	}
}
