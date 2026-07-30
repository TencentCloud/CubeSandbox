// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestBuildPayloadLifecycleMapping(t *testing.T) {
	create, ok := buildPayload(LifecycleEvent{
		StreamID:  "1-0",
		EventID:   "event-create",
		Op:        "create",
		SandboxID: "sandbox-1",
		Timestamp: 1700000000000,
		Payload:   json.RawMessage(`{"template_id":"template-1","host_id":"node-1","instance_type":"cubebox"}`),
	})
	if !ok {
		t.Fatal("create event was rejected")
	}
	if create.Event != EventSandboxCreated || create.TemplateID != "template-1" || create.HostID != "node-1" {
		t.Fatalf("unexpected create payload: %+v", create)
	}
	if !create.Timestamp.Equal(time.UnixMilli(1700000000000).UTC()) {
		t.Fatalf("timestamp = %s", create.Timestamp)
	}

	cases := []struct {
		state string
		want  string
	}{
		{"paused", EventSandboxPaused},
		{"running", EventSandboxResumed},
	}
	for _, test := range cases {
		payload, ok := buildPayload(LifecycleEvent{
			StreamID:  "2-0",
			EventID:   "event-state",
			Op:        "state",
			SandboxID: "sandbox-1",
			Timestamp: 1700000001000,
			Payload:   json.RawMessage(`{"state":"` + test.state + `","actor":"cubemaster","source":"api"}`),
		})
		if !ok || payload.Event != test.want {
			t.Fatalf("state %q mapped to %+v, ok=%v", test.state, payload, ok)
		}
	}

	deleted, ok := buildPayload(LifecycleEvent{
		StreamID:  "3-0",
		EventID:   "event-delete",
		Op:        "delete",
		SandboxID: "sandbox-1",
		Timestamp: 1700000002000,
		Payload:   json.RawMessage(`{"template_id":"template-1"}`),
	})
	if !ok || deleted.Event != EventSandboxDeleted || deleted.TemplateID != "template-1" {
		t.Fatalf("delete mapped to %+v, ok=%v", deleted, ok)
	}
	if _, ok := buildPayload(LifecycleEvent{Op: "update"}); ok {
		t.Fatal("metadata update must not become an external webhook event")
	}
}

func TestDecodeMessagePreservesEventAndStreamIDs(t *testing.T) {
	event, ok := decodeMessage(redis.XMessage{
		ID: "1700000000000-0",
		Values: map[string]any{
			"event_id":   "80639f37-1b79-42c4-93ff-a33cd93c5eef",
			"op":         "create",
			"sandbox_id": "sandbox-1",
			"ts":         int64(1700000000000),
			"payload":    []byte(`{"template_id":"template-1"}`),
		},
	})
	if !ok {
		t.Fatal("message was rejected")
	}
	if event.StreamID != "1700000000000-0" || event.EventID != "80639f37-1b79-42c4-93ff-a33cd93c5eef" {
		t.Fatalf("IDs not preserved: %+v", event)
	}
}
