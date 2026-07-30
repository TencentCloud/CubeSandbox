// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

func TestTerminalRoutesUseSeparateAuthenticationBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Bind:           "127.0.0.1:0",
		CubeMasterAddr: "http://127.0.0.1:8081",
		JWTSecret:      "test-jwt-secret-32-bytes-long-enough",
		AccessTTL:      15 * time.Minute,
		RefreshTTL:     time.Hour,
		Terminal: config.TerminalConfig{
			Enabled:               true,
			GrantTTLSeconds:       60,
			HandshakeTimeoutSec:   10,
			PingIntervalSeconds:   20,
			PongTimeoutSeconds:    10,
			WriteDeadlineSeconds:  10,
			ReconnectGraceSeconds: 30,
			MaxFrameBytes:         64 << 10,
			MaxSessionsPerUser:    5,
			MaxSessionsPerReplica: 200,
			DrainTimeoutSeconds:   30,
			InternalToken:         "test-internal-token-32-bytes-long",
		},
	}
	server := New(cfg, nil)
	router := server.buildRouter()

	grantRequest := httptest.NewRequest(http.MethodPost, "/api/v1/terminal/grants", nil)
	grantRecorder := httptest.NewRecorder()
	router.ServeHTTP(grantRecorder, grantRequest)
	if grantRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("grant route without JWT status = %d, want 401", grantRecorder.Code)
	}

	wsRequest := httptest.NewRequest(http.MethodGet, "/api/v1/terminal/ws", nil)
	wsRecorder := httptest.NewRecorder()
	router.ServeHTTP(wsRecorder, wsRequest)
	if wsRecorder.Code != http.StatusBadRequest {
		t.Fatalf("WebSocket route without upgrade status = %d, want 400 instead of JWT 401", wsRecorder.Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown before Start: %v", err)
	}
}
