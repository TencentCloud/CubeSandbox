// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
)

// fakeSender records deliveries synchronously; used with Run's consumers via
// pollUntil (delivery is async from the test's perspective).
type fakeSender struct {
	mu    sync.Mutex
	calls []sendCall
	failN int // fail the next N sends
}

type sendCall struct {
	ep Endpoint
	ev Event
}

func (f *fakeSender) Send(_ context.Context, ep Endpoint, ev Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sendCall{ep, ev})
	if f.failN > 0 {
		f.failN--
		return errors.New("boom")
	}
	return nil
}

func (f *fakeSender) all() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sendCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeStream is an in-memory streamReader. ReadGroup blocks on the incoming
// channel until an event is pushed or ctx is cancelled, so consumers don't
// spin. ReadPending returns the preloaded pending list (crash-recovery path);
// ReclaimStale returns and clears the preloaded stale list (periodic safety
// net), mirroring how real XAUTOCLAIM removes claimed entries from pending.
type fakeStream struct {
	mu       sync.Mutex
	pending  []redisstream.Event
	stale    []redisstream.Event
	incoming chan redisstream.Event
	acked    []string
}

func newFakeStream() *fakeStream {
	return &fakeStream{incoming: make(chan redisstream.Event, 100)}
}

func (f *fakeStream) push(ev redisstream.Event) { f.incoming <- ev }

func (f *fakeStream) setPending(evs []redisstream.Event) {
	f.mu.Lock()
	f.pending = evs
	f.mu.Unlock()
}

func (f *fakeStream) setStale(evs []redisstream.Event) {
	f.mu.Lock()
	f.stale = evs
	f.mu.Unlock()
}

func (f *fakeStream) EnsureGroup(context.Context, string) error { return nil }

func (f *fakeStream) ReadGroup(ctx context.Context, _, _ string, _ time.Duration, _ int) ([]redisstream.Event, error) {
	select {
	case ev := <-f.incoming:
		return []redisstream.Event{ev}, nil
	case <-ctx.Done():
		return nil, nil
	}
}

func (f *fakeStream) ReadPending(context.Context, string, string) ([]redisstream.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]redisstream.Event, len(f.pending))
	copy(out, f.pending)
	return out, nil
}

func (f *fakeStream) ReclaimStale(context.Context, string, string, time.Duration) ([]redisstream.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.stale
	f.stale = nil
	return out, nil
}

func (f *fakeStream) Ack(_ context.Context, _ string, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, id)
	return nil
}

func (f *fakeStream) ackedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.acked))
	copy(out, f.acked)
	return out
}

func newTestManager(fs *fakeStream, sender Sender, endpoints []Endpoint, events []string) *Manager {
	return New(Options{
		Endpoints: endpoints,
		Events:    events,
		Sender:    sender,
		Stream:    fs,
		Workers:   1, // deterministic single consumer
		Log:       zap.NewNop(),
	})
}

func pollUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

// runManager starts m.Run in a goroutine and returns cancel + done so tests
// can stop it (defer cancel(); <-done) and drain the consumer goroutine.
func runManager(t *testing.T, m *Manager) (context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Run(ctx)
	}()
	return cancel, done
}

func streamEvent(id, op, sid string, meta *lifecycle.SandboxLifecycleMeta, sp *lifecycle.StatePayload) redisstream.Event {
	return redisstream.Event{StreamID: id, Op: op, SandboxID: sid, Meta: meta, State: sp, Timestamp: 1000}
}

var epAll = Endpoint{ID: "all", URL: "http://all", Enabled: true}

func TestManager_DeliversMappedEventsInOrder(t *testing.T) {
	fs := newFakeStream()
	sender := &fakeSender{}
	m := newTestManager(fs, sender, []Endpoint{epAll}, nil)
	cancel, done := runManager(t, m)
	defer func() { cancel(); <-done }()

	meta := lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1", TemplateID: "tpl-1", TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60)}
	fs.push(streamEvent("1-0", lifecycle.OpCreate, "sb-1", &meta, nil))
	fs.push(streamEvent("2-0", lifecycle.OpState, "sb-1", nil, &lifecycle.StatePayload{State: lifecycle.StatePaused}))
	fs.push(streamEvent("3-0", lifecycle.OpUpdate, "sb-1", &meta, nil))
	fs.push(streamEvent("4-0", lifecycle.OpState, "sb-1", nil, &lifecycle.StatePayload{State: lifecycle.StateRunning}))
	fs.push(streamEvent("5-0", lifecycle.OpDelete, "sb-1", nil, nil))

	pollUntil(t, func() bool { return sender.count() == 5 })
	calls := sender.all()

	want := []struct {
		event      string
		eventID    string
		templateID string
		state      string
	}{
		{"sandbox.created", "1-0", "tpl-1", ""},
		{"sandbox.paused", "2-0", "tpl-1", "paused"},
		{"sandbox.timeout.updated", "3-0", "tpl-1", ""},
		{"sandbox.resumed", "4-0", "tpl-1", "running"},
		{"sandbox.deleted", "5-0", "tpl-1", ""}, // template_id from the meta cache
	}
	for i, w := range want {
		got := calls[i].ev
		if got.Event != w.event {
			t.Errorf("call %d event = %q, want %q", i, got.Event, w.event)
		}
		if got.EventID != w.eventID {
			t.Errorf("call %d event_id = %q, want %q", i, got.EventID, w.eventID)
		}
		if got.TemplateID != w.templateID {
			t.Errorf("call %d template_id = %q, want %q", i, got.TemplateID, w.templateID)
		}
		if got.State != w.state {
			t.Errorf("call %d state = %q, want %q", i, got.State, w.state)
		}
	}

	pollUntil(t, func() bool { return len(fs.ackedIDs()) == 5 })
}

func TestManager_AcksAfterDelivery(t *testing.T) {
	fs := newFakeStream()
	sender := &fakeSender{}
	m := newTestManager(fs, sender, []Endpoint{epAll}, nil)
	cancel, done := runManager(t, m)
	defer func() { cancel(); <-done }()

	fs.push(streamEvent("9-0", lifecycle.OpCreate, "sb-9",
		&lifecycle.SandboxLifecycleMeta{SandboxID: "sb-9"}, nil))

	pollUntil(t, func() bool { return len(fs.ackedIDs()) == 1 })
	if acked := fs.ackedIDs(); acked[0] != "9-0" {
		t.Fatalf("acked = %v, want [9-0]", acked)
	}
	// The event must be delivered before it is acked.
	if n := sender.count(); n != 1 {
		t.Fatalf("sender deliveries = %d, want 1 (ack must follow delivery)", n)
	}
}

func TestManager_ReclaimsPendingOnStartup(t *testing.T) {
	fs := newFakeStream()
	sender := &fakeSender{}
	m := newTestManager(fs, sender, []Endpoint{epAll}, nil)

	// Simulate a crash that left create+delete unacked: reclaim must rebuild
	// the meta cache from the create so the delete carries template_id.
	fs.setPending([]redisstream.Event{
		streamEvent("1-0", lifecycle.OpCreate, "sb-x", &lifecycle.SandboxLifecycleMeta{SandboxID: "sb-x", TemplateID: "tpl-x"}, nil),
		streamEvent("2-0", lifecycle.OpDelete, "sb-x", nil, nil),
	})

	cancel, done := runManager(t, m)
	defer func() { cancel(); <-done }()

	pollUntil(t, func() bool { return sender.count() == 2 })
	calls := sender.all()
	if calls[1].ev.Event != "sandbox.deleted" || calls[1].ev.TemplateID != "tpl-x" {
		t.Fatalf("reclaimed delete = %+v, want sandbox.deleted with template tpl-x", calls[1].ev)
	}
}

func TestManager_PeriodicReclaimRetriesStuckPending(t *testing.T) {
	fs := newFakeStream()
	sender := &fakeSender{}
	// RetryInterval tuned down so the periodic reclaimLoop fires within the
	// test's poll window; the fake's ReclaimStale mirrors XAUTOCLAIM (claims
	// and removes, so each stuck entry is retried once per pass).
	m := New(Options{
		Endpoints:     []Endpoint{epAll},
		Sender:        sender,
		Stream:        fs,
		Workers:       1,
		RetryInterval: 50 * time.Millisecond,
		StaleMinIdle:  time.Millisecond,
		Log:           zap.NewNop(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Run(ctx)
	}()
	defer func() { cancel(); <-done }()

	// An entry stuck in pending (e.g. a dead peer replica read it and never
	// acked): the periodic reclaimLoop must pick it up and deliver it.
	fs.setStale([]redisstream.Event{
		streamEvent("1-0", lifecycle.OpCreate, "sb-stuck",
			&lifecycle.SandboxLifecycleMeta{SandboxID: "sb-stuck", TemplateID: "tpl-stuck"}, nil),
		streamEvent("2-0", lifecycle.OpDelete, "sb-stuck", nil, nil),
	})

	pollUntil(t, func() bool { return sender.count() == 2 })
	calls := sender.all()
	if calls[0].ev.Event != "sandbox.created" || calls[0].ev.TemplateID != "tpl-stuck" {
		t.Fatalf("reclaimed create = %+v, want sandbox.created with template tpl-stuck", calls[0].ev)
	}
	// delete carries template_id because the reclaimed create populated the cache.
	if calls[1].ev.Event != "sandbox.deleted" || calls[1].ev.TemplateID != "tpl-stuck" {
		t.Fatalf("reclaimed delete = %+v, want sandbox.deleted with template tpl-stuck", calls[1].ev)
	}
	// Both stuck entries are acked after delivery.
	pollUntil(t, func() bool { return len(fs.ackedIDs()) == 2 })
}

func TestManager_GlobalFilter(t *testing.T) {
	fs := newFakeStream()
	sender := &fakeSender{}
	m := newTestManager(fs, sender, []Endpoint{epAll}, []string{"sandbox.paused"})
	cancel, done := runManager(t, m)
	defer func() { cancel(); <-done }()

	fs.push(streamEvent("1-0", lifecycle.OpCreate, "sb-1", &lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1"}, nil))
	fs.push(streamEvent("2-0", lifecycle.OpState, "sb-1", nil, &lifecycle.StatePayload{State: lifecycle.StatePaused}))
	fs.push(streamEvent("3-0", lifecycle.OpState, "sb-1", nil, &lifecycle.StatePayload{State: lifecycle.StateRunning}))

	pollUntil(t, func() bool { return sender.count() == 1 })
	calls := sender.all()
	if calls[0].ev.Event != "sandbox.paused" {
		t.Fatalf("delivered %q, want only sandbox.paused under the global filter", calls[0].ev.Event)
	}
	// The filtered events are still consumed (acked), not left pending.
	pollUntil(t, func() bool { return len(fs.ackedIDs()) == 3 })
}

func TestManager_PerEndpointSubscription(t *testing.T) {
	fs := newFakeStream()
	sender := &fakeSender{}
	pausedOnly := Endpoint{ID: "paused-only", URL: "http://p", Events: []string{"sandbox.paused"}, Enabled: true}
	m := newTestManager(fs, sender, []Endpoint{pausedOnly, epAll}, nil)
	cancel, done := runManager(t, m)
	defer func() { cancel(); <-done }()

	fs.push(streamEvent("1-0", lifecycle.OpCreate, "sb-1", &lifecycle.SandboxLifecycleMeta{SandboxID: "sb-1"}, nil))
	fs.push(streamEvent("2-0", lifecycle.OpState, "sb-1", nil, &lifecycle.StatePayload{State: lifecycle.StatePaused}))

	pollUntil(t, func() bool { return sender.count() == 3 })
	var pausedOnlyCount, allCount int
	for _, c := range sender.all() {
		switch c.ep.ID {
		case "paused-only":
			pausedOnlyCount++
			if c.ev.Event != "sandbox.paused" {
				t.Fatalf("paused-only endpoint got %q", c.ev.Event)
			}
		case "all":
			allCount++
		}
	}
	if pausedOnlyCount != 1 || allCount != 2 {
		t.Fatalf("paused-only got %d, all got %d; want 1 and 2", pausedOnlyCount, allCount)
	}
}

func TestManager_UnmappableEventIsDroppedAndAcked(t *testing.T) {
	fs := newFakeStream()
	sender := &fakeSender{}
	m := newTestManager(fs, sender, []Endpoint{epAll}, nil)
	cancel, done := runManager(t, m)
	defer func() { cancel(); <-done }()

	fs.push(redisstream.Event{StreamID: "1-0", Op: "bogus", SandboxID: "sb-1"})

	pollUntil(t, func() bool { return len(fs.ackedIDs()) == 1 })
	if n := sender.count(); n != 0 {
		t.Fatalf("sender deliveries = %d, want 0", n)
	}
	dropped, _, _ := m.Stats()
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}

func TestManager_RunReturnsCtxErrOnCancel(t *testing.T) {
	fs := newFakeStream()
	m := newTestManager(fs, &fakeSender{}, []Endpoint{epAll}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestManager_NilStreamReturnsError(t *testing.T) {
	m := New(Options{
		Endpoints: []Endpoint{epAll},
		Sender:    &fakeSender{},
		Log:       zap.NewNop(),
	})
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("Run = nil, want error when Stream is not configured")
	}
}

func TestManager_EndpointCRUD(t *testing.T) {
	m := New(Options{Log: zap.NewNop()})

	created, err := m.AddEndpoint(Endpoint{URL: "http://host/x", Events: []string{"sandbox.paused"}, Enabled: true})
	if err != nil || created.ID == "" {
		t.Fatalf("AddEndpoint = (%+v, %v), want generated ID", created, err)
	}
	if got := m.Endpoints(); len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("Endpoints() = %+v, want the created endpoint", got)
	}

	if _, err := m.AddEndpoint(Endpoint{ID: created.ID, URL: "http://host/y"}); err == nil {
		t.Error("duplicate ID should be rejected")
	}
	if _, err := m.AddEndpoint(Endpoint{ID: "bad", URL: "not-a-url"}); err == nil {
		t.Error("invalid URL should be rejected")
	}

	if err := m.UpdateEndpoint(created.ID, Endpoint{URL: "http://host/z", Enabled: false}); err != nil {
		t.Fatalf("UpdateEndpoint: %v", err)
	}
	got := m.Endpoints()[0]
	if got.URL != "http://host/z" || got.Enabled {
		t.Fatalf("updated endpoint = %+v, want disabled + new URL", got)
	}

	if err := m.DeleteEndpoint(created.ID); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if len(m.Endpoints()) != 0 {
		t.Fatalf("endpoints after delete = %+v, want empty", m.Endpoints())
	}

	if err := m.DeleteEndpoint("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteEndpoint(missing) = %v, want ErrNotFound", err)
	}
	if err := m.UpdateEndpoint("nope", Endpoint{URL: "http://host/x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateEndpoint(missing) = %v, want ErrNotFound", err)
	}
}
