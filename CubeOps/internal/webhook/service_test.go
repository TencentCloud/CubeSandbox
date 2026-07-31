// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	once        sync.Once
	event       LifecycleEvent
	acked       chan string
	claimPages  []claimPage
	claimStarts []string
}

type claimPage struct {
	events    []LifecycleEvent
	nextStart string
}

func (f *fakeSource) EnsureGroup(context.Context, string) error { return nil }
func (f *fakeSource) Read(ctx context.Context, _, _ string, _ time.Duration, _ int64) ([]LifecycleEvent, error) {
	var events []LifecycleEvent
	f.once.Do(func() { events = []LifecycleEvent{f.event} })
	if events != nil {
		return events, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f *fakeSource) Claim(
	_ context.Context,
	_, _ string,
	_ time.Duration,
	start string,
	_ int64,
) ([]LifecycleEvent, string, error) {
	f.claimStarts = append(f.claimStarts, start)
	if len(f.claimPages) == 0 {
		return nil, "0-0", nil
	}
	page := f.claimPages[0]
	f.claimPages = f.claimPages[1:]
	return page.events, page.nextStart, nil
}
func (f *fakeSource) Ack(_ context.Context, _, streamID string) error {
	f.acked <- streamID
	return nil
}
func (f *fakeSource) Close() error { return nil }

type failingDispatcher struct {
	events chan LifecycleEvent
}

func (d failingDispatcher) Deliver(_ context.Context, event LifecycleEvent) error {
	d.events <- event
	return errors.New("retry budget exhausted")
}

type cancellationDispatcher struct {
	started chan struct{}
}

func (d cancellationDispatcher) Deliver(ctx context.Context, _ LifecycleEvent) error {
	close(d.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestServiceAcknowledgesAfterDeliveryBudgetIsExhausted(t *testing.T) {
	source := &fakeSource{
		event: LifecycleEvent{
			StreamID: "1-0", EventID: "event-1", Op: "delete", SandboxID: "sandbox-1",
		},
		acked: make(chan string, 1),
	}
	delivered := make(chan LifecycleEvent, 1)
	service := &Service{
		source:      source,
		dispatcher:  failingDispatcher{events: delivered},
		group:       "cubeops-webhook",
		consumer:    "consumer-1",
		readBlock:   time.Millisecond,
		pendingIdle: time.Hour,
		workers:     1,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	select {
	case event := <-delivered:
		if event.EventID != "event-1" {
			t.Fatalf("event_id = %q", event.EventID)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not dispatched")
	}
	select {
	case streamID := <-source.acked:
		if streamID != "1-0" {
			t.Fatalf("acked stream ID = %q", streamID)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not acknowledged")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop")
	}
}

func TestWorkerIndexPreservesPerSandboxOrdering(t *testing.T) {
	create := workerIndex(LifecycleEvent{SandboxID: "sandbox-1", StreamID: "1-0"}, 8)
	deleted := workerIndex(LifecycleEvent{SandboxID: "sandbox-1", StreamID: "9-0"}, 8)
	if create != deleted {
		t.Fatalf("same sandbox routed to workers %d and %d", create, deleted)
	}
}

func TestServiceReclaimsAllPendingPagesFromOldestToNewest(t *testing.T) {
	source := &fakeSource{claimPages: []claimPage{
		{events: []LifecycleEvent{{StreamID: "1-0", SandboxID: "sandbox-1"}}, nextStart: "2-0"},
		{events: []LifecycleEvent{{StreamID: "2-0", SandboxID: "sandbox-1"}}, nextStart: "3-0"},
		{events: []LifecycleEvent{{StreamID: "3-0", SandboxID: "sandbox-1"}}, nextStart: "0-0"},
	}}
	service := &Service{
		source:      source,
		group:       "cubeops-webhook",
		consumer:    "consumer-1",
		pendingIdle: time.Minute,
	}
	jobs := []chan LifecycleEvent{make(chan LifecycleEvent, 3)}

	if !service.reclaimPending(t.Context(), jobs) {
		t.Fatal("reclaimPending stopped unexpectedly")
	}

	if got, want := source.claimStarts, []string{"0-0", "2-0", "3-0"}; !slices.Equal(got, want) {
		t.Fatalf("claim starts = %v, want %v", got, want)
	}
	for _, want := range []string{"1-0", "2-0", "3-0"} {
		if got := (<-jobs[0]).StreamID; got != want {
			t.Fatalf("reclaimed stream ID = %q, want %q", got, want)
		}
	}
}

func TestServiceDoesNotAcknowledgeDeliveryCanceledDuringShutdown(t *testing.T) {
	source := &fakeSource{
		event: LifecycleEvent{
			StreamID: "1-0", EventID: "event-1", Op: "delete", SandboxID: "sandbox-1",
		},
		acked: make(chan string, 1),
	}
	started := make(chan struct{})
	service := &Service{
		source:      source,
		dispatcher:  cancellationDispatcher{started: started},
		group:       "cubeops-webhook",
		consumer:    "consumer-1",
		readBlock:   time.Millisecond,
		pendingIdle: time.Hour,
		workers:     1,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("event delivery did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop")
	}
	select {
	case streamID := <-source.acked:
		t.Fatalf("canceled event %q was acknowledged", streamID)
	default:
	}
}
