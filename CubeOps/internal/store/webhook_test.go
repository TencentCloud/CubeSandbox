// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

func newWebhookSub(name, url string, events ...string) *store.WebhookSubscription {
	sub := &store.WebhookSubscription{Name: name, URL: url, Enabled: true}
	for _, e := range events {
		sub.Events = append(sub.Events, store.WebhookSubscriptionEvent{EventType: e})
	}
	return sub
}

// TestStore_WebhookSubscriptionCRUD exercises the subscription lifecycle
// against a real database: create → get → list → update → soft delete.
// Requires Docker (see dockertest_fixture_test.go); skipped otherwise.
func TestStore_WebhookSubscriptionCRUD(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	sub := newWebhookSub("sys-a", "https://example.com/hook", "sandbox.created", "sandbox.deleted")
	if err := s.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if sub.ID == 0 {
		t.Fatal("created subscription should have an id")
	}
	if len(sub.Events) != 2 || sub.Events[0].SubscriptionID != sub.ID {
		t.Fatalf("events not persisted with FK: %+v", sub.Events)
	}

	got, err := s.GetWebhookSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetWebhookSubscription: %v", err)
	}
	if got.Name != "sys-a" || !got.Enabled || len(got.Events) != 2 {
		t.Fatalf("get mismatch: %+v", got)
	}

	list, err := s.ListWebhookSubscriptions(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == sub.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("created subscription missing from list")
	}

	// Fan-out lookup by event type.
	byEvent, err := s.ListWebhookSubscriptionsByEventType(ctx, "sandbox.created")
	if err != nil {
		t.Fatalf("ListWebhookSubscriptionsByEventType: %v", err)
	}
	if len(byEvent) == 0 {
		t.Fatal("no subscription found for sandbox.created")
	}
	byEvent, err = s.ListWebhookSubscriptionsByEventType(ctx, "sandbox.resumed")
	if err != nil {
		t.Fatalf("ListWebhookSubscriptionsByEventType(resumed): %v", err)
	}
	for _, x := range byEvent {
		if x.ID == sub.ID {
			t.Fatal("subscription should not match an unsubscribed event type")
		}
	}

	// Update: rename + replace events.
	updated := &store.WebhookSubscription{
		ID: sub.ID, Name: "sys-a2", URL: "https://example.com/v2", Enabled: false,
		Events: []store.WebhookSubscriptionEvent{{EventType: "sandbox.paused"}},
	}
	if err := s.UpdateWebhookSubscription(ctx, updated); err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}
	got, err = s.GetWebhookSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetWebhookSubscription after update: %v", err)
	}
	if got.Name != "sys-a2" || got.Enabled || len(got.Events) != 1 || got.Events[0].EventType != "sandbox.paused" {
		t.Fatalf("update mismatch: %+v", got)
	}

	// Soft delete: name renamed, deleted_at set, list excludes it.
	if err := s.SoftDeleteWebhookSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("SoftDeleteWebhookSubscription: %v", err)
	}
	got, err = s.GetWebhookSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetWebhookSubscription after delete: %v", err)
	}
	if got.DeletedAt == nil {
		t.Fatal("deleted_at should be set")
	}
	if !strings.Contains(got.Name, "#del#") {
		t.Fatalf("deleted name should carry #del# marker, got %q", got.Name)
	}
	list, err = s.ListWebhookSubscriptions(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions after delete: %v", err)
	}
	for _, x := range list {
		if x.ID == sub.ID {
			t.Fatal("soft-deleted subscription must not appear in list")
		}
	}
}

// TestStore_WebhookDeliveries exercises the deliveries query endpoint path.
func TestStore_WebhookDeliveries(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	sub := newWebhookSub("sys-d", "https://example.com/hook", "sandbox.created")
	if err := s.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	for _, id := range []string{"test:a", "test:b", "real:1"} {
		d := &store.WebhookDelivery{
			EventID:        id,
			SubscriptionID: sub.ID,
			Payload:        `{"event":"sandbox.created"}`,
			Status:         "pending",
			NextRetryAt:    time.Now(),
		}
		if err := s.CreateWebhookDelivery(ctx, d); err != nil {
			t.Fatalf("CreateWebhookDelivery(%s): %v", id, err)
		}
	}

	all, err := s.ListWebhookDeliveries(ctx, sub.ID, "", "", 0, 0)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 deliveries, got %d", len(all))
	}
	testOnly, err := s.ListWebhookDeliveries(ctx, sub.ID, "", "test:", 0, 0)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries(prefix): %v", err)
	}
	if len(testOnly) != 2 {
		t.Fatalf("want 2 test deliveries, got %d", len(testOnly))
	}
	pending, err := s.ListWebhookDeliveries(ctx, sub.ID, "pending", "", 1, 0)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries(status): %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending row with limit=1, got %d", len(pending))
	}
}
