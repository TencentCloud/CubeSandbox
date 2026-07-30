// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func validBatch() InternalBatch {
	return InternalBatch{
		SchemaVersion: 1,
		Events: []Event{{
			"timestamp": []byte(`"2026-07-30T00:00:00Z"`),
			"level":     []byte(`"info"`),
			"event":     []byte(`"sandbox.created"`),
		}},
	}
}

func performBatchRequest(t *testing.T, engine http.Handler, batch InternalBatch, auth string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/webhook/events/batch", bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

func TestHandler_AcceptsCompleteBatchWithoutAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(10)
	engine := gin.New()
	NewHandler(q).Register(engine)

	response := performBatchRequest(t, engine, validBatch(), "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", response.Code, response.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, ok := q.Receive(ctx)
	if !ok || len(got.Events) != 1 {
		t.Fatalf("received batch = %#v, ok=%v", got, ok)
	}
}

func TestHandler_RejectsBadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(10)
	engine := gin.New()
	NewHandler(q).Register(engine)
	req := httptest.NewRequest(http.MethodPost, "/internal/webhook/events/batch", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestHandler_RejectsEmptyBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(10)
	engine := gin.New()
	NewHandler(q).Register(engine)
	response := performBatchRequest(t, engine, InternalBatch{SchemaVersion: 1}, "Bearer ignored")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestHandler_RejectsUnsupportedSchemaVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(10)
	engine := gin.New()
	NewHandler(q).Register(engine)
	batch := validBatch()
	batch.SchemaVersion = 2
	response := performBatchRequest(t, engine, batch, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestHandler_RejectsInvalidEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(10)
	engine := gin.New()
	NewHandler(q).Register(engine)
	batch := validBatch()
	batch.Events[0]["timestamp"] = []byte(`"not-a-timestamp"`)
	response := performBatchRequest(t, engine, batch, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestHandler_RejectsMoreThan100Events(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(200)
	engine := gin.New()
	NewHandler(q).Register(engine)
	batch := validBatch()
	batch.Events = make([]Event, 101)
	for index := range batch.Events {
		batch.Events[index] = validBatch().Events[0]
	}
	response := performBatchRequest(t, engine, batch, "")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
}

func TestHandler_Returns503WithoutPartialEnqueue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(1)
	engine := gin.New()
	NewHandler(q).Register(engine)
	if response := performBatchRequest(t, engine, validBatch(), ""); response.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", response.Code)
	}
	second := validBatch()
	second.Events = append(second.Events, validBatch().Events[0])
	if response := performBatchRequest(t, engine, second, ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want 503", response.Code)
	}
	if got := q.Stats().Snapshot().RejectedBatches; got != 1 {
		t.Fatalf("rejected batches = %d, want 1", got)
	}
}

func TestHandler_RecordsInvalidBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(10)
	engine := gin.New()
	NewHandler(q).Register(engine)
	req := httptest.NewRequest(http.MethodPost, "/internal/webhook/events/batch", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if got := q.Stats().Snapshot().InvalidBatches; got != 1 {
		t.Fatalf("invalid batches = %d, want 1", got)
	}
}

func TestHandler_DoesNotRequireOrReturnBatchID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	q := NewIngress(10)
	engine := gin.New()
	NewHandler(q).Register(engine)
	response := performBatchRequest(t, engine, validBatch(), "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, exists := payload["batch_id"]; exists {
		t.Fatalf("response contains batch_id: %s", response.Body.String())
	}
}
