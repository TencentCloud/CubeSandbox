// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// recordedCall captures one Do invocation for later assertion.
type recordedCall struct {
	cmd  string
	args []interface{}
}

type fakeRedis struct {
	mu    sync.Mutex
	calls []recordedCall
	// errOn maps command name -> error to return on the Nth call (counter-based).
	failHSET bool
	failHDEL bool
	failXADD bool
}

func (f *fakeRedis) Do(cmd string, args ...interface{}) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{cmd: cmd, args: args})
	switch cmd {
	case "HSET":
		if f.failHSET {
			return nil, errors.New("HSET boom")
		}
	case "HDEL":
		if f.failHDEL {
			return nil, errors.New("HDEL boom")
		}
	case "XADD":
		if f.failXADD {
			return nil, errors.New("XADD boom")
		}
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

func TestSandboxLifecycleMeta_JSONRoundTrip(t *testing.T) {
	timeout := 60
	in := SandboxLifecycleMeta{
		SandboxID:      "sbx-1",
		TemplateID:     "tpl-1",
		HostID:         "host-1",
		HostIP:         "10.0.0.1",
		InstanceType:   "cubebox",
		TimeoutSeconds: &timeout,
		AutoPause:      true,
		AutoResume:     true,
		CreatedAt:      1700000000000,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out SandboxLifecycleMeta
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// TimeoutSeconds is a pointer now, so compare by value (DeepEqual) rather
	// than == which would compare pointer identity.
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip mismatch: got %+v want %+v", out, in)
	}
}

func TestStore_PublishCreate_HappyPath(t *testing.T) {
	r := &fakeRedis{}
	s := NewStore(r)

	timeout := 60
	meta := &SandboxLifecycleMeta{
		SandboxID:      "sbx-42",
		TimeoutSeconds: &timeout,
		AutoPause:      true,
	}
	s.PublishCreate(context.Background(), meta)

	calls := r.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 calls (HSET + XADD), got %d: %+v", len(calls), calls)
	}
	if calls[0].cmd != "HSET" || calls[0].args[0] != MetaKey || calls[0].args[1] != "sbx-42" {
		t.Fatalf("HSET args wrong: %+v", calls[0])
	}
	if calls[1].cmd != "XADD" || calls[1].args[0] != EventStreamKey {
		t.Fatalf("XADD args wrong: %+v", calls[1])
	}
	// XADD args layout: stream, MAXLEN, ~, N, *, op, OpCreate, sandbox_id, id, ts, ms, payload, bytes
	if calls[1].args[5] != FieldOp || calls[1].args[6] != OpCreate {
		t.Fatalf("XADD op field wrong: %+v", calls[1].args)
	}
	if calls[1].args[7] != FieldSandboxID || calls[1].args[8] != "sbx-42" {
		t.Fatalf("XADD sandbox_id field wrong: %+v", calls[1].args)
	}
	// payload must round-trip through JSON
	payloadBytes, ok := calls[0].args[2].([]byte)
	if !ok {
		t.Fatalf("HSET payload not bytes: %T", calls[0].args[2])
	}
	var got SandboxLifecycleMeta
	if err := json.Unmarshal(payloadBytes, &got); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if got.SandboxID != "sbx-42" || !got.AutoPause || got.TimeoutSeconds == nil || *got.TimeoutSeconds != 60 {
		t.Fatalf("payload wrong: %+v", got)
	}
}

func TestStore_PublishCreate_HSETFailureStillEmitsXADD(t *testing.T) {
	r := &fakeRedis{failHSET: true}
	s := NewStore(r)

	s.PublishCreate(context.Background(), &SandboxLifecycleMeta{SandboxID: "sbx-1"})

	calls := r.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 calls even when HSET fails, got %d", len(calls))
	}
	if calls[1].cmd != "XADD" {
		t.Fatalf("expected XADD as second call, got %s", calls[1].cmd)
	}
}

func TestStore_PublishDelete(t *testing.T) {
	r := &fakeRedis{}
	s := NewStore(r)

	s.PublishDelete(context.Background(), "sbx-9")

	calls := r.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want HDEL + XADD, got %d", len(calls))
	}
	if calls[0].cmd != "HDEL" || calls[0].args[1] != "sbx-9" {
		t.Fatalf("HDEL wrong: %+v", calls[0])
	}
	if calls[1].cmd != "XADD" || calls[1].args[6] != OpDelete {
		t.Fatalf("XADD op should be %q, got %+v", OpDelete, calls[1].args)
	}
	// OpDelete carries no payload field.
	for _, a := range calls[1].args {
		if s, ok := a.(string); ok && s == FieldPayload {
			t.Fatalf("delete event should not include payload field: %+v", calls[1].args)
		}
	}
}

func TestStore_DisabledIsNoOp(t *testing.T) {
	r := &fakeRedis{}
	s := NewStore(r)
	s.SetEnabled(false)

	s.PublishCreate(context.Background(), &SandboxLifecycleMeta{SandboxID: "sbx-1"})
	s.PublishDelete(context.Background(), "sbx-1")
	s.PublishState(context.Background(), "sbx-1", StatePaused, "api")

	if got := len(r.snapshot()); got != 0 {
		t.Fatalf("disabled store should make zero calls, got %d", got)
	}
}

func TestStore_PublishState_HappyPath(t *testing.T) {
	cases := []struct {
		name  string
		state string
	}{
		{"paused", StatePaused},
		{"running", StateRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRedis{}
			s := NewStore(r)

			s.PublishState(context.Background(), "sbx-1", tc.state, "api")

			calls := r.snapshot()
			if len(calls) != 1 {
				t.Fatalf("want single XADD, got %d: %+v", len(calls), calls)
			}
			c := calls[0]
			if c.cmd != "XADD" || c.args[0] != EventStreamKey {
				t.Fatalf("XADD target wrong: %+v", c)
			}
			// State events MUST NOT touch MetaKey.
			for _, prev := range calls {
				if prev.cmd == "HSET" || prev.cmd == "HDEL" {
					t.Fatalf("state event must not mutate MetaKey: %+v", prev)
				}
			}
			if c.args[5] != FieldOp || c.args[6] != OpState {
				t.Fatalf("XADD op wrong: %+v", c.args)
			}
			if c.args[7] != FieldSandboxID || c.args[8] != "sbx-1" {
				t.Fatalf("XADD sandbox_id wrong: %+v", c.args)
			}
			// Locate payload field.
			var payloadBytes []byte
			for i := 0; i < len(c.args); i++ {
				if s, ok := c.args[i].(string); ok && s == FieldPayload && i+1 < len(c.args) {
					if b, bok := c.args[i+1].([]byte); bok {
						payloadBytes = b
					}
				}
			}
			if payloadBytes == nil {
				t.Fatalf("payload not found in XADD args: %+v", c.args)
			}
			var got StatePayload
			if err := json.Unmarshal(payloadBytes, &got); err != nil {
				t.Fatalf("payload json: %v", err)
			}
			if got.State != tc.state {
				t.Fatalf("payload state = %q, want %q", got.State, tc.state)
			}
			if got.Actor != ActorCubeMaster {
				t.Fatalf("payload actor = %q, want %q", got.Actor, ActorCubeMaster)
			}
			if got.Source != "api" {
				t.Fatalf("payload source = %q, want api", got.Source)
			}
		})
	}
}

func TestStore_PublishState_InvalidState(t *testing.T) {
	cases := []string{"", "pausing", "resuming", "PAUSED", "unknown"}
	for _, state := range cases {
		t.Run(state, func(t *testing.T) {
			r := &fakeRedis{}
			s := NewStore(r)
			s.PublishState(context.Background(), "sbx-1", state, "api")
			if got := len(r.snapshot()); got != 0 {
				t.Fatalf("invalid state %q must not reach Redis, got %d calls", state, got)
			}
		})
	}
}

func TestStore_PublishState_EmptySandboxID(t *testing.T) {
	r := &fakeRedis{}
	s := NewStore(r)
	s.PublishState(context.Background(), "", StatePaused, "api")
	if got := len(r.snapshot()); got != 0 {
		t.Fatalf("empty sandbox id must not reach Redis, got %d calls", got)
	}
}

func TestStore_PublishState_XADDFailureSwallowed(t *testing.T) {
	r := &fakeRedis{failXADD: true}
	s := NewStore(r)
	// Must not panic and must not propagate the error.
	s.PublishState(context.Background(), "sbx-1", StateRunning, "api")
	calls := r.snapshot()
	if len(calls) != 1 || calls[0].cmd != "XADD" {
		t.Fatalf("expected single XADD attempt, got %+v", calls)
	}
}

func TestStore_NilGuards(t *testing.T) {
	// nil store, nil doer, nil meta, empty id — all must be safe.
	var s *Store
	s.PublishCreate(context.Background(), &SandboxLifecycleMeta{SandboxID: "x"})
	s.PublishDelete(context.Background(), "x")
	s.PublishState(context.Background(), "x", StatePaused, "api")

	s2 := NewStore(nil)
	s2.PublishCreate(context.Background(), &SandboxLifecycleMeta{SandboxID: "x"})
	s2.PublishDelete(context.Background(), "x")
	s2.PublishState(context.Background(), "x", StatePaused, "api")

	r := &fakeRedis{}
	s3 := NewStore(r)
	s3.PublishCreate(context.Background(), nil)
	s3.PublishCreate(context.Background(), &SandboxLifecycleMeta{})
	s3.PublishDelete(context.Background(), "")
	s3.PublishState(context.Background(), "", StatePaused, "api")
	if got := len(r.snapshot()); got != 0 {
		t.Fatalf("nil/empty inputs must not reach Redis, got %d calls", got)
	}
}
