// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

func TestNewState_MapsTerminalStates(t *testing.T) {
	paused := &lifecycle.StatePayload{State: lifecycle.StatePaused, Actor: lifecycle.ActorCubeMaster, Source: "api"}
	ev, ok := NewState("sb-1", paused, nil, "1725030000000-0", 1725030000000)
	if !ok {
		t.Fatal("expected ok for paused")
	}
	if ev.Event != string(EventPaused) {
		t.Fatalf("event = %q, want %q", ev.Event, EventPaused)
	}
	if ev.State != "paused" || ev.Actor != "cubemaster" || ev.Source != "api" {
		t.Fatalf("state fields not copied: %+v", ev)
	}

	running := &lifecycle.StatePayload{State: lifecycle.StateRunning}
	ev, ok = NewState("sb-1", running, nil, "id", 0)
	if !ok {
		t.Fatal("expected ok for running")
	}
	if ev.Event != string(EventResumed) {
		t.Fatalf("event = %q, want %q", ev.Event, EventResumed)
	}
}

func TestNewState_DropsTransitionMarkersAndNil(t *testing.T) {
	for _, sp := range []*lifecycle.StatePayload{
		nil,
		{State: "pausing"},
		{State: "resuming"},
		{State: "killing"},
		{State: ""},
	} {
		if _, ok := NewState("sb-1", sp, nil, "id", 0); ok {
			t.Fatalf("expected not-ok for state payload %+v", sp)
		}
	}
}

func TestNewCreated_CarriesMetaContext(t *testing.T) {
	meta := lifecycle.SandboxLifecycleMeta{
		SandboxID:      "sb-1",
		TemplateID:     "tpl-python",
		HostID:         "node-1",
		InstanceType:   "cubebox",
		TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60),
		AutoPause:      true,
		AutoResume:     true,
		CreatedAt:      1725030000000,
		EndAt:          1725033600000,
	}
	ev := NewCreated(meta, "stream-0", 1725030000000)

	b, err := ev.MarshalBody()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	checks := map[string]any{
		"event":           "sandbox.created",
		"event_id":        "stream-0",
		"sandbox_id":      "sb-1",
		"template_id":     "tpl-python",
		"host_id":         "node-1",
		"instance_type":   "cubebox",
		"timeout_seconds": float64(60),
		"auto_pause":      true,
		"auto_resume":     true,
		"created_at":      float64(1725030000000),
		"end_at":          float64(1725033600000),
	}
	for k, want := range checks {
		if got := m[k]; got != want {
			t.Errorf("field %q = %v, want %v", k, got, want)
		}
	}

	ts, err := time.Parse(time.RFC3339, m["timestamp"].(string))
	if err != nil {
		t.Fatalf("timestamp not RFC3339: %v", err)
	}
	if ts.Location() != time.UTC || ts.UnixMilli() != 1725030000000 {
		t.Fatalf("timestamp not UTC epoch: %+v", ts)
	}
}

func TestNewDeleted_NilMetaOmitsContext(t *testing.T) {
	ev := NewDeleted("sb-1", nil, "stream-1", 1725030000000)
	b, err := ev.MarshalBody()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != "sandbox.deleted" {
		t.Fatalf("event = %v", m["event"])
	}
	if _, present := m["template_id"]; present {
		t.Error("template_id should be omitted when meta is nil")
	}
}

func TestNewDeleted_WithMetaCarriesTemplate(t *testing.T) {
	meta := lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1", TemplateID: "tpl-1"}
	ev := NewDeleted("sb-1", &meta, "stream-1", 0)
	b, _ := ev.MarshalBody()
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["template_id"] != "tpl-1" {
		t.Fatalf("template_id = %v, want tpl-1", m["template_id"])
	}
}

func TestNewUpdated_SetsEventAndTimeout(t *testing.T) {
	meta := lifecycle.SandboxLifecycleMeta{
		SandboxID:      "sb-1",
		TemplateID:     "tpl-1",
		TimeoutSeconds: lifecycle.TimeoutSecondsPtr(300),
	}
	ev := NewUpdated(meta, "stream-u", 1725030000000)

	b, err := ev.MarshalBody()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != "sandbox.timeout.updated" {
		t.Fatalf("event = %v, want sandbox.timeout.updated", m["event"])
	}
	if m["event_id"] != "stream-u" {
		t.Fatalf("event_id = %v, want stream-u", m["event_id"])
	}
	if m["timeout"] != float64(300) {
		t.Fatalf("timeout = %v, want 300", m["timeout"])
	}
}

func TestEvent_TimeoutFieldAbsentOnOtherEvents(t *testing.T) {
	// Non-update events must not carry the `timeout` key (omitempty).
	ev := NewCreated(lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1"}, "stream-c", 1725030000000)
	b, _ := ev.MarshalBody()
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, present := m["timeout"]; present {
		t.Error("`timeout` should be omitted on sandbox.created")
	}
}
