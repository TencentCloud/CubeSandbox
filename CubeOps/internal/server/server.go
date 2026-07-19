// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// Server is the CubeOps HTTP server.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	jm      *auth.JWTManager
	httpSrv *http.Server
	cm      *cubemaster.Client
}

// New creates a new CubeOps server.
func New(cfg *config.Config, s *store.Store) *Server {
	jm := auth.NewJWTManager(cfg.JWTSecret, cfg.AccessTTL, cfg.RefreshTTL)
	cm := cubemaster.New(cfg.CubeMasterAddr)
	return &Server{
		cfg:   cfg,
		store: s,
		jm:    jm,
		cm:    cm,
	}
}

// Start begins listening for HTTP requests.
func (s *Server) Start() error {
	engine := s.buildRouter()

	s.httpSrv = &http.Server{
		Addr:              s.cfg.Bind,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,  // mitigate Slowloris attacks
		WriteTimeout:      300 * time.Second, // match nginx proxy_read_timeout
		IdleTimeout:       120 * time.Second,
		// ReadTimeout is intentionally NOT set. Go's http.Server.ReadTimeout
		// covers the entire request body read AND cancels the request context
		// when it expires — which would abort long-running handlers like Agent
		// creation (applyOpenclawRuntime can take 25+ seconds). ReadHeaderTimeout
		// alone is sufficient for Slowloris mitigation.
	}

	slog.Info("CubeOps starting", "addr", s.cfg.Bind)
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	slog.Info("CubeOps shutting down")
	return s.httpSrv.Shutdown(ctx)
}

// buildRouter configures all routes on a gin engine.
func (s *Server) buildRouter() *gin.Engine {
	// We use gin.New() rather than gin.Default() so we can attach our own
	// slog-based access logger and recovery handler. gin.Default() writes to
	// stdout and bypasses any logger the operator has configured.
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())

	// Health check (no auth) — defined at the root rather than under /api/v1
	// because external load balancers and k8s probes hit it without a prefix.
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// Wire up service layer + handlers.
	authSvc := service.NewAuthService(s.store, s.jm)
	authH := auth.NewHandler(authSvc)
	clusterH := handler.NewClusterHandler(s.cm)
	storeH := handler.NewStoreHandler()
	configH := handler.NewConfigHandler(s.cfg.Bind, 100, s.cfg.JWTSecret != "", s.cfg.SandboxDomain, "cubebox")
	agenthubH := handler.NewAgentHubHandler(s.store, s.cm)
	// SDK handler gets the AgentHubService so that E2B template/snapshot
	// deletions can reverse-sync AgentHub registrations (matching the old
	// Rust reverse_sync_agenthub_template that lived in CubeAPI).
	sdkH := handler.NewSDKHandler(s.cm).WithAgentHubService(agenthubH.AgentHubService())

	// Public (no auth) routes — login + refresh.
	public := r.Group("/api/v1")
	authH.RegisterPublic(public)

	// Authenticated routes. The session / logout / change-password endpoints
	// are mounted here, behind the JWT middleware.
	authed := r.Group("/api/v1", auth.Middleware(s.jm))
	authH.RegisterAuthed(authed)

	clusterH.Register(authed)
	configH.Register(authed)
	storeH.Register(authed)
	agenthubH.Register(authed)

	// SDK routes — mounted at both /api/v1/sdk and /api/v1/sdk/v2 because
	// the WebUI and the E2B-compatible clients hit different prefixes.
	sdkGroup := authed.Group("/sdk")
	sdkH.Register(sdkGroup)
	sdkV2Group := authed.Group("/sdk/v2")
	sdkV2Group.GET("/sandboxes", sdkH.ListSandboxes)
	sdkV2Group.GET("/sandboxes/:id/logs", sdkH.GetSandboxLogs)

	return r
}

// requestLogger is a gin middleware that emits a structured slog line per
// request, matching the format the CubeOps CLI tooling expects.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"size", c.Writer.Size(),
			"latency_ms", time.Since(start).Milliseconds(),
			"client", c.ClientIP(),
		)
	}
}
