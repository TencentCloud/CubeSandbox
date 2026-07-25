// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisstream

import (
	"encoding/json"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

func metaEntry(t *testing.T, op, sid string, meta lifecycle.SandboxLifecycleMeta) redis.XMessage {
	t.Helper()
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return redis.XMessage{
		ID: "1-0",
		Values: map[string]interface{}{
			lifecycle.FieldOp:        op,
			lifecycle.FieldSandboxID: sid,
			lifecycle.FieldPayload:   string(payload),
			lifecycle.FieldTimestamp: "1700000000000",
		},
	}
}

func stateEntry(t *testing.T, sid string, sp lifecycle.StatePayload) redis.XMessage {
	t.Helper()
	payload, err := json.Marshal(sp)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return redis.XMessage{
		ID: "2-0",
		Values: map[string]interface{}{
			lifecycle.FieldOp:        lifecycle.OpState,
			lifecycle.FieldSandboxID: sid,
			lifecycle.FieldPayload:   string(payload),
			lifecycle.FieldTimestamp: "1700000001000",
		},
	}
}

func TestDecodeEvent_Create(t *testing.T) {
	msg := metaEntry(t, lifecycle.OpCreate, "sbx-1", lifecycle.SandboxLifecycleMeta{
		SandboxID:  "sbx-1",
		AutoPause:  true,
		AutoResume: false,
	})
	ev := decodeEvent(msg)
	if ev == nil {
		t.Fatal("decodeEvent returned nil")
	}
	if ev.Op != lifecycle.OpCreate || ev.SandboxID != "sbx-1" {
		t.Fatalf("wrong op/sid: %+v", ev)
	}
	if ev.Meta == nil || !ev.Meta.AutoPause {
		t.Fatalf("meta not decoded: %+v", ev.Meta)
	}
	if ev.State != nil {
		t.Fatalf("state must be nil for create: %+v", ev.State)
	}
	if ev.Timestamp != 1700000000000 {
		t.Fatalf("timestamp not decoded: %d", ev.Timestamp)
	}
}

func TestDecodeEvent_State_Paused(t *testing.T) {
	msg := stateEntry(t, "sbx-1", lifecycle.StatePayload{
		State:  lifecycle.StatePaused,
		Actor:  lifecycle.ActorCubeMaster,
		Source: "api",
	})
	ev := decodeEvent(msg)
	if ev == nil {
		t.Fatal("decodeEvent returned nil")
	}
	if ev.Op != lifecycle.OpState || ev.SandboxID != "sbx-1" {
		t.Fatalf("wrong op/sid: %+v", ev)
	}
	if ev.Meta != nil {
		t.Fatalf("state event must not carry meta: %+v", ev.Meta)
	}
	if ev.State == nil {
		t.Fatal("state payload not decoded")
	}
	if ev.State.State != lifecycle.StatePaused {
		t.Fatalf("state = %q, want %q", ev.State.State, lifecycle.StatePaused)
	}
	if ev.State.Actor != lifecycle.ActorCubeMaster {
		t.Fatalf("actor = %q", ev.State.Actor)
	}
	if ev.State.Source != "api" {
		t.Fatalf("source = %q", ev.State.Source)
	}
}

func TestDecodeEvent_State_Running(t *testing.T) {
	msg := stateEntry(t, "sbx-2", lifecycle.StatePayload{
		State: lifecycle.StateRunning,
		Actor: lifecycle.ActorCubeMaster,
	})
	ev := decodeEvent(msg)
	if ev == nil || ev.State == nil || ev.State.State != lifecycle.StateRunning {
		t.Fatalf("running state decode failed: %+v", ev)
	}
}

func TestDecodeEvent_State_MissingPayload(t *testing.T) {
	msg := redis.XMessage{
		ID: "3-0",
		Values: map[string]interface{}{
			lifecycle.FieldOp:        lifecycle.OpState,
			lifecycle.FieldSandboxID: "sbx-1",
			// no payload
		},
	}
	ev := decodeEvent(msg)
	if ev == nil {
		t.Fatal("decodeEvent returned nil for state event without payload")
	}
	if ev.State != nil {
		t.Fatalf("state must be nil when payload absent: %+v", ev.State)
	}
	// downstream statesync is responsible for warning; we just must not panic.
}

func TestDecodeEvent_State_MalformedPayload(t *testing.T) {
	msg := redis.XMessage{
		ID: "4-0",
		Values: map[string]interface{}{
			lifecycle.FieldOp:        lifecycle.OpState,
			lifecycle.FieldSandboxID: "sbx-1",
			lifecycle.FieldPayload:   "not-json",
		},
	}
	ev := decodeEvent(msg)
	if ev == nil {
		t.Fatal("decodeEvent returned nil for malformed state payload")
	}
	if ev.State != nil {
		t.Fatalf("state must be nil when payload is malformed: %+v", ev.State)
	}
}

func TestDecodeEvent_MissingOpOrSID(t *testing.T) {
	cases := []redis.XMessage{
		{ID: "a", Values: map[string]interface{}{lifecycle.FieldSandboxID: "x"}},
		{ID: "b", Values: map[string]interface{}{lifecycle.FieldOp: "create"}},
		{ID: "c", Values: map[string]interface{}{}},
	}
	for _, m := range cases {
		if ev := decodeEvent(m); ev != nil {
			t.Fatalf("expected nil for %+v, got %+v", m, ev)
		}
	}
}

func TestDecodeEvent_UnknownOpSurvives(t *testing.T) {
	// Old CLM should tolerate future op codes: decoder produces an Event with
	// no Meta/State payload; upstream handleEvent falls into the default arm
	// and warn+ACKs.
	msg := redis.XMessage{
		ID: "5-0",
		Values: map[string]interface{}{
			lifecycle.FieldOp:        "future-op",
			lifecycle.FieldSandboxID: "sbx-1",
			lifecycle.FieldPayload:   `{"foo":"bar"}`,
		},
	}
	ev := decodeEvent(msg)
	if ev == nil {
		t.Fatal("decoder must not drop unknown ops (upstream handles them)")
	}
	if ev.Op != "future-op" || ev.SandboxID != "sbx-1" {
		t.Fatalf("op/sid corrupted: %+v", ev)
	}
	if ev.Meta != nil || ev.State != nil {
		t.Fatalf("unknown op must not populate Meta/State: %+v", ev)
	}
}
