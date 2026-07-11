// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// BenchmarkGinRouteMatch measures the overhead of Gin routing for a typical
// sandbox create request (POST /cube/sandbox). This isolates HTTP-layer cost:
// radix-tree lookup + handler invocation + response write.
func BenchmarkGinRouteMatch(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	engine.POST("/cube/sandbox", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ret": gin.H{"ret_code": 200, "ret_msg": "ok"}})
	})

	body := []byte(`{"template_id":"tpl-test","requestID":"bench-1"}`)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/cube/sandbox", bytes.NewReader(body))
		engine.ServeHTTP(w, req)
	}
}

// BenchmarkGinParamRoute measures path parameter extraction (GET /cube/snapshot/:id).
func BenchmarkGinParamRoute(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	engine.GET("/cube/snapshot/:snapshot_id", func(c *gin.Context) {
		id := c.Param("snapshot_id")
		c.JSON(http.StatusOK, gin.H{"id": id})
	})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/snap-abc123", nil)
		engine.ServeHTTP(w, req)
	}
}

// BenchmarkGinStaticPriority measures static-vs-param routing resolution
// (GET /cube/snapshot/storage should hit the static route, not :snapshot_id).
func BenchmarkGinStaticPriority(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	engine.GET("/cube/snapshot/storage", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"type": "storage"})
	})
	engine.GET("/cube/snapshot/:snapshot_id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.Param("snapshot_id")})
	})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/storage", nil)
		engine.ServeHTTP(w, req)
	}
}
