// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// newTestEngine builds the production engine (routes + NoRoute/NoMethod) without
// needing config/redis: the request middleware only reads config when it *runs*,
// and these tests only exercise bare NoRoute/NoMethod paths and the route table.
func newTestEngine(t *testing.T) *gin.Engine {
	t.Helper()
	s := &internalHttp{engine: newEngine()}
	s.registerRoutes()
	return s.engine
}

// TestCADownloadRouteRegistration verifies that the CA download route is
// registered for both GET and HEAD.
func TestCADownloadRouteRegistration(t *testing.T) {
	s := newTestEngine(t)
	gotGet, gotHead := false, false
	for _, route := range s.Routes() {
		if route.Path == "/cube/ca/:filename" {
			switch route.Method {
			case http.MethodGet:
				gotGet = true
			case http.MethodHead:
				gotHead = true
			}
		}
	}
	assert.True(t, gotGet, "GET /cube/ca/:filename route should be registered")
	assert.True(t, gotHead, "HEAD /cube/ca/:filename route should be registered")
}

// TestNotFoundReturnsBare404 — unmatched paths must return plain HTTP 404 like
// gorilla/mux (http.NotFoundHandler → "404 page not found"), NOT the business
// JSON-200 envelope.
func TestNotFoundReturnsBare404(t *testing.T) {
	engine := newTestEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/definitely/not/a/route", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "404 page not found")
}

// TestMethodNotAllowedReturnsBare405 — a registered path hit with an
// unregistered method must return HTTP 405 (empty body), matching mux, not the
// JSON-200 MasterParamsError envelope. /cube/sandbox is POST/DELETE only.
func TestMethodNotAllowedReturnsBare405(t *testing.T) {
	engine := newTestEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/cube/sandbox", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Empty(t, w.Body.String(), "405 must have an empty body")
}

// TestMethodNotAllowedSetsAllowHeader documents an intentional, beneficial
// difference from gorilla/mux: gin's 405 includes an RFC 7231 "Allow" header
// listing the methods supported at the path; mux's default 405 did not set it.
// We keep gin's behavior (standards-compliant, helps clients) — this test locks
// it in as a known/accepted parity delta. /cube/sandbox is registered POST+DELETE.
func TestMethodNotAllowedSetsAllowHeader(t *testing.T) {
	engine := newTestEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/cube/sandbox", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	allow := w.Header().Get("Allow")
	assert.NotEmpty(t, allow, "gin sets an Allow header on 405 (RFC 7231; mux did not — intentional)")
	assert.Contains(t, allow, http.MethodPost)
	assert.Contains(t, allow, http.MethodDelete)
}

// TestStaticPrioritySnapshotStorageRoute — the static /cube/snapshot/storage
// route is registered separately from /cube/snapshot/:snapshot_id so gin's
// radix tree resolves the static path (not the param).
func TestStaticPrioritySnapshotStorageRoute(t *testing.T) {
	engine := newTestEngine(t)
	hasStorage, hasParam := false, false
	for _, route := range engine.Routes() {
		if route.Method == http.MethodGet {
			switch route.Path {
			case "/cube/snapshot/storage":
				hasStorage = true
			case "/cube/snapshot/:snapshot_id":
				hasParam = true
			}
		}
	}
	assert.True(t, hasStorage, "static /cube/snapshot/storage must be registered")
	assert.True(t, hasParam, "param /cube/snapshot/:snapshot_id must be registered")
}

// TestQueryAndWsAreMethodAgnostic — /internal/query and /internal/ws must remain
// registered for any method (g.Any), matching the previous mux HandleFunc
// without .Methods(...).
func TestQueryAndWsAreMethodAgnostic(t *testing.T) {
	engine := newTestEngine(t)
	want := map[string]bool{"/internal/query": false, "/internal/ws": false}
	for _, route := range engine.Routes() {
		if _, ok := want[route.Path]; ok && route.Method == http.MethodGet {
			want[route.Path] = true
		}
	}
	for p, got := range want {
		assert.True(t, got, "%s should be reachable via GET (method-agnostic Any)", p)
	}
}

// TestMiddlewareSkippedOnNotFoundAndMethodMismatch proves the routing structure:
// the request middleware (which contains checkAuth) runs on matched business
// routes but is NOT invoked for unmatched-path (404) or method-mismatch (405)
// responses — preserving mux behavior where router middleware is skipped on
// MatchErr != nil. A sentinel middleware stands in for GinRequestMiddleware.
func TestMiddlewareSkippedOnNotFoundAndMethodMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.NoRoute(func(c *gin.Context) { http.NotFound(c.Writer, c.Request) })
	r.NoMethod(func(c *gin.Context) { c.AbortWithStatus(http.StatusMethodNotAllowed) })

	root := r.Group("")
	const mwHeader = "X-Mw-Ran"
	root.Use(func(c *gin.Context) { c.Header(mwHeader, "1"); c.Next() })
	root.POST("/cube/sandbox", func(c *gin.Context) { c.Status(http.StatusOK) })

	do := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}

	// Matched route: middleware runs.
	w := do(http.MethodPost, "/cube/sandbox")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "1", w.Header().Get(mwHeader), "middleware must run on matched routes")

	// 404: middleware must NOT run.
	w = do(http.MethodGet, "/no-such-path")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, w.Header().Get(mwHeader), "middleware must not run on unmatched paths (no auth on 404)")

	// 405: middleware must NOT run.
	w = do(http.MethodGet, "/cube/sandbox")
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Empty(t, w.Header().Get(mwHeader), "middleware must not run on method-mismatch (no auth on 405)")
}
