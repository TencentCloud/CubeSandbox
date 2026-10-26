// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisstream

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// stubRedis implements the Set/Del/Publish subset WriteState and
// ClearStateNotify need. Other UniversalClient methods panic if called.
type stubRedis struct {
	redis.UniversalClient
	publishCh chan publishCall
	xrev      []redis.XMessage
	xoldest   []redis.XMessage
	xread     []redis.XStream
	xreadErr  error
	exists    int64
	xinfo     *redis.XInfoStream
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

func (s *stubRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	return redis.NewStringResult("", redis.Nil)
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

func (s *stubRedis) XRevRangeN(_ context.Context, _, _, _ string, _ int64) *redis.XMessageSliceCmd {
	return redis.NewXMessageSliceCmdResult(s.xrev, nil)
}

func (s *stubRedis) XRangeN(_ context.Context, _, _, _ string, _ int64) *redis.XMessageSliceCmd {
	return redis.NewXMessageSliceCmdResult(s.xoldest, nil)
}

func (s *stubRedis) Exists(ctx context.Context, _ ...string) *redis.IntCmd {
	return redis.NewIntResult(s.exists, nil)
}

func (s *stubRedis) XInfoStream(ctx context.Context, stream string) *redis.XInfoStreamCmd {
	cmd := redis.NewXInfoStreamCmd(ctx, stream)
	cmd.SetVal(s.xinfo)
	return cmd
}

func (s *stubRedis) XRead(_ context.Context, _ *redis.XReadArgs) *redis.XStreamSliceCmd {
	return redis.NewXStreamSliceCmdResult(s.xread, s.xreadErr)
}

type recordingBus struct {
	mu   sync.Mutex
	ids  []string
	done chan struct{}
}

func TestLatestID(t *testing.T) {
	empty := New(&stubRedis{}, zap.NewNop())
	got, err := empty.LatestID(context.Background())
	if err != nil || got != "0-0" {
		t.Fatalf("empty LatestID = %q, %v", got, err)
	}

	withEvent := New(&stubRedis{xrev: []redis.XMessage{{ID: "123-4"}}}, zap.NewNop())
	got, err = withEvent.LatestID(context.Background())
	if err != nil || got != "123-4" {
		t.Fatalf("LatestID = %q, %v", got, err)
	}
}

func TestReadBroadcastAdvancesPastMalformedEvent(t *testing.T) {
	rdb := &stubRedis{
		xoldest: []redis.XMessage{{ID: "9-0"}},
		xread: []redis.XStream{{
			Stream: lifecycle.EventStreamKey,
			Messages: []redis.XMessage{
				{ID: "10-0", Values: map[string]interface{}{lifecycle.FieldOp: lifecycle.OpCreate}},
				{ID: "11-0", Values: map[string]interface{}{
					lifecycle.FieldOp:        lifecycle.OpDelete,
					lifecycle.FieldSandboxID: "sbx",
				}},
			},
		}}}
	client := New(rdb, zap.NewNop())

	events, cursor, err := client.Read(context.Background(), "9-0", time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "11-0" {
		t.Fatalf("cursor = %q, want 11-0", cursor)
	}
	if len(events) != 1 || events[0].SandboxID != "sbx" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReadRejectsCursorTrimmedDuringRead(t *testing.T) {
	rdb := &stubRedis{
		xread: []redis.XStream{{
			Stream: lifecycle.EventStreamKey,
			Messages: []redis.XMessage{{
				ID: "12-0",
				Values: map[string]interface{}{
					lifecycle.FieldOp:        lifecycle.OpDelete,
					lifecycle.FieldSandboxID: "sbx",
				},
			}},
		}},
		xoldest: []redis.XMessage{{ID: "11-0"}},
	}
	client := New(rdb, zap.NewNop())

	_, cursor, err := client.Read(context.Background(), "9-0", time.Second, 100)
	if !errors.Is(err, ErrCursorTrimmed) {
		t.Fatalf("Read() error = %v, want ErrCursorTrimmed", err)
	}
	if cursor != "9-0" {
		t.Fatalf("cursor advanced to %q after trim detection", cursor)
	}
}

func TestZeroCursorDetectsTrimmedHistory(t *testing.T) {
	client := New(&stubRedis{
		exists: 1,
		xinfo: &redis.XInfoStream{
			Length:       1,
			EntriesAdded: 3,
		},
	}, zap.NewNop())

	valid, err := client.CursorValid(context.Background(), "0-0")
	if err != nil || valid {
		t.Fatalf("CursorValid(0-0 after trim) = (%v, %v), want (false, nil)", valid, err)
	}
}

func TestNonZeroCursorRejectsEmptyStream(t *testing.T) {
	client := New(&stubRedis{}, zap.NewNop())
	valid, err := client.CursorValid(context.Background(), "9-0")
	if err != nil || valid {
		t.Fatalf("CursorValid(nonzero, empty stream) = (%v, %v), want (false, nil)", valid, err)
	}
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
	server := miniredis.RunT(t)
	base := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = base.Close() })
	rdb := &stubRedis{UniversalClient: base, publishCh: make(chan publishCall)}
	bus := &recordingBus{done: make(chan struct{})}
	c := New(rdb, zap.NewNop())
	c.SetNotifyEnabled(true)
	c.SetLocalBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	if err := c.WriteState(ctx, "sbx", "running", time.Second); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	cancel()
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
