// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package httpapi exposes CLM's internal HTTP surface used by the
// CubeProxy /_sidecar_resume internal location. Routes:
//
//	POST /internal/resume?sandbox_id=...&request_id=...
//	GET  /healthz
//	GET  /readyz
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/resumer"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/webhook"
)

// FleetSizer is an optional dependency: when set, /readyz reports the
// current number of live CubeProxy replicas. Kept as an interface so the
// package doesn't pull in the discovery package.
type FleetSizer interface {
	Snapshot() int
}

// Server wires resume/healthz handlers and runs a *http.Server.
type Server struct {
	addr       string
	resumer    *resumer.Resumer
	registry   *registry.Registry
	fleet      FleetSizer
	webhook    *webhook.Manager
	adminToken string
	log        *zap.Logger
	srv        *http.Server
}

func New(addr string, r *resumer.Resumer, reg *registry.Registry, log *zap.Logger) *Server {
	return &Server{addr: addr, resumer: r, registry: reg, log: log}
}

// WithFleetSizer sets the optional fleet-size probe used by /readyz. Chainable.
func (s *Server) WithFleetSizer(fs FleetSizer) *Server {
	s.fleet = fs
	return s
}

// WithWebhook exposes the webhook endpoint management API under /admin/webhooks.
// All /admin/webhooks* routes require X-Cube-Admin-Token (see WithAdminToken).
func (s *Server) WithWebhook(mgr *webhook.Manager) *Server {
	s.webhook = mgr
	return s
}

// WithAdminToken sets the shared secret required by the /admin/webhooks*
// management API. When empty, the management API is disabled (403).
func (s *Server) WithAdminToken(token string) *Server {
	s.adminToken = token
	return s
}

// Run blocks until ctx is cancelled or ListenAndServe returns.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/resume", s.handleResume)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)

	// Webhook endpoint management. Go 1.22+ method patterns give automatic
	// 405s and {id} path params; every route is token-gated in the handler.
	mux.HandleFunc("GET /admin/webhooks", s.handleListWebhooks)
	mux.HandleFunc("POST /admin/webhooks", s.handleAddWebhook)
	mux.HandleFunc("PUT /admin/webhooks/{id}", s.handleUpdateWebhook)
	mux.HandleFunc("DELETE /admin/webhooks/{id}", s.handleDeleteWebhook)
	mux.HandleFunc("GET /admin/webhooks/stats", s.handleWebhookStats)

	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Resume RPCs to CubeMaster can be slow; make sure the sub-request
		// from CubeProxy isn't cut off by us.
		WriteTimeout: 35 * time.Second,
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("cube-lifecycle-manager http server listening", zap.String("addr", s.addr))
		errCh <- s.srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleResume(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sid := req.URL.Query().Get("sandbox_id")
	if sid == "" {
		http.Error(w, "sandbox_id query param is required", http.StatusBadRequest)
		return
	}
	rid := req.URL.Query().Get("request_id")

	// CubeProxy's proxy_read_timeout is 30s. Cap our own work at 25s so we
	// always have time to flush a 5xx response before nginx gives up on us
	// — otherwise nginx returns "504 upstream timed out" with no body and
	// we lose the ability to report what actually went wrong.
	ctx, cancel := context.WithTimeout(req.Context(), 25*time.Second)
	defer cancel()

	start := time.Now()
	s.log.Info("resume request received",
		zap.String("sandbox_id", sid),
		zap.String("request_id", rid))

	if err := s.resumer.Resume(ctx, sid); err != nil {
		s.log.Warn("resume failed",
			zap.String("sandbox_id", sid),
			zap.String("request_id", rid),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.log.Info("resume request completed",
		zap.String("sandbox_id", sid),
		zap.String("request_id", rid),
		zap.Duration("elapsed", time.Since(start)))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"sandbox_id": sid,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"ok":           true,
		"registry_len": s.registry.Len(),
	}
	if s.fleet != nil {
		resp["fleet_size"] = s.fleet.Snapshot()
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// authorizeAdmin gates the /admin/webhooks management API. It refuses any
// request when no token is configured (the management surface is opt-in), and
// rejects requests whose X-Cube-Admin-Token does not match.
func (s *Server) authorizeAdmin(w http.ResponseWriter, req *http.Request) bool {
	if s.adminToken == "" {
		http.Error(w, "webhook admin API disabled: CUBE_LCM_ADMIN_TOKEN not configured", http.StatusForbidden)
		return false
	}
	if req.Header.Get("X-Cube-Admin-Token") != s.adminToken {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, req *http.Request) {
	if !s.authorizeAdmin(w, req) {
		return
	}
	if s.webhook == nil {
		// Not enabled: treat as an empty list rather than an error so a
		// management client can render "no webhooks configured".
		writeJSON(w, http.StatusOK, []webhook.Endpoint{})
		return
	}
	writeJSON(w, http.StatusOK, s.webhook.Endpoints())
}

func (s *Server) handleAddWebhook(w http.ResponseWriter, req *http.Request) {
	if !s.authorizeAdmin(w, req) {
		return
	}
	if s.webhook == nil {
		http.Error(w, "webhook delivery not enabled", http.StatusNotFound)
		return
	}
	var ep webhook.Endpoint
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&ep); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}
	created, err := s.webhook.AddEndpoint(ep)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, req *http.Request) {
	if !s.authorizeAdmin(w, req) {
		return
	}
	if s.webhook == nil {
		http.Error(w, "webhook delivery not enabled", http.StatusNotFound)
		return
	}
	var ep webhook.Endpoint
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&ep); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.webhook.UpdateEndpoint(req.PathValue("id"), ep); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, req *http.Request) {
	if !s.authorizeAdmin(w, req) {
		return
	}
	if s.webhook == nil {
		http.Error(w, "webhook delivery not enabled", http.StatusNotFound)
		return
	}
	if err := s.webhook.DeleteEndpoint(req.PathValue("id")); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWebhookStats(w http.ResponseWriter, req *http.Request) {
	if !s.authorizeAdmin(w, req) {
		return
	}
	if s.webhook == nil {
		http.Error(w, "webhook delivery not enabled", http.StatusNotFound)
		return
	}
	dropped, delivered, failed := s.webhook.Stats()
	writeJSON(w, http.StatusOK, map[string]uint64{
		"dropped":   dropped,
		"delivered": delivered,
		"failed":    failed,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
