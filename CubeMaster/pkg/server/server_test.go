// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// TestCADownloadRouteRegistration verifies that the CA download route is
// registered for both GET and HEAD. It inspects the gin route table directly
// rather than executing a request, so no middleware runs and no config/redis
// initialization is required.
func TestCADownloadRouteRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &internalHttp{engine: gin.New()}
	s.registerRoutes()

	routes := s.engine.Routes()
	gotGet := false
	gotHead := false
	for _, route := range routes {
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
