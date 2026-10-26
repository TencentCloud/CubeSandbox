// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package images

import (
	"context"

	"github.com/docker/go-metrics"
	prom "github.com/prometheus/client_golang/prometheus"
)

var (
	imagePulls           metrics.LabeledCounter
	inProgressImagePulls metrics.Gauge
	imagePullThroughput  prom.Histogram
)

func initMetrics(ctx context.Context) {
	const (
		namespace = "cube"
		subsystem = "cubebox"
	)

	ns := metrics.NewNamespace(namespace, subsystem, nil)

	name := "image_host_pull"
	imagePulls = ns.NewLabeledCounter(name, "succeeded and failed counters", "status")

	name = "image_host_in_progress_pulls"
	inProgressImagePulls = ns.NewGauge(name, "in progress pulls", metrics.Total)

	name = "image_host_pulling_throughput_mib_per_sec"
	imagePullThroughput = prom.NewHistogram(
		prom.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      name,
			Help:      "image pull throughput",
			Buckets:   prom.DefBuckets,
		},
	)
	ns.Add(imagePullThroughput)
	metrics.Register(ns)
}
