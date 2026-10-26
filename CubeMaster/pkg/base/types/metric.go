// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package types contains the types used by the metrics package
package types

import (
	"sync"
	"time"
)

type MetricInfo struct {
	mu   sync.Mutex
	stat []*Metric
}

func (m *MetricInfo) GetMetric() []*Metric {
	return m.stat
}

func (m *MetricInfo) AddMetric(err error, id string, t time.Duration) {
	m.mu.Lock()
	m.stat = append(m.stat, &Metric{id: id, err: err, duration: t})
	m.mu.Unlock()
}

type Metric struct {
	id       string
	err      error
	duration time.Duration
}

func (m Metric) ID() string {
	return m.id
}

func (m Metric) Error() error {
	return m.err
}

func (m Metric) Duration() time.Duration {
	return m.duration
}
