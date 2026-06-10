// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
)

// Server is a minimal HTTP server that exposes health probes.
type Server struct {
	httpServer     *http.Server
	listener       net.Listener
	unixSocketPath string
	service        service.Service
}

// New creates a probe server bound to the given listen address.
func New(listen string, svc service.Service) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/v1/network/ensure", func(w http.ResponseWriter, r *http.Request) {
		handleJSON(w, r, func(body []byte) (interface{}, error) {
			req := &service.EnsureNetworkRequest{}
			if err := json.Unmarshal(body, req); err != nil {
				return nil, err
			}
			return svc.EnsureNetwork(r.Context(), req)
		})
	})
	mux.HandleFunc("/v1/network/release", func(w http.ResponseWriter, r *http.Request) {
		handleJSON(w, r, func(body []byte) (interface{}, error) {
			req := &service.ReleaseNetworkRequest{}
			if err := json.Unmarshal(body, req); err != nil {
				return nil, err
			}
			return svc.ReleaseNetwork(r.Context(), req)
		})
	})
	mux.HandleFunc("/v1/network/reconcile", func(w http.ResponseWriter, r *http.Request) {
		handleJSON(w, r, func(body []byte) (interface{}, error) {
			req := &service.ReconcileNetworkRequest{}
			if err := json.Unmarshal(body, req); err != nil {
				return nil, err
			}
			return svc.ReconcileNetwork(r.Context(), req)
		})
	})
	mux.HandleFunc("/v1/network/get", func(w http.ResponseWriter, r *http.Request) {
		handleJSON(w, r, func(body []byte) (interface{}, error) {
			req := &service.GetNetworkRequest{}
			if err := json.Unmarshal(body, req); err != nil {
				return nil, err
			}
			return svc.GetNetwork(r.Context(), req)
		})
	})
	mux.HandleFunc("/v1/network/list", func(w http.ResponseWriter, r *http.Request) {
		handleJSON(w, r, func(body []byte) (interface{}, error) {
			req := &service.ListNetworksRequest{}
			if len(body) != 0 {
				if err := json.Unmarshal(body, req); err != nil {
					return nil, err
				}
			}
			return svc.ListNetworks(r.Context(), req)
		})
	})

	// CubeEgress bootstrap pull. CUBE_EGRESS_BOOTSTRAP_URL points here,
	// and lua/bootstrap.lua does a plain GET (no body, JSON response).
	// The endpoint is unconditionally exposed: even when no sandboxes
	// have rules, returning an empty `policies` map is the right answer
	// — bootstrap.lua treats that as "nothing to load" and proceeds.
	mux.HandleFunc("/v1/policies/dump", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		policies, err := svc.DumpEgressPolicies(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Match bootstrap.lua's expected shape: {"policies": {...}}.
		// We don't redact secrets here (unlike CubeEgress's own
		// /admin/v1/dump which does): the consumer is the colocated
		// CubeEgress that needs the inline secrets to function. The
		// loopback isolation that protects /admin/v1/dump from
		// untrusted callers protects this endpoint too — both bind
		// only to 127.0.0.1.
		resp := map[string]any{"policies": policies}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	return &Server{
		httpServer: &http.Server{
			Addr:         listen,
			Handler:      mux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
			IdleTimeout:  15 * time.Second,
		},
		service: svc,
	}
}

// NewEndpoint creates a server from endpoint, supporting unix:// and tcp://.
func NewEndpoint(endpoint string, svc service.Service) (*Server, error) {
	srv := New("", svc)
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return nil, fmt.Errorf("endpoint is empty")
	}

	switch {
	case strings.HasPrefix(ep, "unix://"):
		socketPath := strings.TrimPrefix(ep, "unix://")
		if socketPath == "" {
			return nil, fmt.Errorf("unix endpoint is invalid: %q", endpoint)
		}
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
			return nil, err
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			return nil, err
		}
		srv.listener = ln
		srv.unixSocketPath = socketPath
	case strings.HasPrefix(ep, "tcp://"):
		addr := strings.TrimPrefix(ep, "tcp://")
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		srv.listener = ln
	default:
		// Backward compatible: treat raw host:port as tcp address.
		ln, err := net.Listen("tcp", ep)
		if err != nil {
			return nil, err
		}
		srv.listener = ln
	}
	return srv, nil
}

// Start serves probes until the server is closed.
func (s *Server) Start() error {
	var err error
	if s.listener != nil {
		err = s.httpServer.Serve(s.listener)
	} else {
		err = s.httpServer.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop gracefully shuts down the probe server.
func (s *Server) Stop(ctx context.Context) error {
	err := s.httpServer.Shutdown(ctx)
	if s.unixSocketPath != "" {
		_ = os.Remove(s.unixSocketPath)
	}
	return err
}

func handleJSON(w http.ResponseWriter, r *http.Request, fn func(body []byte) (interface{}, error)) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	resp, err := fn(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
