// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
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
	router := s.buildRouter()

	s.httpSrv = &http.Server{
		Addr:         s.cfg.Bind,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 300 * time.Second, // match nginx proxy_read_timeout
		IdleTimeout:  120 * time.Second,
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

// buildRouter configures all routes.
func (s *Server) buildRouter() *mux.Router {
	r := mux.NewRouter()

	// Health check (no auth)
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}).Methods(http.MethodGet)

	// Handlers
	authHandler := auth.NewHandler(s.store, s.jm)
	clusterHandler := handler.NewClusterHandler(s.cm)
	storeHandler := handler.NewStoreHandler()
	configHandler := handler.NewConfigHandler(s.cfg.Bind, 100, s.cfg.JWTSecret != "", s.cfg.SandboxDomain, "cubebox")
	agenthubHandler := handler.NewAgentHubHandler(s.store, s.cm)
	sdkHandler := handler.NewSDKHandler(s.cm)

	// Public routes (no auth required)
	public := r.PathPrefix("/api/v1").Subrouter()
	public.HandleFunc("/auth/login", authHandler.Login).Methods(http.MethodPost)
	public.HandleFunc("/auth/refresh", authHandler.Refresh).Methods(http.MethodPost)

	// Authenticated routes
	authed := r.PathPrefix("/api/v1").Subrouter()
	authed.Use(auth.Middleware(s.jm))

	// Auth
	authed.HandleFunc("/auth/session", authHandler.Session).Methods(http.MethodGet)
	authed.HandleFunc("/auth/logout", authHandler.Logout).Methods(http.MethodPost)
	authed.HandleFunc("/auth/change-password", authHandler.ChangePassword).Methods(http.MethodPost)

	// Cluster
	authed.HandleFunc("/cluster/overview", clusterHandler.Overview).Methods(http.MethodGet)
	authed.HandleFunc("/cluster/versions", clusterHandler.Versions).Methods(http.MethodGet)
	authed.HandleFunc("/nodes", clusterHandler.ListNodes).Methods(http.MethodGet)
	authed.HandleFunc("/nodes/{nodeID}", clusterHandler.GetNode).Methods(http.MethodGet)

	// Config
	authed.HandleFunc("/config", configHandler.GetConfig).Methods(http.MethodGet)

	// Store
	authed.HandleFunc("/store/meta", storeHandler.GetStoreMeta).Methods(http.MethodGet)
	authed.HandleFunc("/store/refresh", storeHandler.RefreshStoreMeta).Methods(http.MethodPost)

	// AgentHub — instances
	authed.HandleFunc("/agenthub/instances", agenthubHandler.ListInstances).Methods(http.MethodGet)
	authed.HandleFunc("/agenthub/instances", agenthubHandler.CreateInstance).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}", agenthubHandler.DeleteInstance).Methods(http.MethodDelete)
	authed.HandleFunc("/agenthub/instances/{agentID}/restart", agenthubHandler.RestartAgent).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/operations", agenthubHandler.ListOperations).Methods(http.MethodGet)
	authed.HandleFunc("/agenthub/instances/{agentID}/gateway/health", agenthubHandler.GatewayHealth).Methods(http.MethodGet)
	authed.HandleFunc("/agenthub/instances/{agentID}/pause", agenthubHandler.PauseAgent).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/resume", agenthubHandler.ResumeAgent).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/upgrade", agenthubHandler.UpgradeAgent).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/model", agenthubHandler.UpdateModel).Methods(http.MethodPut)
	authed.HandleFunc("/agenthub/instances/{agentID}/wecom", agenthubHandler.GetWecomConfig).Methods(http.MethodGet)
	authed.HandleFunc("/agenthub/instances/{agentID}/wecom", agenthubHandler.UpdateWecomConfig).Methods(http.MethodPut)

	// AgentHub — snapshots
	authed.HandleFunc("/agenthub/instances/{agentID}/snapshots", agenthubHandler.ListSnapshots).Methods(http.MethodGet)
	authed.HandleFunc("/agenthub/instances/{agentID}/snapshots", agenthubHandler.CreateSnapshot).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/snapshots/{snapshotID}", agenthubHandler.DeleteSnapshot).Methods(http.MethodDelete)
	authed.HandleFunc("/agenthub/instances/{agentID}/snapshots/{snapshotID}", agenthubHandler.UpdateSnapshot).Methods(http.MethodPatch)
	authed.HandleFunc("/agenthub/instances/{agentID}/rollback", agenthubHandler.RollbackAgent).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/recover", agenthubHandler.RecoverAgent).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/clone", agenthubHandler.CloneAgent).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/instances/{agentID}/publish-template", agenthubHandler.PublishTemplate).Methods(http.MethodPost)

	// AgentHub — templates
	authed.HandleFunc("/agenthub/templates", agenthubHandler.ListTemplates).Methods(http.MethodGet)
	authed.HandleFunc("/agenthub/templates/market", agenthubHandler.RegisterMarketTemplate).Methods(http.MethodPost)
	authed.HandleFunc("/agenthub/templates/{templateID}", agenthubHandler.UpdateTemplate).Methods(http.MethodPatch)
	authed.HandleFunc("/agenthub/templates/{templateID}", agenthubHandler.DeleteTemplate).Methods(http.MethodDelete)

	// AgentHub — settings
	authed.HandleFunc("/agenthub/settings", agenthubHandler.GetSettings).Methods(http.MethodGet)
	authed.HandleFunc("/agenthub/settings", agenthubHandler.UpdateSettings).Methods(http.MethodPut)

	// SDK — WebUI sandbox/snapshot/template operations via CubeMaster direct.
	// These replace the former CubeAPI reverse proxy; CubeOps now calls
	// CubeMaster HTTP REST API directly for all SDK data needs.
	authed.HandleFunc("/sdk/sandboxes", sdkHandler.ListSandboxes).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/sandboxes", sdkHandler.CreateSandbox).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/sandboxes/{id}", sdkHandler.GetSandbox).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/sandboxes/{id}", sdkHandler.DeleteSandbox).Methods(http.MethodDelete)
	authed.HandleFunc("/sdk/sandboxes/{id}/logs", sdkHandler.GetSandboxLogs).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/sandboxes/{id}/timeout", sdkHandler.SetSandboxTimeout).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/sandboxes/{id}/refreshes", sdkHandler.RefreshSandbox).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/sandboxes/{id}/pause", sdkHandler.PauseSandbox).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/sandboxes/{id}/resume", sdkHandler.ResumeSandbox).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/sandboxes/{id}/connect", sdkHandler.ConnectSandbox).Methods(http.MethodPost)

	// SDK — v2 sandboxes (E2B v2 compatible, same handlers)
	authed.HandleFunc("/sdk/v2/sandboxes", sdkHandler.ListSandboxes).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/v2/sandboxes/{id}/logs", sdkHandler.GetSandboxLogs).Methods(http.MethodGet)

	// SDK — snapshots
	authed.HandleFunc("/sdk/snapshots", sdkHandler.ListSnapshots).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/sandboxes/{id}/snapshots", sdkHandler.CreateSnapshot).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/sandboxes/{id}/rollback", sdkHandler.RollbackSandbox).Methods(http.MethodPost)

	// SDK — templates
	// NOTE: literal paths (compat) must be registered BEFORE {id} routes,
	// otherwise gorilla/mux matches "compat" as {id}="compat".
	authed.HandleFunc("/sdk/templates", sdkHandler.ListTemplates).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/templates", sdkHandler.CreateTemplate).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/templates/compat", sdkHandler.GetTemplateCompat).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/templates/{id}", sdkHandler.GetTemplate).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/templates/{id}", sdkHandler.RebuildTemplate).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/templates/{id}", sdkHandler.DeleteTemplate).Methods(http.MethodDelete)
	authed.HandleFunc("/sdk/templates/{id}/builds/{buildID}", sdkHandler.StartTemplateBuild).Methods(http.MethodPost)
	authed.HandleFunc("/sdk/templates/{id}/builds/{buildID}/status", sdkHandler.GetTemplateBuildStatus).Methods(http.MethodGet)
	authed.HandleFunc("/sdk/templates/{id}/builds/{buildID}/logs", sdkHandler.GetTemplateBuildLogs).Methods(http.MethodGet)

	return r
}
