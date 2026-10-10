// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
)

func TestRegisterHTTPExposesNetworkMetrics(t *testing.T) {
	l := &local{}
	handlers := map[string]http.Handler{}

	if err := l.RegisterHTTP(handlers); err != nil {
		t.Fatalf("RegisterHTTP returned error: %v", err)
	}
	if handlers[networkMetricsPath] == nil {
		t.Fatalf("%s handler not registered", networkMetricsPath)
	}
}

func TestNetworkMetricsHandlerExportsPrometheusSnapshot(t *testing.T) {
	originalSandboxRead := readCubeVSSandboxTrafficMetrics
	originalSNATRead := readCubeVSSNATAllocationMetrics
	t.Cleanup(func() {
		readCubeVSSandboxTrafficMetrics = originalSandboxRead
		readCubeVSSNATAllocationMetrics = originalSNATRead
	})

	readCubeVSSandboxTrafficMetrics = func() ([]cubevs.SandboxTrafficMetrics, error) {
		return []cubevs.SandboxTrafficMetrics{
			{
				SandboxIP:          "192.168.0.10",
				IngressPackets:     11,
				IngressBytes:       1200,
				IngressDropPackets: 2,
				IngressDropBytes:   64,
				EgressPackets:      21,
				EgressBytes:        2200,
				EgressDropPackets:  3,
				EgressDropBytes:    96,
				SNATAllocFailures:  4,
			},
		}, nil
	}
	readCubeVSSNATAllocationMetrics = func() ([]cubevs.SNATAllocationMetrics, error) {
		return []cubevs.SNATAllocationMetrics{
			{
				SNATIP:        "203.0.113.2",
				SessionsInUse: 17,
				PortsCapacity: 35536,
				PortsUnused:   35531,
			},
		}, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, networkMetricsPath, nil)
	newNetworkMetricsHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	body := recorder.Body.String()
	for _, want := range []string{
		"cube_cubebox_network_exporter_up 1",
		`cube_cubebox_network_sandbox_ingress_packets_total{sandbox_ip="192.168.0.10"} 11`,
		`cube_cubebox_network_sandbox_egress_bytes_total{sandbox_ip="192.168.0.10"} 2200`,
		`cube_cubebox_network_sandbox_snat_alloc_failures_total{sandbox_ip="192.168.0.10"} 4`,
		`cube_cubebox_network_snat_sessions_in_use{snat_ip="203.0.113.2"} 17`,
		`cube_cubebox_network_snat_ports_capacity{snat_ip="203.0.113.2"} 35536`,
		`cube_cubebox_network_snat_ports_unused{snat_ip="203.0.113.2"} 35531`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\nfull body:\n%s", want, body)
		}
	}
}

func TestNetworkMetricsHandlerDeduplicatesSNATIP(t *testing.T) {
	originalSandboxRead := readCubeVSSandboxTrafficMetrics
	originalSNATRead := readCubeVSSNATAllocationMetrics
	t.Cleanup(func() {
		readCubeVSSandboxTrafficMetrics = originalSandboxRead
		readCubeVSSNATAllocationMetrics = originalSNATRead
	})

	readCubeVSSandboxTrafficMetrics = func() ([]cubevs.SandboxTrafficMetrics, error) {
		return nil, nil
	}
	readCubeVSSNATAllocationMetrics = func() ([]cubevs.SNATAllocationMetrics, error) {
		snat := cubevs.SNATAllocationMetrics{
			SNATIP:        "10.12.208.116",
			PortsCapacity: 35536,
			PortsUnused:   35536,
		}
		return []cubevs.SNATAllocationMetrics{snat, snat, snat, snat}, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, networkMetricsPath, nil)
	newNetworkMetricsHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	const metric = `cube_cubebox_network_snat_sessions_in_use{snat_ip="10.12.208.116"}`
	if count := strings.Count(recorder.Body.String(), metric); count != 1 {
		t.Fatalf("metric count=%d, want 1\nfull body:\n%s", count, recorder.Body.String())
	}
}
