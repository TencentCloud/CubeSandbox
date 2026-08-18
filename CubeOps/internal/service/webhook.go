// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// WebhookStore is the subset of *store.Store the webhook service depends on.
// Defined as an interface so tests can substitute a fake without a database.
type WebhookStore interface {
	CreateWebhookSubscription(ctx context.Context, sub *store.WebhookSubscription) error
	GetWebhookSubscription(ctx context.Context, id int64) (*store.WebhookSubscription, error)
	ListWebhookSubscriptions(ctx context.Context, limit, offset int) ([]store.WebhookSubscription, error)
	UpdateWebhookSubscription(ctx context.Context, sub *store.WebhookSubscription) error
	SoftDeleteWebhookSubscription(ctx context.Context, id int64) error
	CreateWebhookDelivery(ctx context.Context, d *store.WebhookDelivery) error
	ListWebhookDeliveries(ctx context.Context, subscriptionID int64, status, eventIDPrefix string, limit, offset int) ([]store.WebhookDelivery, error)
}

// WebhookEventTypes is the whitelist of deliverable sandbox lifecycle events.
var WebhookEventTypes = map[string]bool{
	"sandbox.created": true,
	"sandbox.deleted": true,
	"sandbox.paused":  true,
	"sandbox.resumed": true,
}

const (
	// testWindowDuration is the in-memory rate-limit window for the test
	// delivery endpoint (per subscription).
	testWindowDuration = time.Minute
	// testWindowMaxCalls is the per-subscription cap per window. The counter
	// is process-local: with N replicas the effective cap is 5×N (documented
	// anti-abuse limit, not an exact distributed limiter).
	testWindowMaxCalls = 5
)

// WebhookService implements webhook subscription CRUD and the test delivery
// endpoint. The actual delivery worker (consumer/sender) lives in
// internal/webhook and is started by main.go when webhook.enabled is set.
type WebhookService struct {
	store WebhookStore

	// testLimits is a per-subscription in-memory rate limit for POST /:id/test.
	// Guarded by testMu: concurrent requests must not race on window state.
	testLimits map[int64]*testWindow
	testMu     sync.Mutex
	// now is injectable so tests can advance the rate-limit window.
	now func() time.Time
}

type testWindow struct {
	count   int
	resetAt time.Time
}

// NewWebhookService creates a WebhookService over the given store.
func NewWebhookService(s WebhookStore) *WebhookService {
	return &WebhookService{
		store:      s,
		testLimits: map[int64]*testWindow{},
		now:        time.Now,
	}
}

// WebhookCreateRequest is the POST /api/v1/webhooks body.
type WebhookCreateRequest struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Secret  *string  `json:"secret,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
}

// WebhookUpdateRequest is the PUT /api/v1/webhooks/:id body. Omitted fields
// keep their current value; secret=="" explicitly clears the signature.
type WebhookUpdateRequest struct {
	Name    *string  `json:"name,omitempty"`
	URL     *string  `json:"url,omitempty"`
	Events  []string `json:"events,omitempty"`
	Secret  *string  `json:"secret,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
}

// Create validates and persists a new subscription. Secret (when provided)
// is encrypted in the same operation; the transaction rolls back if
// encryption fails, so no half-created subscription is left behind.
func (s *WebhookService) Create(ctx context.Context, req *WebhookCreateRequest) (*store.WebhookSubscription, *Error) {
	if err := validateWebhookInput(req.Name, req.URL, req.Events); err != nil {
		return nil, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	sub := &store.WebhookSubscription{
		Name:    strings.TrimSpace(req.Name),
		URL:     strings.TrimSpace(req.URL),
		Enabled: enabled,
		Events:  toWebhookEvents(req.Events),
	}
	if req.Secret != nil && *req.Secret != "" {
		enc, err := crypto.EncryptSecret(*req.Secret)
		if err != nil {
			return nil, NewInternal("encrypt secret: " + err.Error())
		}
		sub.SecretCiphertext = &enc
	}
	if err := s.store.CreateWebhookSubscription(ctx, sub); err != nil {
		return nil, webhookStoreError(err)
	}
	created, err := s.store.GetWebhookSubscription(ctx, sub.ID)
	if err != nil {
		return nil, webhookStoreError(err)
	}
	return created, nil
}

// Get returns a subscription, including soft-deleted rows so the response
// can distinguish "disabled" (enabled=false, deleted_at=null) from "deleted"
// (deleted_at set).
func (s *WebhookService) Get(ctx context.Context, id int64) (*store.WebhookSubscription, *Error) {
	sub, err := s.store.GetWebhookSubscription(ctx, id)
	if err != nil {
		return nil, webhookStoreError(err)
	}
	return sub, nil
}

// List returns a page of non-deleted subscriptions.
func (s *WebhookService) List(ctx context.Context, limit, offset int) ([]store.WebhookSubscription, *Error) {
	subs, err := s.store.ListWebhookSubscriptions(ctx, limit, offset)
	if err != nil {
		return nil, NewInternal(err.Error())
	}
	return subs, nil
}

// Update applies a partial update. Secret semantics: omitted → keep the
// existing ciphertext; non-empty → re-encrypt; explicit "" → clear the
// signature. Events, when provided, atomically replace the allowlist.
func (s *WebhookService) Update(ctx context.Context, id int64, req *WebhookUpdateRequest) (*store.WebhookSubscription, *Error) {
	existing, err := s.store.GetWebhookSubscription(ctx, id)
	if err != nil {
		return nil, webhookStoreError(err)
	}
	if existing.DeletedAt != nil {
		return nil, NewNotFound("subscription deleted")
	}

	name := existing.Name
	if req.Name != nil {
		name = *req.Name
	}
	endpoint := existing.URL
	if req.URL != nil {
		endpoint = *req.URL
	}
	events := webhookEventTypesFromRows(existing.Events)
	if req.Events != nil {
		events = req.Events
	}
	enabled := existing.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if err := validateWebhookInput(name, endpoint, events); err != nil {
		return nil, err
	}

	sub := &store.WebhookSubscription{
		ID:               id,
		Name:             strings.TrimSpace(name),
		URL:              strings.TrimSpace(endpoint),
		Enabled:          enabled,
		SecretCiphertext: existing.SecretCiphertext,
		Events:           toWebhookEvents(events),
	}
	if req.Secret != nil {
		switch *req.Secret {
		case "":
			sub.SecretCiphertext = nil
		default:
			enc, err := crypto.EncryptSecret(*req.Secret)
			if err != nil {
				return nil, NewInternal("encrypt secret: " + err.Error())
			}
			sub.SecretCiphertext = &enc
		}
	}
	if err := s.store.UpdateWebhookSubscription(ctx, sub); err != nil {
		return nil, webhookStoreError(err)
	}
	updated, err := s.store.GetWebhookSubscription(ctx, id)
	if err != nil {
		return nil, webhookStoreError(err)
	}
	return updated, nil
}

// Delete soft-deletes the subscription (enabled=false, deleted_at=now(),
// name renamed to release the unique constraint). Historical delivery rows
// stay queryable under the old subscription id.
func (s *WebhookService) Delete(ctx context.Context, id int64) *Error {
	if err := s.store.SoftDeleteWebhookSubscription(ctx, id); err != nil {
		return webhookStoreError(err)
	}
	return nil
}

// CreateTestDelivery inserts a synthetic sandbox.created delivery row and
// returns it. The row flows through the same claim/send pipeline as real
// events once the worker is enabled; the global worker switch is checked by
// the handler (webhook.enabled=false → 503 before reaching here).
func (s *WebhookService) CreateTestDelivery(ctx context.Context, id int64) (*store.WebhookDelivery, *Error) {
	sub, err := s.store.GetWebhookSubscription(ctx, id)
	if err != nil {
		return nil, webhookStoreError(err)
	}
	if sub.DeletedAt != nil {
		return nil, NewNotFound("subscription deleted")
	}
	if !sub.Enabled {
		return nil, NewConflict("subscription is disabled")
	}
	if !hasWebhookEvent(sub.Events, "sandbox.created") {
		return nil, NewBadRequest("subscription does not subscribe to sandbox.created; cannot send a test delivery")
	}
	if !s.allowTestCall(id) {
		return nil, NewError(429, "too many test deliveries for this subscription; retry later")
	}

	eventID := "test:" + uuid.NewString()
	now := time.Now()
	// Field set mirrors the real consumer payload (including occurred_at) so
	// receivers can validate the test delivery against the same schema.
	payload := fmt.Sprintf(
		`{"schema_version":"1","event":"sandbox.created","event_id":%q,"timestamp":%d,"occurred_at":%q,"sandbox_id":"test-sandbox","template_id":"test-template"}`,
		eventID, now.UnixMilli(), now.UTC().Format(time.RFC3339))
	d := &store.WebhookDelivery{
		EventID:        eventID,
		SubscriptionID: id,
		Payload:        payload,
		Status:         "pending",
	}
	if err := s.store.CreateWebhookDelivery(ctx, d); err != nil {
		return nil, webhookStoreError(err)
	}
	return d, nil
}

// ListDeliveries returns delivery rows for a subscription with optional
// status / event_id_prefix filters.
func (s *WebhookService) ListDeliveries(ctx context.Context, id int64, status, eventIDPrefix string, limit, offset int) ([]store.WebhookDelivery, *Error) {
	if _, err := s.store.GetWebhookSubscription(ctx, id); err != nil {
		return nil, webhookStoreError(err)
	}
	out, err := s.store.ListWebhookDeliveries(ctx, id, status, eventIDPrefix, limit, offset)
	if err != nil {
		return nil, NewInternal(err.Error())
	}
	return out, nil
}

// allowTestCall enforces the per-subscription in-memory rate limit. The
// counter is intentionally process-local (see testWindowMaxCalls doc).
func (s *WebhookService) allowTestCall(subscriptionID int64) bool {
	s.testMu.Lock()
	defer s.testMu.Unlock()
	now := s.now()
	w := s.testLimits[subscriptionID]
	if w == nil {
		w = &testWindow{resetAt: now.Add(testWindowDuration)}
		s.testLimits[subscriptionID] = w
	}
	if !now.Before(w.resetAt) {
		w.count = 0
		w.resetAt = now.Add(testWindowDuration)
	}
	w.count++
	return w.count <= testWindowMaxCalls
}

func hasWebhookEvent(rows []store.WebhookSubscriptionEvent, eventType string) bool {
	for _, r := range rows {
		if r.EventType == eventType {
			return true
		}
	}
	return false
}

func validateWebhookInput(name, endpoint string, events []string) *Error {
	if strings.TrimSpace(name) == "" {
		return NewBadRequest("name is required")
	}
	if len(name) > 128 {
		return NewBadRequest("name must be at most 128 characters")
	}
	if strings.TrimSpace(endpoint) == "" {
		return NewBadRequest("url is required")
	}
	if len(endpoint) > 2048 {
		return NewBadRequest("url must be at most 2048 characters")
	}
	if len(events) == 0 {
		return NewBadRequest("events must contain at least one event type")
	}
	for _, e := range events {
		if !WebhookEventTypes[e] {
			return NewBadRequest("unknown event type: " + e)
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return NewBadRequest("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return NewBadRequest("url scheme must be http or https")
	}
	if u.User != nil {
		return NewBadRequest("url must not contain userinfo")
	}
	if u.Host == "" {
		return NewBadRequest("url must include a host")
	}
	return nil
}

func toWebhookEvents(events []string) []store.WebhookSubscriptionEvent {
	out := make([]store.WebhookSubscriptionEvent, 0, len(events))
	for _, e := range events {
		out = append(out, store.WebhookSubscriptionEvent{EventType: e})
	}
	return out
}

func webhookEventTypesFromRows(rows []store.WebhookSubscriptionEvent) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	return out
}

func webhookStoreError(err error) *Error {
	if errors.Is(err, store.ErrWebhookSubscriptionNotFound) {
		return NewNotFound("webhook subscription not found")
	}
	return NewInternal(err.Error())
}
