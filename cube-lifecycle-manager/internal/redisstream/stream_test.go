// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisstream

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// stubRedis implements the Set/Del/Publish subset WriteState and
// ClearStateNotify need. Other UniversalClient methods panic if called.
type stubRedis struct {
	redis.UniversalClient
	publishCh chan publishCall
}

type publishCall struct {
	ctx     context.Context
	channel string
	payload string
}

func (s *stubRedis) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	cmd.SetVal("OK")
	return cmd
}

func (s *stubRedis) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(1)
	return cmd
}

func (s *stubRedis) Publish(ctx context.Context, channel string, message interface{}) *redis.IntCmd {
	var payload string
	switch v := message.(type) {
	case string:
		payload = v
	case []byte:
		payload = string(v)
	default:
		payload = ""
	}
	s.publishCh <- publishCall{ctx: ctx, channel: channel, payload: payload}
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(1)
	return cmd
}

type recordingBus struct {
	mu   sync.Mutex
	ids  []string
	done chan struct{}
}

func (b *recordingBus) Publish(sandboxID string) {
	b.mu.Lock()
	b.ids = append(b.ids, sandboxID)
	b.mu.Unlock()
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}

func TestWriteState_PublishDetachedAndNonBlocking(t *testing.T) {
	rdb := &stubRedis{publishCh: make(chan publishCall)}
	bus := &recordingBus{done: make(chan struct{})}
	c := New(rdb, zap.NewNop())
	c.SetNotifyEnabled(true)
	c.SetLocalBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	if err := c.WriteState(ctx, "sbx", "running", time.Second); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if time.Since(started) > 50*time.Millisecond {
		t.Fatal("WriteState blocked on Redis PUBLISH")
	}

	select {
	case <-bus.done:
	case <-time.After(time.Second):
		t.Fatal("local bus was not published synchronously")
	}

	select {
	case call := <-rdb.publishCh:
		if call.channel != lifecycle.EventChannel {
			t.Fatalf("channel = %q", call.channel)
		}
		if call.payload != `{"sandbox_id":"sbx"}` {
			t.Fatalf("payload = %q", call.payload)
		}
		if call.ctx == ctx {
			t.Fatal("PUBLISH used the cancelled caller context")
		}
		deadline, ok := call.ctx.Deadline()
		if !ok {
			t.Fatal("PUBLISH context has no timeout")
		}
		remain := time.Until(deadline)
		if remain <= 0 || remain > notifyPublishTimeout+50*time.Millisecond {
			t.Fatalf("PUBLISH timeout remaining = %v, want (0, %v]", remain, notifyPublishTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("Redis PUBLISH was not issued")
	}
}

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

// readGroupStub records the XReadGroup args and returns prebuilt streams,
// so the ">" (new) vs "0" (pending) distinction is testable.
type readGroupStub struct {
	redis.UniversalClient
	args    *redis.XReadGroupArgs
	streams []redis.XStream
}

func (s *readGroupStub) XReadGroup(ctx context.Context, args *redis.XReadGroupArgs) *redis.XStreamSliceCmd {
	s.args = args
	cmd := redis.NewXStreamSliceCmd(ctx)
	cmd.SetVal(s.streams)
	return cmd
}

func TestReadGroup_UsesNewEntriesID(t *testing.T) {
	s := &readGroupStub{}
	c := New(s, zap.NewNop())
	_, err := c.ReadGroup(context.Background(), "g", "c", time.Second, 10)
	if err != nil {
		t.Fatalf("ReadGroup: %v", err)
	}
	if s.args == nil || s.args.Streams[1] != ">" {
		t.Fatalf("ReadGroup stream ID = %v, want %q", s.args, ">")
	}
}

func TestReadPending_ReadsPendingListAndDecodes(t *testing.T) {
	s := &readGroupStub{
		streams: []redis.XStream{{Messages: []redis.XMessage{
			{ID: "1-0", Values: map[string]interface{}{
				lifecycle.FieldOp:        lifecycle.OpCreate,
				lifecycle.FieldSandboxID: "sb-1",
				lifecycle.FieldTimestamp: "1725030000000",
				lifecycle.FieldPayload:   `{"sandbox_id":"sb-1","template_id":"tpl-1"}`,
			}},
			{ID: "2-0", Values: map[string]interface{}{
				lifecycle.FieldOp:        lifecycle.OpState,
				lifecycle.FieldSandboxID: "sb-1",
				lifecycle.FieldPayload:   `{"state":"paused"}`,
			}},
		}}},
	}
	c := New(s, zap.NewNop())
	events, err := c.ReadPending(context.Background(), "g", "c")
	if err != nil {
		t.Fatalf("ReadPending: %v", err)
	}
	if s.args == nil || s.args.Streams[1] != "0" {
		t.Fatalf("ReadPending stream ID = %v, want %q", s.args, "0")
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Op != lifecycle.OpCreate || events[0].SandboxID != "sb-1" || events[0].Meta == nil || events[0].Meta.TemplateID != "tpl-1" {
		t.Fatalf("create decoded wrong: %+v", events[0])
	}
	if events[0].Timestamp != 1725030000000 {
		t.Fatalf("create ts = %d, want 1725030000000", events[0].Timestamp)
	}
	if events[1].Op != lifecycle.OpState || events[1].State == nil || events[1].State.State != "paused" {
		t.Fatalf("state decoded wrong: %+v", events[1])
	}
}

func TestReadPending_EmptyPendingReturnsEmpty(t *testing.T) {
	s := &readGroupStub{}
	c := New(s, zap.NewNop())
	events, err := c.ReadPending(context.Background(), "g", "c")
	if err != nil {
		t.Fatalf("ReadPending: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0", len(events))
	}
}

// xAutoClaimStub records the XAutoClaim args and returns prebuilt messages,
// so ReclaimStale's minIdle passthrough and decode path are testable.
type xAutoClaimStub struct {
	redis.UniversalClient
	args    *redis.XAutoClaimArgs
	messages []redis.XMessage
}

func (s *xAutoClaimStub) XAutoClaim(ctx context.Context, args *redis.XAutoClaimArgs) *redis.XAutoClaimCmd {
	s.args = args
	cmd := redis.NewXAutoClaimCmd(ctx)
	cmd.SetVal(s.messages, "0-0")
	return cmd
}

func TestReclaimStale_PassesMinIdleAndDecodes(t *testing.T) {
	s := &xAutoClaimStub{
		messages: []redis.XMessage{
			{ID: "1-0", Values: map[string]interface{}{
				lifecycle.FieldOp:        lifecycle.OpDelete,
				lifecycle.FieldSandboxID: "sb-1",
			}},
		},
	}
	c := New(s, zap.NewNop())
	events, err := c.ReclaimStale(context.Background(), "g", "c", 60*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if s.args == nil || s.args.MinIdle != 60*time.Second {
		t.Fatalf("MinIdle = %v, want 60s", s.args.MinIdle)
	}
	if s.args.Consumer != "c" {
		t.Fatalf("Consumer = %q, want c", s.args.Consumer)
	}
	if len(events) != 1 || events[0].Op != lifecycle.OpDelete || events[0].SandboxID != "sb-1" {
		t.Fatalf("decoded = %+v, want one delete for sb-1", events)
	}
}

func TestReclaimStale_EmptyReturnsEmpty(t *testing.T) {
	s := &xAutoClaimStub{}
	c := New(s, zap.NewNop())
	events, err := c.ReclaimStale(context.Background(), "g", "c", time.Second)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0", len(events))
	}
}
