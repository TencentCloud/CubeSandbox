// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
)

func TestDelegateNetworkManagerRegisterHTTPExposesNetworkMetrics(t *testing.T) {
	m := &delegateNetworkManager{tapPlugin: &local{}}
	handlers := map[string]http.Handler{}

	if err := m.RegisterHTTP(handlers); err != nil {
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
		`cubesandbox_network_exporter_up{source="sandbox"} 1`,
		`cubesandbox_network_exporter_up{source="snat"} 1`,
		`cubesandbox_network_sandbox_ingress_packets_total{sandbox_ip="192.168.0.10"} 11`,
		`cubesandbox_network_sandbox_egress_bytes_total{sandbox_ip="192.168.0.10"} 2200`,
		`cubesandbox_network_sandbox_snat_alloc_failures_total{sandbox_ip="192.168.0.10"} 4`,
		`cubesandbox_network_snat_sessions_in_use{snat_ip="203.0.113.2"} 17`,
		`cubesandbox_network_snat_ports_capacity{snat_ip="203.0.113.2"} 35536`,
		`cubesandbox_network_snat_ports_unused{snat_ip="203.0.113.2"} 35531`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\nfull body:\n%s", want, body)
		}
	}
}

func TestNetworkMetricsHandlerHandlesSourceFailuresIndependently(t *testing.T) {
	originalSandboxRead := readCubeVSSandboxTrafficMetrics
	originalSNATRead := readCubeVSSNATAllocationMetrics
	t.Cleanup(func() {
		readCubeVSSandboxTrafficMetrics = originalSandboxRead
		readCubeVSSNATAllocationMetrics = originalSNATRead
	})

	sandboxMetrics := []cubevs.SandboxTrafficMetrics{{
		SandboxIP:      "192.168.0.10",
		IngressPackets: 11,
	}}
	snatMetrics := []cubevs.SNATAllocationMetrics{{
		SNATIP:        "203.0.113.2",
		SessionsInUse: 17,
	}}
	sandboxSeries := `cubesandbox_network_sandbox_ingress_packets_total{sandbox_ip="192.168.0.10"} 11`
	snatSeries := `cubesandbox_network_snat_sessions_in_use{snat_ip="203.0.113.2"} 17`

	tests := []struct {
		name       string
		sandboxErr error
		snatErr    error
		want       []string
		notWant    []string
	}{
		{
			name:    "SNAT failure preserves sandbox metrics",
			snatErr: errors.New("SNAT read failed"),
			want: []string{
				`cubesandbox_network_exporter_up{source="sandbox"} 1`,
				`cubesandbox_network_exporter_up{source="snat"} 0`,
				sandboxSeries,
			},
			notWant: []string{snatSeries},
		},
		{
			name:       "sandbox failure preserves SNAT metrics",
			sandboxErr: errors.New("sandbox read failed"),
			want: []string{
				`cubesandbox_network_exporter_up{source="sandbox"} 0`,
				`cubesandbox_network_exporter_up{source="snat"} 1`,
				snatSeries,
			},
			notWant: []string{sandboxSeries},
		},
		{
			name:       "both failures report both sources down",
			sandboxErr: errors.New("sandbox read failed"),
			snatErr:    errors.New("SNAT read failed"),
			want: []string{
				`cubesandbox_network_exporter_up{source="sandbox"} 0`,
				`cubesandbox_network_exporter_up{source="snat"} 0`,
			},
			notWant: []string{sandboxSeries, snatSeries},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readCubeVSSandboxTrafficMetrics = func() ([]cubevs.SandboxTrafficMetrics, error) {
				return sandboxMetrics, tt.sandboxErr
			}
			readCubeVSSNATAllocationMetrics = func() ([]cubevs.SNATAllocationMetrics, error) {
				return snatMetrics, tt.snatErr
			}

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, networkMetricsPath, nil)
			newNetworkMetricsHandler().ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Errorf("metrics output missing %q\nfull body:\n%s", want, body)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(body, notWant) {
					t.Errorf("metrics output unexpectedly contains %q\nfull body:\n%s", notWant, body)
				}
			}
		})
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

	const metric = `cubesandbox_network_snat_sessions_in_use{snat_ip="10.12.208.116"}`
	if count := strings.Count(recorder.Body.String(), metric); count != 1 {
		t.Fatalf("metric count=%d, want 1\nfull body:\n%s", count, recorder.Body.String())
	}
}
