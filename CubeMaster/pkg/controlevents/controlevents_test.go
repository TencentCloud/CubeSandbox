// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controlevents

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

type recordedCall struct {
	cmd  string
	args []interface{}
}

type fakeRedis struct {
	mu       sync.Mutex
	calls    []recordedCall
	failXADD bool
}

func (f *fakeRedis) Do(cmd string, args ...interface{}) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{cmd: cmd, args: args})
	if cmd == "XADD" && f.failXADD {
		return nil, errors.New("XADD boom")
	}
	return "OK", nil
}

func (f *fakeRedis) snapshot() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func TestPublisher_PublishNodeIsolation_Isolate(t *testing.T) {
	r := &fakeRedis{}
	p := NewPublisher(r)
	p.origin = "master-a"

	p.PublishNodeIsolation(context.Background(), "node-1", true)

	calls := r.snapshot()
	if len(calls) != 1 || calls[0].cmd != "XADD" {
		t.Fatalf("want 1 XADD, got %+v", calls)
	}
	args := calls[0].args
	if args[0] != EventStreamKey {
		t.Fatalf("stream key: got %v", args[0])
	}
	if args[5] != FieldOp || args[6] != OpNodeIsolate {
		t.Fatalf("op wrong: %+v", args)
	}
	if args[7] != FieldNodeID || args[8] != "node-1" {
		t.Fatalf("node_id wrong: %+v", args)
	}
	// payload is last pair
	var payload []byte
	for i := 0; i+1 < len(args); i++ {
		if args[i] == FieldPayload {
			b, ok := args[i+1].([]byte)
			if !ok {
				t.Fatalf("payload type %T", args[i+1])
			}
			payload = b
		}
	}
	if len(payload) == 0 {
		t.Fatal("missing payload")
	}
	var got IsolationPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if !got.SchedulingDisabled || got.Origin != "master-a" {
		t.Fatalf("payload wrong: %+v", got)
	}
}

func TestPublisher_PublishNodeIsolation_Unisolate(t *testing.T) {
	r := &fakeRedis{}
	p := NewPublisher(r)
	p.PublishNodeIsolation(context.Background(), "node-2", false)

	calls := r.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].args[6] != OpNodeUnisolate {
		t.Fatalf("want unisolate op, got %v", calls[0].args[6])
	}
}

func TestPublisher_PublishNodeIsolation_XADDFailureSwallowed(t *testing.T) {
	r := &fakeRedis{failXADD: true}
	p := NewPublisher(r)
	// Must not panic.
	p.PublishNodeIsolation(context.Background(), "node-1", true)
}

func TestPublisher_PublishNodeIsolation_EmptyNodeNoop(t *testing.T) {
	r := &fakeRedis{}
	p := NewPublisher(r)
	p.PublishNodeIsolation(context.Background(), "", true)
	if len(r.snapshot()) != 0 {
		t.Fatal("expected no redis calls for empty node_id")
	}
}

func TestApplyIsolationEvent_IsolateUnisolateIdempotent(t *testing.T) {
	var mu sync.Mutex
	applied := make([]bool, 0, 4)

	apply := func(_ context.Context, nodeID string, disabled bool) error {
		if nodeID != "n1" {
			t.Fatalf("node_id=%s", nodeID)
		}
		mu.Lock()
		applied = append(applied, disabled)
		mu.Unlock()
		return nil
	}
	h := NewIsolationHandler(apply)

	payload, _ := json.Marshal(IsolationPayload{SchedulingDisabled: true})
	for i := 0; i < 2; i++ {
		if err := h(context.Background(), Event{
			Op: OpNodeIsolate, NodeID: "n1", Payload: payload,
		}); err != nil {
			t.Fatalf("isolate: %v", err)
		}
	}
	payloadOff, _ := json.Marshal(IsolationPayload{SchedulingDisabled: false})
	if err := h(context.Background(), Event{
		Op: OpNodeUnisolate, NodeID: "n1", Payload: payloadOff,
	}); err != nil {
		t.Fatalf("unisolate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 3 || !applied[0] || !applied[1] || applied[2] {
		t.Fatalf("applied sequence wrong: %v", applied)
	}
}

func TestApplyIsolationEvent_UnknownOpIgnored(t *testing.T) {
	called := false
	h := NewIsolationHandler(func(context.Context, string, bool) error {
		called = true
		return nil
	})
	if err := h(context.Background(), Event{Op: "node.labels", NodeID: "n1"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called {
		t.Fatal("unknown op should not call apply")
	}
}

func TestApplyIsolationEvent_MissingNodeID(t *testing.T) {
	h := NewIsolationHandler(func(context.Context, string, bool) error { return nil })
	if err := h(context.Background(), Event{Op: OpNodeIsolate}); err == nil {
		t.Fatal("expected error for missing node_id")
	}
}

func TestParseXReadReply(t *testing.T) {
	reply := []interface{}{
		[]interface{}{
			[]byte(EventStreamKey),
			[]interface{}{
				[]interface{}{
					[]byte("1710000000000-0"),
					[]interface{}{
						[]byte(FieldOp), []byte(OpNodeIsolate),
						[]byte(FieldNodeID), []byte("node-9"),
						[]byte(FieldTimestamp), []byte("1710000000000"),
						[]byte(FieldPayload), []byte(`{"scheduling_disabled":true}`),
					},
				},
			},
		},
	}
	events, lastID, err := parseXReadReply(reply)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lastID != "1710000000000-0" {
		t.Fatalf("lastID=%s", lastID)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	ev := events[0]
	if ev.Op != OpNodeIsolate || ev.NodeID != "node-9" || ev.Timestamp != 1710000000000 {
		t.Fatalf("event wrong: %+v", ev)
	}
	var p IsolationPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil || !p.SchedulingDisabled {
		t.Fatalf("payload wrong: %v %+v", err, p)
	}
}

func TestParseXReadReply_Empty(t *testing.T) {
	events, lastID, err := parseXReadReply([]interface{}{})
	if err != nil || len(events) != 0 || lastID != "" {
		t.Fatalf("got events=%v lastID=%q err=%v", events, lastID, err)
	}
}
