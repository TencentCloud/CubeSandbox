// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	networkruntime "github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

const (
	egressPolicyDumpPath = "/v1/policies/dump"
	networkTapsPath      = "/v1/network/taps"
)

// RegisterHTTP exposes the network endpoints served by Cubelet itself.
//
// /v1/policies/dump is used by CubeEgress bootstrap. /v1/network/taps is a
// read-only operator endpoint for inspecting the embedded network runtime.
// Both handlers reject non-loopback clients because Cubelet HTTP may bind all
// host interfaces.
func (l *local) RegisterHTTP(handlers map[string]http.Handler) error {
	if handlers == nil {
		return fmt.Errorf("http handlers map is nil")
	}
	for path, handler := range map[string]http.Handler{
		egressPolicyDumpPath: http.HandlerFunc(l.handleDumpEgressPolicies),
		networkTapsPath:      http.HandlerFunc(l.handleListNetworkTaps),
	} {
		if handlers[path] != nil {
			return fmt.Errorf("duplicate http handler for %s", path)
		}
		handlers[path] = handler
	}
	return nil
}

func (m *delegateNetworkManager) RegisterHTTP(handlers map[string]http.Handler) error {
	if m == nil || m.tapPlugin == nil {
		return fmt.Errorf("network tap plugin is nil")
	}
	return m.tapPlugin.RegisterHTTP(handlers)
}

func (l *local) handleListNetworkTaps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Read-only diagnostics still expose sandbox IDs, IPs, port mappings, and
	// cleanup errors. Cubelet's HTTP listener may bind all host interfaces, so
	// keep the same loopback trust boundary as /v1/policies/dump.
	if !isLoopbackHTTPClient(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	resp, err := l.networkRuntime.ListTaps(r.Context(), &networkruntime.ListTapsRequest{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.G(r.Context()).Warnf("encode network taps failed: %v", err)
	}
}

func (l *local) handleDumpEgressPolicies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The dump contains header-injection secrets and exists only for the colocated
	// CubeEgress bootstrap client. Cubelet's normal HTTP listener may bind all host
	// interfaces, so enforce the loopback trust boundary at the handler.
	if !isLoopbackHTTPClient(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	policies, err := l.networkRuntime.DumpEgressPolicies(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if policies == nil {
		policies = map[string]map[string]any{}
	}
	resp := struct {
		Policies map[string]map[string]any `json:"policies"`
	}{Policies: policies}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.G(r.Context()).Warnf("encode egress policies dump failed: %v", err)
	}
}

// isLoopbackHTTPClient reports whether the request came from a loopback address.
// RemoteAddr is expected in host:port form from net/http.
func isLoopbackHTTPClient(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
