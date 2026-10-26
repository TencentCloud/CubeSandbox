// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package chics

import (
	"github.com/docker/go-metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	imageForwards           metrics.LabeledCounter
	inProgressImageForwards metrics.Gauge
	imageForwardsCache      metrics.LabeledCounter

	imageForwardLatency prometheus.Histogram
)

func init() {
	const (
		namespace = "chi"
		subsystem = "cubebox"
	)

	ns := metrics.NewNamespace(namespace, subsystem, nil)

	imageForwards = ns.NewLabeledCounter("image_forwards", "succeeded and failed counters", "status")
	inProgressImageForwards = ns.NewGauge("in_progress_image_forwards", "in progress forwards", metrics.Total)

	imageForwardsCache = ns.NewLabeledCounter("image_forwards_with_cache", "host image cache counters", "cache")
	imageForwardLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "image_forward_latency_milliseconds",
			Help:      "image forwards latency in milliseconds",
			Buckets:   prometheus.ExponentialBuckets(1, 10, 8),
		},
	)
	ns.Add(imageForwardLatency)
	metrics.Register(ns)
}

const (
	instanceMark               = ""
	inProgressImageForwardMark = "in_progress_image_forwards"
	imageForwardMark           = "image_forwards"
	imageForwardSizeMark       = "image_forwards_size"
	imageForwardLatencyMark    = "image_forward_latency_milliseconds"
	imageForwardCacheMark      = "image_forwards_with_cache"

	statusKey     = "status"
	statusSuccess = "success"
	statusFail    = "fail"
	namespaceKey  = "namespace"
)

type hostImageForwardCollector struct {
}

func newHostImageForwardCollector() *hostImageForwardCollector {
	return &hostImageForwardCollector{}
}

func (m *hostImageForwardCollector) complete(status string, latency float64, size uint64) {
	if status == statusSuccess {
		imageForwardLatency.Observe(latency)
		imageForwards.WithValues(statusSuccess).Inc()
	} else {
		imageForwards.WithValues(statusFail).Inc()
	}
	inProgressImageForwards.Dec()
}

func (m *hostImageForwardCollector) add() {
	inProgressImageForwards.Inc()
}

func (m *hostImageForwardCollector) addCache(hit bool) {
	if hit {
		imageForwardsCache.WithValues("true").Inc()
	} else {
		imageForwardsCache.WithValues("false").Inc()
	}
}
