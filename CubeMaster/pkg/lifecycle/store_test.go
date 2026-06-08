// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
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
	in := SandboxLifecycleMeta{
		SandboxID:      "sbx-1",
		TemplateID:     "tpl-1",
		HostID:         "host-1",
		HostIP:         "10.0.0.1",
		InstanceType:   "cubebox",
		TimeoutSeconds: 60,
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
	if out != in {
		t.Fatalf("round trip mismatch: got %+v want %+v", out, in)
	}
}

func TestStore_PublishCreate_HappyPath(t *testing.T) {
	r := &fakeRedis{}
	s := NewStore(r)

	meta := &SandboxLifecycleMeta{
		SandboxID:      "sbx-42",
		TimeoutSeconds: 60,
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
	if got.SandboxID != "sbx-42" || !got.AutoPause || got.TimeoutSeconds != 60 {
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

	if got := len(r.snapshot()); got != 0 {
		t.Fatalf("disabled store should make zero calls, got %d", got)
	}
}

func TestStore_NilGuards(t *testing.T) {
	// nil store, nil doer, nil meta, empty id — all must be safe.
	var s *Store
	s.PublishCreate(context.Background(), &SandboxLifecycleMeta{SandboxID: "x"})
	s.PublishDelete(context.Background(), "x")

	s2 := NewStore(nil)
	s2.PublishCreate(context.Background(), &SandboxLifecycleMeta{SandboxID: "x"})
	s2.PublishDelete(context.Background(), "x")

	r := &fakeRedis{}
	s3 := NewStore(r)
	s3.PublishCreate(context.Background(), nil)
	s3.PublishCreate(context.Background(), &SandboxLifecycleMeta{})
	s3.PublishDelete(context.Background(), "")
	if got := len(r.snapshot()); got != 0 {
		t.Fatalf("nil/empty inputs must not reach Redis, got %d calls", got)
	}
}
