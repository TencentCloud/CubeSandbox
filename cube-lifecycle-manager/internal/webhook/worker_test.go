// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
)

// fakeStream implements Stream with scripted pending/live batches and records
// ACKed stream IDs in order.
type fakeStream struct {
	mu      sync.Mutex
	pending [][]redisstream.Event
	live    [][]redisstream.Event
	acks    []string
}

func (f *fakeStream) EnsureGroup(context.Context, string) error { return nil }

func (f *fakeStream) ReadGroup(ctx context.Context, _, _ string, _ time.Duration, _ int) ([]redisstream.Event, error) {
	f.mu.Lock()
	if len(f.live) > 0 {
		batch := f.live[0]
		f.live = f.live[1:]
		f.mu.Unlock()
		return batch, nil
	}
	f.mu.Unlock()
	// No scripted events: behave like an idle blocking read.
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeStream) ReadPending(_ context.Context, _, _ string, _ int) ([]redisstream.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil, nil
	}
	batch := f.pending[0]
	f.pending = f.pending[1:]
	return batch, nil
}

func (f *fakeStream) Ack(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, id)
	return nil
}

func (f *fakeStream) acked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acks...)
}

func createEvent(id, sid string) redisstream.Event {
	return redisstream.Event{
		StreamID:  id,
		Op:        lifecycle.OpCreate,
		SandboxID: sid,
		Meta:      &lifecycle.SandboxLifecycleMeta{SandboxID: sid, TemplateID: "tpl-1"},
		Timestamp: 1700000000000,
	}
}

func deleteEvent(id, sid string) redisstream.Event {
	return redisstream.Event{
		StreamID:  id,
		Op:        lifecycle.OpDelete,
		SandboxID: sid,
		Timestamp: 1700000001000,
	}
}

func newTestWorker(ep Endpoint, stream Stream, d *Deliverer) *Worker {
	return NewWorker(ep, stream, d, "consumer-1", time.Second, zap.NewNop())
}

func TestWorker_RedeliversPendingOnStartup(t *testing.T) {
	r := &receiver{}
	srv := httptest.NewServer(r)
	defer srv.Close()

	stream := &fakeStream{
		pending: [][]redisstream.Event{{createEvent("1-0", "sbx-1")}},
		// no live events; Run blocks on ReadGroup until cancelled
	}
	w := newTestWorker(Endpoint{URL: srv.URL}, stream, newTestDeliverer(testConfig()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait for the ACK, not just the HTTP request: the request being seen by
	// the receiver does not mean delivery has completed yet.
	deadline := time.After(2 * time.Second)
	for len(stream.acked()) == 0 {
		select {
		case <-deadline:
			t.Fatal("pending event was not redelivered")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if r.count() != 1 {
		t.Fatalf("requests = %d, want 1", r.count())
	}
	if acks := stream.acked(); len(acks) != 1 || acks[0] != "1-0" {
		t.Fatalf("acks = %v, want [1-0]", acks)
	}
}

func TestWorker_DeliversInStreamOrder(t *testing.T) {
	r := &receiver{}
	srv := httptest.NewServer(r)
	defer srv.Close()

	stream := &fakeStream{}
	w := newTestWorker(Endpoint{URL: srv.URL}, stream, newTestDeliverer(testConfig()))

	batch := []redisstream.Event{createEvent("1-0", "sbx-1"), deleteEvent("2-0", "sbx-1")}
	if err := w.deliverBatch(context.Background(), w.endpoint.Group(), batch); err != nil {
		t.Fatalf("deliverBatch: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("requests = %d, want 2", r.count())
	}
	if got := r.got(0).header.Get("X-Cube-Event"); got != EventCreated {
		t.Fatalf("first event = %q, want %q", got, EventCreated)
	}
	if got := r.got(1).header.Get("X-Cube-Event"); got != EventDeleted {
		t.Fatalf("second event = %q, want %q", got, EventDeleted)
	}
	acks := stream.acked()
	if len(acks) != 2 || acks[0] != "1-0" || acks[1] != "2-0" {
		t.Fatalf("acks = %v, want [1-0 2-0]", acks)
	}
}

func TestWorker_SkipsUnsubscribedAndNonTransitionEvents(t *testing.T) {
	r := &receiver{}
	srv := httptest.NewServer(r)
	defer srv.Close()

	stream := &fakeStream{}
	ep := Endpoint{URL: srv.URL, Events: []string{EventCreated}}
	w := newTestWorker(ep, stream, newTestDeliverer(testConfig()))

	// Deleted is not subscribed; update is not an external transition.
	batch := []redisstream.Event{
		deleteEvent("1-0", "sbx-1"),
		{StreamID: "2-0", Op: lifecycle.OpUpdate, SandboxID: "sbx-1", Timestamp: 1},
		createEvent("3-0", "sbx-1"),
	}
	if err := w.deliverBatch(context.Background(), ep.Group(), batch); err != nil {
		t.Fatalf("deliverBatch: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("requests = %d, want 1 (only sandbox.created)", r.count())
	}
	if got := r.got(0).header.Get("X-Cube-Event"); got != EventCreated {
		t.Fatalf("event = %q", got)
	}
	if acks := stream.acked(); len(acks) != 3 {
		t.Fatalf("acks = %v, want all 3 acked", acks)
	}
}

func TestWorker_AcksTerminalDeliveryFailure(t *testing.T) {
	// 400 is permanent: the event must be ACKed (after logging) so a single
	// rejecting endpoint cannot stall its consumer group forever.
	r := &receiver{statuses: []int{400}}
	srv := httptest.NewServer(r)
	defer srv.Close()

	stream := &fakeStream{}
	w := newTestWorker(Endpoint{URL: srv.URL}, stream, newTestDeliverer(testConfig()))
	if ack := w.handleEvent(context.Background(), createEvent("1-0", "sbx-1")); !ack {
		t.Fatal("terminal rejection must be ACKed")
	}
}

func TestWorker_KeepsEventPendingOnShutdown(t *testing.T) {
	r := &receiver{}
	srv := httptest.NewServer(r)
	defer srv.Close()

	stream := &fakeStream{}
	w := newTestWorker(Endpoint{URL: srv.URL}, stream, newTestDeliverer(testConfig()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // delivery is interrupted before it starts
	if ack := w.handleEvent(ctx, createEvent("1-0", "sbx-1")); ack {
		t.Fatal("interrupted delivery must stay pending (no ACK)")
	}
	if acks := stream.acked(); len(acks) != 0 {
		t.Fatalf("acks = %v, want none", acks)
	}
}
