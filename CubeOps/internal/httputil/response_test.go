// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package httputil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func runRequest(t *testing.T, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.GET("/json", func(c *gin.Context) { WriteJSON(c, http.StatusCreated, map[string]string{"ok": "yes"}) })
	r.GET("/err", func(c *gin.Context) { WriteError(c, http.StatusBadRequest, "bad input") })
	r.GET("/no", func(c *gin.Context) { WriteNoContent(c) })

	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWriteJSON(t *testing.T) {
	w := runRequest(t, "GET", "/json", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ok"] != "yes" {
		t.Errorf("body.ok = %q, want %q", body["ok"], "yes")
	}
}

func TestWriteError(t *testing.T) {
	w := runRequest(t, "GET", "/err", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["error"] != "bad input" {
		t.Errorf("body.error = %q, want %q", body["error"], "bad input")
	}
}

func TestWriteNoContent(t *testing.T) {
	w := runRequest(t, "GET", "/no", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
}

func TestWriteRawJSON(t *testing.T) {
	r := gin.New()
	raw := json.RawMessage(`{"pre": "encoded"}`)
	r.GET("/raw", func(c *gin.Context) { WriteRawJSON(c, http.StatusOK, raw) })
	req := httptest.NewRequest("GET", "/raw", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != `{"pre": "encoded"}` {
		t.Errorf("body = %q, want raw JSON", w.Body.String())
	}
}
