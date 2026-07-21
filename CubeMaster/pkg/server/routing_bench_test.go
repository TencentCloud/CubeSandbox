// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// routing_bench_test.go is a gin routing overhead regression benchmark.
//
// It builds a representative gin engine mirroring CubeMaster's real route
// table (static, param, static-priority and nested-param routes) and
// measures the per-request router lookup + response-write cost while
// excluding business logic. The benchmarks guard against accidental
// regressions in the HTTP routing layer after the migration to gin.

// setupGinEngine creates a gin engine with a representative set of routes
// mirroring CubeMaster's real route table (static, param, static-priority).
func setupGinEngine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	noop := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ret": gin.H{"ret_code": 200}}) }

	r.POST("/cube/sandbox", noop)
	r.DELETE("/cube/sandbox", noop)
	r.GET("/cube/sandbox/list", noop)
	r.POST("/cube/sandbox/list", noop)
	r.GET("/cube/sandbox/info", noop)
	r.POST("/cube/sandbox/info", noop)
	r.POST("/cube/sandbox/:sandbox_id/rollback", noop)
	r.GET("/cube/snapshot", noop)
	r.POST("/cube/snapshot", noop)
	r.GET("/cube/snapshot/storage", noop)
	r.GET("/cube/snapshot/:snapshot_id", noop)
	r.DELETE("/cube/snapshot/:snapshot_id", noop)
	r.GET("/cube/operation/:operation_id", noop)
	r.GET("/cube/template", noop)
	r.POST("/cube/template", noop)
	r.DELETE("/cube/template", noop)
	r.GET("/cube/template/build/:build_id/status", noop)
	r.GET("/cube/ca/:filename", noop)
	r.HEAD("/cube/ca/:filename", noop)
	r.POST("/cube/listinventory", noop)
	r.GET("/internal/node", noop)
	r.GET("/internal/query", noop)
	r.GET("/internal/meta/readyz", noop)
	r.POST("/internal/meta/nodes/register", noop)
	r.GET("/internal/meta/nodes/:node_id", noop)
	r.POST("/internal/meta/nodes/:node_id/status", noop)
	return r
}

// Bench scenario 1: POST /cube/sandbox (typical create request)
func BenchmarkGinCreateRoute(b *testing.B) {
	r := setupGinEngine()
	body := []byte(`{"template_id":"tpl-test"}`)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/cube/sandbox", bytes.NewReader(body))
		r.ServeHTTP(w, req)
	}
}

// Bench scenario 2: GET /cube/snapshot/:id (path param extraction)
func BenchmarkGinParamRoute(b *testing.B) {
	r := setupGinEngine()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/snap-abc123", nil)
		r.ServeHTTP(w, req)
	}
}

// Bench scenario 3: GET /cube/snapshot/storage (static-priority over param)
func BenchmarkGinStaticPriority(b *testing.B) {
	r := setupGinEngine()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/storage", nil)
		r.ServeHTTP(w, req)
	}
}

// Bench scenario 4: GET /cube/template/build/:build_id/status (nested param)
func BenchmarkGinNestedParam(b *testing.B) {
	r := setupGinEngine()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/template/build/job-42/status", nil)
		r.ServeHTTP(w, req)
	}
}
