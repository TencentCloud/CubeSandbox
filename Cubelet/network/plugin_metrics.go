// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

const maxConcurrentNetworkMetricScrapes = 2

const networkMetricsPath = "/v1/metrics/network"

var (
	readCubeVSSandboxTrafficMetrics = cubevs.ReadSandboxTrafficMetrics
	readCubeVSSNATAllocationMetrics = cubevs.ReadSNATAllocationMetrics
)

var (
	networkExporterUp = prometheus.NewDesc(
		"cube_cubebox_network_exporter_up",
		"Whether the CubeVS network metrics exporter could read the pinned eBPF maps during the last scrape.",
		nil, nil,
	)

	sandboxIngressPackets = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_ingress_packets_total",
		"Total packets delivered to the sandbox datapath.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxIngressBytes = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_ingress_bytes_total",
		"Total bytes delivered to the sandbox datapath.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxIngressDropPackets = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_ingress_drop_packets_total",
		"Total ingress packets dropped before reaching the sandbox.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxIngressDropBytes = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_ingress_drop_bytes_total",
		"Total ingress bytes dropped before reaching the sandbox.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxEgressPackets = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_egress_packets_total",
		"Total packets emitted from the sandbox datapath.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxEgressBytes = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_egress_bytes_total",
		"Total bytes emitted from the sandbox datapath.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxEgressDropPackets = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_egress_drop_packets_total",
		"Total sandbox egress packets dropped by the datapath.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxEgressDropBytes = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_egress_drop_bytes_total",
		"Total sandbox egress bytes dropped by the datapath.",
		[]string{"sandbox_ip"}, nil,
	)
	sandboxSNATAllocFailures = prometheus.NewDesc(
		"cube_cubebox_network_sandbox_snat_alloc_failures_total",
		"Total SNAT allocation failures observed for the sandbox.",
		[]string{"sandbox_ip"}, nil,
	)

	snatSessionsInUse = prometheus.NewDesc(
		"cube_cubebox_network_snat_sessions_in_use",
		"Current number of live SNAT sessions using this SNAT IP.",
		[]string{"snat_ip"}, nil,
	)
	snatPortsCapacity = prometheus.NewDesc(
		"cube_cubebox_network_snat_ports_capacity",
		"Source-port range size available per destination and protocol on this SNAT IP.",
		[]string{"snat_ip"}, nil,
	)
	snatPortsUnused = prometheus.NewDesc(
		"cube_cubebox_network_snat_ports_unused",
		"Current source-port numbers not used by any live session on this SNAT IP.",
		[]string{"snat_ip"}, nil,
	)
)

type cubeVSNetworkCollector struct {
	readSandboxMetrics func() ([]cubevs.SandboxTrafficMetrics, error)
	readSNATMetrics    func() ([]cubevs.SNATAllocationMetrics, error)
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func newNetworkMetricsHandler() http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(&cubeVSNetworkCollector{
		readSandboxMetrics: readCubeVSSandboxTrafficMetrics,
		readSNATMetrics:    readCubeVSSNATAllocationMetrics,
	})
	base := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		MaxRequestsInFlight: maxConcurrentNetworkMetricScrapes,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusCapturingResponseWriter{ResponseWriter: w}
		base.ServeHTTP(rw, r)
		status := rw.statusCode
		if status == 0 {
			status = http.StatusOK
		}
		if status >= http.StatusBadRequest {
			log.G(r.Context()).Errorf("network metrics scrape finish: method=%s path=%s remote=%s status=%d duration=%s", r.Method, r.URL.Path, r.RemoteAddr, status, time.Since(start))
			return
		}
	})
}

func (w *statusCapturingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusCapturingResponseWriter) Write(body []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (c *cubeVSNetworkCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		networkExporterUp,
		sandboxIngressPackets,
		sandboxIngressBytes,
		sandboxIngressDropPackets,
		sandboxIngressDropBytes,
		sandboxEgressPackets,
		sandboxEgressBytes,
		sandboxEgressDropPackets,
		sandboxEgressDropBytes,
		sandboxSNATAllocFailures,
		snatSessionsInUse,
		snatPortsCapacity,
		snatPortsUnused,
	} {
		ch <- desc
	}
}

func (c *cubeVSNetworkCollector) Collect(ch chan<- prometheus.Metric) {
	scrapeID := time.Now().UnixNano()
	if c == nil || c.readSandboxMetrics == nil || c.readSNATMetrics == nil {
		log.L.Errorf("network metrics collector unavailable: scrape_id=%d collector_nil=%t sandbox_reader_nil=%t snat_reader_nil=%t",
			scrapeID, c == nil, c == nil || c.readSandboxMetrics == nil, c == nil || c.readSNATMetrics == nil)
		ch <- prometheus.MustNewConstMetric(networkExporterUp, prometheus.GaugeValue, 0)
		return
	}

	sandboxes, err := c.readSandboxMetrics()
	if err != nil {
		log.L.Errorf("network metrics collector sandbox read failed: scrape_id=%d err=%v", scrapeID, err)
		ch <- prometheus.MustNewConstMetric(networkExporterUp, prometheus.GaugeValue, 0)
		return
	}

	snats, err := c.readSNATMetrics()
	if err != nil {
		log.L.Errorf("network metrics collector SNAT read failed: scrape_id=%d err=%v", scrapeID, err)
		ch <- prometheus.MustNewConstMetric(networkExporterUp, prometheus.GaugeValue, 0)
		return
	}

	ch <- prometheus.MustNewConstMetric(networkExporterUp, prometheus.GaugeValue, 1)

	for _, sandbox := range sandboxes {
		labels := []string{sandbox.SandboxIP}
		ch <- prometheus.MustNewConstMetric(sandboxIngressPackets, prometheus.CounterValue, float64(sandbox.IngressPackets), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxIngressBytes, prometheus.CounterValue, float64(sandbox.IngressBytes), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxIngressDropPackets, prometheus.CounterValue, float64(sandbox.IngressDropPackets), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxIngressDropBytes, prometheus.CounterValue, float64(sandbox.IngressDropBytes), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxEgressPackets, prometheus.CounterValue, float64(sandbox.EgressPackets), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxEgressBytes, prometheus.CounterValue, float64(sandbox.EgressBytes), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxEgressDropPackets, prometheus.CounterValue, float64(sandbox.EgressDropPackets), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxEgressDropBytes, prometheus.CounterValue, float64(sandbox.EgressDropBytes), labels...)
		ch <- prometheus.MustNewConstMetric(sandboxSNATAllocFailures, prometheus.CounterValue, float64(sandbox.SNATAllocFailures), labels...)
	}

	seenSNATIPs := make(map[string]struct{}, len(snats))
	for _, snat := range snats {
		if _, ok := seenSNATIPs[snat.SNATIP]; ok {
			log.L.Warnf("network metrics collector skipped duplicate SNAT IP: scrape_id=%d snat_ip=%s", scrapeID, snat.SNATIP)
			continue
		}
		seenSNATIPs[snat.SNATIP] = struct{}{}

		labels := []string{snat.SNATIP}
		ch <- prometheus.MustNewConstMetric(snatSessionsInUse, prometheus.GaugeValue, float64(snat.SessionsInUse), labels...)
		ch <- prometheus.MustNewConstMetric(snatPortsCapacity, prometheus.GaugeValue, float64(snat.PortsCapacity), labels...)
		ch <- prometheus.MustNewConstMetric(snatPortsUnused, prometheus.GaugeValue, float64(snat.PortsUnused), labels...)
	}
}
