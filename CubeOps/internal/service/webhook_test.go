// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

func TestMain(m *testing.M) {
	key, err := crypto.GenerateMasterKeyB64()
	if err != nil {
		panic(err)
	}
	if err := crypto.InstallMasterKey(key); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// fakeWebhookStore is an in-memory WebhookStore for service unit tests.
type fakeWebhookStore struct {
	subs       map[int64]*store.WebhookSubscription
	nextID     int64
	deliveries []store.WebhookDelivery
}

func newFakeWebhookStore() *fakeWebhookStore {
	return &fakeWebhookStore{subs: map[int64]*store.WebhookSubscription{}, nextID: 1}
}

func (f *fakeWebhookStore) CreateWebhookSubscription(_ context.Context, sub *store.WebhookSubscription) error {
	sub.ID = f.nextID
	f.nextID++
	now := time.Now()
	sub.CreatedAt = now
	sub.UpdatedAt = now
	cp := *sub
	cp.Events = append([]store.WebhookSubscriptionEvent(nil), sub.Events...)
	for i := range cp.Events {
		cp.Events[i].SubscriptionID = cp.ID
	}
	f.subs[cp.ID] = &cp
	*sub = cp
	return nil
}

func (f *fakeWebhookStore) GetWebhookSubscription(_ context.Context, id int64) (*store.WebhookSubscription, error) {
	sub, ok := f.subs[id]
	if !ok {
		return nil, store.ErrWebhookSubscriptionNotFound
	}
	cp := *sub
	cp.Events = append([]store.WebhookSubscriptionEvent(nil), sub.Events...)
	return &cp, nil
}

func (f *fakeWebhookStore) ListWebhookSubscriptions(_ context.Context, limit, offset int) ([]store.WebhookSubscription, error) {
	var out []store.WebhookSubscription
	for _, s := range f.subs {
		if s.DeletedAt != nil {
			continue
		}
		out = append(out, *s)
	}
	if limit <= 0 {
		limit = len(out)
	}
	if offset > len(out) {
		offset = len(out)
	}
	if offset+limit > len(out) {
		limit = len(out) - offset
	}
	return out[offset : offset+limit], nil
}

func (f *fakeWebhookStore) UpdateWebhookSubscription(_ context.Context, sub *store.WebhookSubscription) error {
	existing, ok := f.subs[sub.ID]
	if !ok || existing.DeletedAt != nil {
		return store.ErrWebhookSubscriptionNotFound
	}
	existing.Name = sub.Name
	existing.URL = sub.URL
	existing.Enabled = sub.Enabled
	existing.SecretCiphertext = sub.SecretCiphertext
	existing.Events = append([]store.WebhookSubscriptionEvent(nil), sub.Events...)
	for i := range existing.Events {
		existing.Events[i].SubscriptionID = sub.ID
	}
	existing.UpdatedAt = time.Now()
	return nil
}

func (f *fakeWebhookStore) SoftDeleteWebhookSubscription(_ context.Context, id int64) error {
	sub, ok := f.subs[id]
	if !ok || sub.DeletedAt != nil {
		return store.ErrWebhookSubscriptionNotFound
	}
	now := time.Now()
	sub.Enabled = false
	sub.DeletedAt = &now
	sub.Name = sub.Name + "#del#" + time.Now().Format("150405")
	sub.UpdatedAt = now
	return nil
}

func (f *fakeWebhookStore) CreateWebhookDelivery(_ context.Context, d *store.WebhookDelivery) error {
	d.ID = int64(len(f.deliveries) + 1)
	f.deliveries = append(f.deliveries, *d)
	return nil
}

func (f *fakeWebhookStore) ListWebhookDeliveries(_ context.Context, subscriptionID int64, status, eventIDPrefix string, limit, offset int) ([]store.WebhookDelivery, error) {
	var out []store.WebhookDelivery
	for _, d := range f.deliveries {
		if d.SubscriptionID != subscriptionID {
			continue
		}
		if status != "" && d.Status != status {
			continue
		}
		if eventIDPrefix != "" && !strings.HasPrefix(d.EventID, eventIDPrefix) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func mustCreate(t *testing.T, svc *service.WebhookService, name, endpoint string, events []string) *store.WebhookSubscription {
	t.Helper()
	sub, svcErr := svc.Create(context.Background(), &service.WebhookCreateRequest{
		Name: name, URL: endpoint, Events: events,
	})
	if svcErr != nil {
		t.Fatalf("Create: %v", svcErr)
	}
	return sub
}

func TestWebhookCreate_ValidatesInput(t *testing.T) {
	svc := service.NewWebhookService(newFakeWebhookStore())
	cases := []struct {
		label string
		name  string
		url   string
		evts  []string
	}{
		{"missing name", "", "https://example.com/hook", []string{"sandbox.created"}},
		{"bad scheme", "sys", "ftp://example.com/hook", []string{"sandbox.created"}},
		{"userinfo", "sys", "https://user:pass@example.com/hook", []string{"sandbox.created"}},
		{"unknown event", "sys", "https://example.com/hook", []string{"sandbox.unknown"}},
		{"empty events", "sys", "https://example.com/hook", nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			_, svcErr := svc.Create(context.Background(), &service.WebhookCreateRequest{
				Name: tc.name, URL: tc.url, Events: tc.evts,
			})
			if svcErr == nil || svcErr.Status != 400 {
				t.Fatalf("want 400, got %+v", svcErr)
			}
		})
	}
}

func TestWebhookCreate_DefaultsEnabledAndEncryptsSecret(t *testing.T) {
	svc := service.NewWebhookService(newFakeWebhookStore())
	secret := "s3cr3t"
	sub, svcErr := svc.Create(context.Background(), &service.WebhookCreateRequest{
		Name: "sys", URL: "https://example.com/hook",
		Events: []string{"sandbox.created", "sandbox.deleted"},
		Secret: &secret,
	})
	if svcErr != nil {
		t.Fatalf("Create: %v", svcErr)
	}
	if !sub.Enabled {
		t.Fatal("enabled should default to true")
	}
	if sub.SecretCiphertext == nil {
		t.Fatal("secret should be encrypted")
	}
	if *sub.SecretCiphertext == secret || !strings.HasPrefix(*sub.SecretCiphertext, "enc:v1:") {
		t.Fatalf("secret not encrypted properly: %q", *sub.SecretCiphertext)
	}
	if len(sub.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(sub.Events))
	}
}

func TestWebhookUpdate_SecretSemantics(t *testing.T) {
	svc := service.NewWebhookService(newFakeWebhookStore())
	initial := "first"
	sub, svcErr := svc.Create(context.Background(), &service.WebhookCreateRequest{
		Name: "sys", URL: "https://example.com/hook",
		Events: []string{"sandbox.created"},
		Secret: &initial,
	})
	if svcErr != nil {
		t.Fatalf("Create: %v", svcErr)
	}
	created := *sub.SecretCiphertext

	// Omitted secret keeps the existing ciphertext.
	updated, svcErr := svc.Update(context.Background(), sub.ID, &service.WebhookUpdateRequest{})
	if svcErr != nil {
		t.Fatalf("Update(keep): %v", svcErr)
	}
	if updated.SecretCiphertext == nil || *updated.SecretCiphertext != created {
		t.Fatal("omitted secret must keep the existing ciphertext")
	}

	// Explicit empty string clears the signature.
	empty := ""
	updated, svcErr = svc.Update(context.Background(), sub.ID, &service.WebhookUpdateRequest{Secret: &empty})
	if svcErr != nil {
		t.Fatalf("Update(clear): %v", svcErr)
	}
	if updated.SecretCiphertext != nil {
		t.Fatal("explicit empty secret must clear the signature")
	}

	// Non-empty value re-encrypts.
	second := "second"
	updated, svcErr = svc.Update(context.Background(), sub.ID, &service.WebhookUpdateRequest{Secret: &second})
	if svcErr != nil {
		t.Fatalf("Update(replace): %v", svcErr)
	}
	if updated.SecretCiphertext == nil || *updated.SecretCiphertext == second ||
		!strings.HasPrefix(*updated.SecretCiphertext, "enc:v1:") {
		t.Fatal("new secret must be re-encrypted")
	}
}

func TestWebhookDelete_ThenUpdateNotFound(t *testing.T) {
	svc := service.NewWebhookService(newFakeWebhookStore())
	sub := mustCreate(t, svc, "sys", "https://example.com/hook", []string{"sandbox.created"})

	if svcErr := svc.Delete(context.Background(), sub.ID); svcErr != nil {
		t.Fatalf("Delete: %v", svcErr)
	}
	// Deleted rows are still readable (deleted_at set).
	got, svcErr := svc.Get(context.Background(), sub.ID)
	if svcErr != nil {
		t.Fatalf("Get after delete: %v", svcErr)
	}
	if got.DeletedAt == nil {
		t.Fatal("deleted_at should be set after soft delete")
	}
	// But updates on a deleted subscription are 404.
	_, svcErr = svc.Update(context.Background(), sub.ID, &service.WebhookUpdateRequest{})
	if svcErr == nil || svcErr.Status != 404 {
		t.Fatalf("Update on deleted subscription: want 404, got %+v", svcErr)
	}
}

func TestWebhookTestDelivery_RateLimitAndState(t *testing.T) {
	svc := service.NewWebhookService(newFakeWebhookStore())
	sub := mustCreate(t, svc, "sys", "https://example.com/hook", []string{"sandbox.created"})

	for i := 0; i < 5; i++ {
		if _, svcErr := svc.CreateTestDelivery(context.Background(), sub.ID); svcErr != nil {
			t.Fatalf("test delivery %d: %v", i+1, svcErr)
		}
	}
	if _, svcErr := svc.CreateTestDelivery(context.Background(), sub.ID); svcErr == nil || svcErr.Status != 429 {
		t.Fatalf("6th test delivery: want 429, got %+v", svcErr)
	}

	// Disabled subscription → 409.
	f := false
	svc.Update(context.Background(), sub.ID, &service.WebhookUpdateRequest{Enabled: &f})
	if _, svcErr := svc.CreateTestDelivery(context.Background(), sub.ID); svcErr == nil || svcErr.Status != 409 {
		t.Fatalf("disabled test delivery: want 409, got %+v", svcErr)
	}

	// Deleted subscription → 404.
	deleted := mustCreate(t, svc, "gone", "https://example.com/hook", []string{"sandbox.created"})
	if svcErr := svc.Delete(context.Background(), deleted.ID); svcErr != nil {
		t.Fatalf("Delete: %v", svcErr)
	}
	if _, svcErr := svc.CreateTestDelivery(context.Background(), deleted.ID); svcErr == nil || svcErr.Status != 404 {
		t.Fatalf("deleted test delivery: want 404, got %+v", svcErr)
	}
}

func TestWebhookListDeliveries_Filters(t *testing.T) {
	svc := service.NewWebhookService(newFakeWebhookStore())
	sub := mustCreate(t, svc, "sys", "https://example.com/hook", []string{"sandbox.created"})
	for i := 0; i < 3; i++ {
		if _, svcErr := svc.CreateTestDelivery(context.Background(), sub.ID); svcErr != nil {
			t.Fatalf("test delivery %d: %v", i+1, svcErr)
		}
	}
	rows, svcErr := svc.ListDeliveries(context.Background(), sub.ID, "pending", "test:", 0, 0)
	if svcErr != nil {
		t.Fatalf("ListDeliveries: %v", svcErr)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 deliveries, got %d", len(rows))
	}
	for _, d := range rows {
		if !strings.HasPrefix(d.EventID, "test:") {
			t.Fatalf("event_id %q should start with test:", d.EventID)
		}
	}

	// Unknown subscription → 404.
	if _, svcErr := svc.ListDeliveries(context.Background(), 9999, "", "", 0, 0); svcErr == nil || svcErr.Status != 404 {
		t.Fatalf("ListDeliveries unknown sub: want 404, got %+v", svcErr)
	}
}

func TestWebhookStoreErrorMapping(t *testing.T) {
	svc := service.NewWebhookService(newFakeWebhookStore())
	if _, svcErr := svc.Get(context.Background(), 404); svcErr == nil || svcErr.Status != 404 {
		t.Fatalf("Get unknown: want 404, got %+v", svcErr)
	}
	if svcErr := svc.Delete(context.Background(), 404); svcErr == nil || svcErr.Status != 404 {
		t.Fatalf("Delete unknown: want 404, got %+v", svcErr)
	}
}
