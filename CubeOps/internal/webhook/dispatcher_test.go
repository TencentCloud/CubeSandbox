// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"sync"
	"testing"
	"time"
)

type recordedDelivery struct {
	endpoint *Endpoint
	events   []Event
}

type recordingSubmitter struct {
	mu         sync.Mutex
	deliveries []recordedDelivery
	notify     chan struct{}
}

func (s *recordingSubmitter) Submit(_ context.Context, endpoint *Endpoint, events []Event) bool {
	s.mu.Lock()
	s.deliveries = append(s.deliveries, recordedDelivery{endpoint: endpoint, events: append([]Event(nil), events...)})
	s.mu.Unlock()
	if s.notify != nil {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
	return true
}

func (s *recordingSubmitter) snapshot() []recordedDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedDelivery(nil), s.deliveries...)
}

func testEvent(name string) Event {
	event := validBatch().Events[0]
	event["event"] = []byte(`"` + name + `"`)
	return event
}

func TestDispatcher_FiltersUnsubscribedEvents(t *testing.T) {
	endpoint := &Endpoint{ID: 0, Name: "audit", BatchSize: 1}
	cfg := &Config{
		Delivery:  defaultDeliveryConfig(),
		Endpoints: []*Endpoint{endpoint},
		Routes:    map[string][]*Endpoint{"sandbox.created": {endpoint}},
	}
	submitter := &recordingSubmitter{}
	dispatcher := NewDispatcher(NewIngress(10), cfg, submitter)

	dispatcher.dispatchBatch(context.Background(), InternalBatch{Events: []Event{testEvent("api.request")}})
	if got := len(submitter.snapshot()); got != 0 {
		t.Fatalf("deliveries = %d, want 0", got)
	}
	if got := dispatcher.Stats().Snapshot().FilteredEvents; got != 1 {
		t.Fatalf("filtered events = %d, want 1", got)
	}
	if got := dispatcher.Stats().Snapshot().Events["api.request"].Filtered; got != 1 {
		t.Fatalf("filtered api.request events = %d, want 1", got)
	}
}

func TestDispatcher_FansOutAndBatchesPerEndpoint(t *testing.T) {
	one := &Endpoint{ID: 0, Name: "one", BatchSize: 2}
	two := &Endpoint{ID: 1, Name: "two", BatchSize: 2}
	cfg := &Config{
		Delivery:  defaultDeliveryConfig(),
		Endpoints: []*Endpoint{one, two},
		Routes:    map[string][]*Endpoint{"sandbox.created": {one, two}},
	}
	submitter := &recordingSubmitter{}
	dispatcher := NewDispatcher(NewIngress(10), cfg, submitter)
	event := testEvent("sandbox.created")

	dispatcher.dispatchBatch(context.Background(), InternalBatch{Events: []Event{event, event}})
	deliveries := submitter.snapshot()
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %#v, want one per endpoint", deliveries)
	}
	for _, delivery := range deliveries {
		if len(delivery.events) != 2 {
			t.Fatalf("endpoint %s event count = %d, want 2", delivery.endpoint.Name, len(delivery.events))
		}
	}
	stats := dispatcher.Stats().Snapshot()
	if stats.MatchedEvents != 2 || stats.EndpointBatches != 2 {
		t.Fatalf("dispatcher stats = %#v", stats)
	}
	if got := stats.Events["sandbox.created"].Matched; got != 2 {
		t.Fatalf("matched sandbox.created events = %d, want 2", got)
	}
}

func TestDispatcher_FlushesPartialBatchOnInterval(t *testing.T) {
	endpoint := &Endpoint{ID: 0, Name: "audit", BatchSize: 10}
	cfg := &Config{
		Delivery:  defaultDeliveryConfig(),
		Endpoints: []*Endpoint{endpoint},
		Routes:    map[string][]*Endpoint{"sandbox.created": {endpoint}},
	}
	cfg.Delivery.FlushIntervalSecs = 1
	submitter := &recordingSubmitter{notify: make(chan struct{}, 1)}
	ingress := NewIngress(10)
	dispatcher := NewDispatcher(ingress, cfg, submitter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx)

	if !ingress.TryEnqueue(InternalBatch{Events: []Event{testEvent("sandbox.created")}}) {
		t.Fatal("enqueue failed")
	}
	select {
	case <-submitter.notify:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("partial batch was not flushed")
	}
	deliveries := submitter.snapshot()
	if len(deliveries) != 1 || len(deliveries[0].events) != 1 {
		t.Fatalf("deliveries = %#v, want one event", deliveries)
	}
}
