// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package agent is the ops-agent skeleton: it owns the HTTP server, the
// reconcile ticker and the health endpoint. It has no knowledge of any
// concrete task domain.
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"

	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/config"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/version"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/tasks"
)

// Agent wires domains into one process.
type Agent struct {
	cfg     *config.Config
	domains []tasks.TaskDomain
}

// New assembles the agent.
func New(cfg *config.Config, domains ...tasks.TaskDomain) *Agent {
	return &Agent{cfg: cfg, domains: domains}
}

// Run serves until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	status := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": version.Version})
	}
	mux.HandleFunc("GET /health", status)

	for _, d := range a.domains {
		for pattern, h := range d.Routes() {
			mux.HandleFunc(pattern, h)
		}
		go func(d tasks.TaskDomain) {
			ticker := time.NewTicker(a.cfg.ReconcileInterval)
			defer ticker.Stop()
			d.Reconcile(ctx) // reconcile once promptly at startup
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					d.Reconcile(ctx)
				}
			}
		}(d)
	}

	srv := &http.Server{
		Addr:              a.cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	cubelog.Infof("ops-agent listening on %s", a.cfg.ListenAddr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
