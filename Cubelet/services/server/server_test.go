// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"net/http"
	"testing"
)

func TestIsCriticalCubeletPlugin(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "cubelet workflow plugin", id: "io.cubelet.workflow.v1.workflow", want: true},
		{name: "cubelet service plugin", id: "io.cubelet.cubebox-service.v1.gc-service", want: true},
		{name: "containerd builtin plugin", id: "io.containerd.grpc.v1.healthcheck", want: false},
		{name: "empty id", id: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCriticalCubeletPlugin(tc.id); got != tc.want {
				t.Fatalf("isCriticalCubeletPlugin(%q)=%v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestServeMetricsIncludesDedicatedResourceEndpoint(t *testing.T) {
	called := false
	resourceHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	s := &Server{resourceMetricServer: resourceHandler}
	handlers := s.serveMetrics()
	handler := handlers["/v1/metrics/resource"]
	if handler == nil {
		t.Fatal("resource metrics handler is not registered at /v1/metrics/resource")
	}
	handler.ServeHTTP(nil, nil)
	if !called {
		t.Fatal("resource metrics endpoint did not invoke the configured handler")
	}
}
