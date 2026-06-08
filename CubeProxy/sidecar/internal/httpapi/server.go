// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package httpapi exposes the sidecar's internal HTTP surface used by the
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
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/registry"
	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/resumer"
)

// Server wires resume/healthz handlers and runs a *http.Server.
type Server struct {
	addr     string
	resumer  *resumer.Resumer
	registry *registry.Registry
	log      *zap.Logger
	srv      *http.Server
}

func New(addr string, r *resumer.Resumer, reg *registry.Registry, log *zap.Logger) *Server {
	return &Server{addr: addr, resumer: r, registry: reg, log: log}
}

// Run blocks until ctx is cancelled or ListenAndServe returns.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/resume", s.handleResume)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)

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
		s.log.Info("sidecar http server listening", zap.String("addr", s.addr))
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"registry_len": s.registry.Len(),
	})
}
