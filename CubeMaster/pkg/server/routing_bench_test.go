// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/mux"
)

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

// setupMuxRouter creates a gorilla/mux router with the same routes.
// Uses the OLD pattern: all /cube/* routes go through one HttpHandler
// (matching the pre-migration architecture).
func setupMuxRouter() *mux.Router {
	r := mux.NewRouter()
	noop := func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200}}`))
	}

	r.HandleFunc("/cube/sandbox", noop).Methods(http.MethodPost, http.MethodDelete)
	r.HandleFunc("/cube/sandbox/list", noop).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc("/cube/sandbox/info", noop).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc("/cube/sandbox/{sandbox_id}/rollback", noop).Methods(http.MethodPost)
	r.HandleFunc("/cube/snapshot", noop).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc("/cube/snapshot/storage", noop).Methods(http.MethodGet)
	r.HandleFunc("/cube/snapshot/{snapshot_id}", noop).Methods(http.MethodGet, http.MethodDelete)
	r.HandleFunc("/cube/operation/{operation_id}", noop).Methods(http.MethodGet)
	r.HandleFunc("/cube/template", noop).Methods(http.MethodGet, http.MethodPost, http.MethodDelete)
	r.HandleFunc("/cube/template/build/{build_id}/status", noop).Methods(http.MethodGet)
	r.HandleFunc("/cube/ca/{filename}", noop).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/cube/listinventory", noop).Methods(http.MethodPost)
	r.HandleFunc("/internal/node", noop).Methods(http.MethodGet)
	r.HandleFunc("/internal/query", noop)
	r.HandleFunc("/internal/meta/readyz", noop).Methods(http.MethodGet)
	r.HandleFunc("/internal/meta/nodes/register", noop).Methods(http.MethodPost)
	r.HandleFunc("/internal/meta/nodes/{node_id}", noop).Methods(http.MethodGet)
	r.HandleFunc("/internal/meta/nodes/{node_id}/status", noop).Methods(http.MethodPost)
	return r
}

// Bench scenario 1: POST /cube/sandbox (typical create request)
func BenchmarkMuxCreateRoute(b *testing.B) {
	r := setupMuxRouter()
	body := []byte(`{"template_id":"tpl-test"}`)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/cube/sandbox", bytes.NewReader(body))
		r.ServeHTTP(w, req)
	}
}

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
func BenchmarkMuxParamRoute(b *testing.B) {
	r := setupMuxRouter()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/snap-abc123", nil)
		r.ServeHTTP(w, req)
	}
}

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
func BenchmarkMuxStaticPriority(b *testing.B) {
	r := setupMuxRouter()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/storage", nil)
		r.ServeHTTP(w, req)
	}
}

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
func BenchmarkMuxNestedParam(b *testing.B) {
	r := setupMuxRouter()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/template/build/job-42/status", nil)
		r.ServeHTTP(w, req)
	}
}

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
