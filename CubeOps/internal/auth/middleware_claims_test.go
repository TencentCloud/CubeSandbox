// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestMiddlewareInstallsVerifiedAccessClaims(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, time.Hour)
	token, err := manager.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}

	router := gin.New()
	router.GET("/claims", Middleware(manager), func(c *gin.Context) {
		claims, ok := AccessClaimsFromContext(c.Request.Context())
		if !ok {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, gin.H{"username": claims.Username, "role": claims.Role})
	})
	req := httptest.NewRequest(http.MethodGet, "/claims", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); got != "{\"role\":\"admin\",\"username\":\"admin\"}" {
		t.Errorf("body = %s, want verified username and role", got)
	}
}
