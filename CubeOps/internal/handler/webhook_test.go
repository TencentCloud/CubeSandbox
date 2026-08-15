// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
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

// fakeStore is the handler-test in-memory WebhookStore.
type fakeStore struct {
	subs       map[int64]*store.WebhookSubscription
	nextID     int64
	deliveries []store.WebhookDelivery
}

func newFakeStore() *fakeStore {
	return &fakeStore{subs: map[int64]*store.WebhookSubscription{}, nextID: 1}
}

func (f *fakeStore) CreateWebhookSubscription(_ context.Context, sub *store.WebhookSubscription) error {
	sub.ID = f.nextID
	f.nextID++
	now := time.Now()
	sub.CreatedAt, sub.UpdatedAt = now, now
	cp := *sub
	cp.Events = append([]store.WebhookSubscriptionEvent(nil), sub.Events...)
	for i := range cp.Events {
		cp.Events[i].SubscriptionID = cp.ID
	}
	f.subs[cp.ID] = &cp
	*sub = cp
	return nil
}

func (f *fakeStore) GetWebhookSubscription(_ context.Context, id int64) (*store.WebhookSubscription, error) {
	sub, ok := f.subs[id]
	if !ok {
		return nil, store.ErrWebhookSubscriptionNotFound
	}
	cp := *sub
	return &cp, nil
}

func (f *fakeStore) ListWebhookSubscriptions(_ context.Context, _, _ int) ([]store.WebhookSubscription, error) {
	var out []store.WebhookSubscription
	for _, s := range f.subs {
		if s.DeletedAt == nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateWebhookSubscription(_ context.Context, sub *store.WebhookSubscription) error {
	existing, ok := f.subs[sub.ID]
	if !ok || existing.DeletedAt != nil {
		return store.ErrWebhookSubscriptionNotFound
	}
	existing.Name, existing.URL, existing.Enabled = sub.Name, sub.URL, sub.Enabled
	existing.SecretCiphertext = sub.SecretCiphertext
	existing.Events = append([]store.WebhookSubscriptionEvent(nil), sub.Events...)
	return nil
}

func (f *fakeStore) SoftDeleteWebhookSubscription(_ context.Context, id int64) error {
	sub, ok := f.subs[id]
	if !ok || sub.DeletedAt != nil {
		return store.ErrWebhookSubscriptionNotFound
	}
	now := time.Now()
	sub.Enabled, sub.DeletedAt, sub.UpdatedAt = false, &now, now
	return nil
}

func (f *fakeStore) CreateWebhookDelivery(_ context.Context, d *store.WebhookDelivery) error {
	d.ID = int64(len(f.deliveries) + 1)
	f.deliveries = append(f.deliveries, *d)
	return nil
}

func (f *fakeStore) ListWebhookDeliveries(_ context.Context, subscriptionID int64, status, prefix string, _, _ int) ([]store.WebhookDelivery, error) {
	var out []store.WebhookDelivery
	for _, d := range f.deliveries {
		if d.SubscriptionID != subscriptionID {
			continue
		}
		if status != "" && d.Status != status {
			continue
		}
		if prefix != "" && !strings.HasPrefix(d.EventID, prefix) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func newWebhookRouter(enabled bool, st service.WebhookStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := handler.NewWebhookHandler(service.NewWebhookService(st), enabled)
	h.Register(r.Group("/api/v1"))
	return r
}

func doJSON(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func createViaAPI(t *testing.T, r *gin.Engine) int64 {
	t.Helper()
	w := doJSON(t, r, "POST", "/api/v1/webhooks",
		`{"name":"sys","url":"https://example.com/hook","events":["sandbox.created"],"secret":"s"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	var sub store.WebhookSubscription
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return sub.ID
}

func TestWebhookCRUD_Endpoints(t *testing.T) {
	r := newWebhookRouter(false, newFakeStore())

	// Create.
	id := createViaAPI(t, r)

	// Get (secret must not be returned).
	w := doJSON(t, r, "GET", "/api/v1/webhooks/"+itoa(id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatalf("secret leaked in response: %s", w.Body.String())
	}

	// List.
	w = doJSON(t, r, "GET", "/api/v1/webhooks", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sys"`) {
		t.Fatalf("list: status=%d body=%s", w.Code, w.Body.String())
	}

	// Update (rename + disable).
	w = doJSON(t, r, "PUT", "/api/v1/webhooks/"+itoa(id),
		`{"name":"sys2","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", w.Code, w.Body.String())
	}

	// Delete → 204; then GET still 200 with deleted_at; PUT → 404.
	w = doJSON(t, r, "DELETE", "/api/v1/webhooks/"+itoa(id), "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}
	w = doJSON(t, r, "GET", "/api/v1/webhooks/"+itoa(id), "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "deleted_at") {
		t.Fatalf("get after delete: status=%d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, "PUT", "/api/v1/webhooks/"+itoa(id), `{"name":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("put deleted: status=%d, want 404", w.Code)
	}

	// Unknown id → 404.
	w = doJSON(t, r, "GET", "/api/v1/webhooks/999999", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get unknown: status=%d, want 404", w.Code)
	}

	// Invalid create → 400.
	w = doJSON(t, r, "POST", "/api/v1/webhooks",
		`{"name":"","url":"ftp://x","events":["sandbox.nope"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid create: status=%d, want 400", w.Code)
	}
}

func TestWebhookTestEndpoint_DisabledReturns503(t *testing.T) {
	r := newWebhookRouter(false, newFakeStore())
	id := createViaAPI(t, r)
	w := doJSON(t, r, "POST", "/api/v1/webhooks/"+itoa(id)+"/test", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("test with worker disabled: status=%d, want 503", w.Code)
	}
}

func TestWebhookTestEndpoint_EnabledCreatesDelivery(t *testing.T) {
	r := newWebhookRouter(true, newFakeStore())
	id := createViaAPI(t, r)
	w := doJSON(t, r, "POST", "/api/v1/webhooks/"+itoa(id)+"/test", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("test with worker enabled: status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["delivery_id"]; !ok {
		t.Fatalf("response missing delivery_id: %s", w.Body.String())
	}

	// Deliveries query with the test: prefix.
	w = doJSON(t, r, "GET", "/api/v1/webhooks/"+itoa(id)+"/deliveries?status=pending&event_id_prefix=test:", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "test:") {
		t.Fatalf("deliveries: status=%d body=%s", w.Code, w.Body.String())
	}

	// Invalid filters → 400.
	w = doJSON(t, r, "GET", "/api/v1/webhooks/"+itoa(id)+"/deliveries?status=INVALID!", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter: status=%d, want 400", w.Code)
	}
	w = doJSON(t, r, "GET", "/api/v1/webhooks/"+itoa(id)+"/deliveries?event_id_prefix=bad%20chars", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad prefix filter: status=%d, want 400", w.Code)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
